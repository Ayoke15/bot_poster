from __future__ import annotations

import os

import asyncpg
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import HTMLResponse

from app.db import ensure_user, init_db
from app.settings import settings
from app.telegram import (
    extract_channel_from_update,
    extract_post_text,
    extract_private_message,
    tg_send_message,
    tg_set_webhook,
)
from app.vk import VkError, vk_verify_community_token, vk_wall_post


DATABASE_URL = os.environ.get("DATABASE_URL")
if not DATABASE_URL:
    raise RuntimeError("DATABASE_URL env var is required (Postgres)")

app = FastAPI(title="TG → VK Autoposter")


async def get_pool() -> asyncpg.Pool:
    pool = getattr(app.state, "pool", None)
    if pool is None:
        raise RuntimeError("DB pool not initialized")
    return pool


@app.on_event("startup")
async def _startup() -> None:
    app.state.pool = await asyncpg.create_pool(DATABASE_URL, min_size=1, max_size=10)
    pool = await get_pool()
    async with pool.acquire() as conn:
        await init_db(conn)

    # Best-effort webhook setup on boot.
    try:
        await tg_set_webhook(
            bot_token=settings.telegram_bot_token,
            base_url=settings.app_base_url,
            secret_path=settings.telegram_webhook_secret,
        )
    except Exception:
        pass


@app.on_event("shutdown")
async def _shutdown() -> None:
    pool = getattr(app.state, "pool", None)
    if pool is not None:
        await pool.close()


def _mask_token(token: str) -> str:
    t = token.strip()
    if len(t) <= 14:
        return "***"
    return f"{t[:8]}…{t[-4:]}"


@app.get("/", response_class=HTMLResponse)
async def root():
    return HTMLResponse("OK")


@app.post("/telegram/webhook/{secret}")
async def telegram_webhook(secret: str, request: Request):
    if secret != settings.telegram_webhook_secret:
        raise HTTPException(status_code=404, detail="Not found")

    update = await request.json()
    if not isinstance(update, dict):
        return {"ok": True}

    # Private chat commands / callbacks
    msg = extract_private_message(update)
    if msg:
        from_user = msg.get("from") if isinstance(msg.get("from"), dict) else None
        if from_user and isinstance(from_user.get("id"), int):
            tg_user_id = int(from_user["id"])
            pool = await get_pool()
            async with pool.acquire() as conn:
                await ensure_user(conn, tg_user_id)

        text = msg.get("text") if isinstance(msg.get("text"), str) else ""
        chat = msg.get("chat") if isinstance(msg.get("chat"), dict) else None
        chat_id = int(chat["id"]) if chat and isinstance(chat.get("id"), int) else None
        if chat_id is not None:
            if text.startswith("/start"):
                await tg_send_message(
                    bot_token=settings.telegram_bot_token,
                    chat_id=chat_id,
                    text=(
                        "Я читаю посты из Telegram‑канала и публикую текст на стену VK‑сообщества.\n\n"
                        "1) Возьми ключ доступа сообщества: VK → сообщество → Управление → "
                        "Работа с API → Ключи доступа (нужны права на стену).\n"
                        "2) Одной строкой (в личку мне):\n"
                        "/set_vk <id_группы> <токен>\n"
                        "Пример: /set_vk 123456789 vk1.a.XXXX…\n\n"
                        "Команды:\n"
                        "/set_vk — сохранить группу и токен\n"
                        "/status — что сохранено\n"
                        "/clear_vk — удалить сохранённый токен\n"
                        "/enable — включить автопостинг\n"
                        "/disable — выключить\n\n"
                        "Добавь меня админом в канал, откуда копировать посты."
                    ),
                )
            elif text.startswith("/set_vk") and from_user and isinstance(from_user.get("id"), int):
                tg_user_id = int(from_user["id"])
                rest = text[len("/set_vk") :].strip()
                parts = rest.split(None, 1)
                if len(parts) < 2:
                    await tg_send_message(
                        bot_token=settings.telegram_bot_token,
                        chat_id=chat_id,
                        text="Формат: /set_vk <id_группы> <токен>\nПример: /set_vk 123456789 vk1.a.…",
                    )
                    return {"ok": True}
                try:
                    group_id = int(parts[0].strip())
                except ValueError:
                    await tg_send_message(
                        bot_token=settings.telegram_bot_token,
                        chat_id=chat_id,
                        text="id_группы должен быть числом (без минуса), например 123456789.",
                    )
                    return {"ok": True}
                token = parts[1].strip()
                if not token:
                    await tg_send_message(
                        bot_token=settings.telegram_bot_token,
                        chat_id=chat_id,
                        text="Токен пустой. Формат: /set_vk <id_группы> <токен>",
                    )
                    return {"ok": True}
                try:
                    gname = await vk_verify_community_token(
                        token=token, api_version=settings.vk_api_version, group_id=group_id
                    )
                except VkError as e:
                    await tg_send_message(
                        bot_token=settings.telegram_bot_token,
                        chat_id=chat_id,
                        text=f"VK не принял токен/группу: {e}\nПроверь id группы и что ключ выдан именно этому сообществу.",
                    )
                    return {"ok": True}
                pool = await get_pool()
                async with pool.acquire() as conn:
                    await ensure_user(conn, tg_user_id)
                    await conn.execute(
                        """
                        INSERT INTO vk_accounts(tg_user_id, vk_user_id, access_token, expires_at)
                        VALUES ($1, NULL, $2, NULL)
                        ON CONFLICT (tg_user_id) DO UPDATE SET access_token=excluded.access_token,
                            vk_user_id=NULL, expires_at=NULL
                        """,
                        tg_user_id,
                        token,
                    )
                    await conn.execute(
                        """
                        UPDATE user_settings
                        SET selected_vk_group_id=$1, updated_at=now()
                        WHERE tg_user_id=$2
                        """,
                        group_id,
                        tg_user_id,
                    )
                label = f" «{gname}»" if gname else ""
                await tg_send_message(
                    bot_token=settings.telegram_bot_token,
                    chat_id=chat_id,
                    text=(
                        f"Ок: группа {group_id}{label}, токен сохранён ({_mask_token(token)}).\n"
                        "Добавь меня админом в канал и напиши /enable."
                    ),
                )
            elif text.startswith("/clear_vk") and from_user and isinstance(from_user.get("id"), int):
                tg_user_id = int(from_user["id"])
                pool = await get_pool()
                async with pool.acquire() as conn:
                    await conn.execute("DELETE FROM vk_accounts WHERE tg_user_id=$1", tg_user_id)
                    await conn.execute(
                        "UPDATE user_settings SET selected_vk_group_id=NULL, enabled=FALSE, updated_at=now() WHERE tg_user_id=$1",
                        tg_user_id,
                    )
                await tg_send_message(
                    bot_token=settings.telegram_bot_token,
                    chat_id=chat_id,
                    text="Токен и привязка к группе удалены. Автопостинг выключен.",
                )
            elif text.startswith("/status") and from_user and isinstance(from_user.get("id"), int):
                tg_user_id = int(from_user["id"])
                pool = await get_pool()
                async with pool.acquire() as conn:
                    st = await conn.fetchrow(
                        "SELECT selected_vk_group_id, enabled FROM user_settings WHERE tg_user_id=$1",
                        tg_user_id,
                    )
                    acc = await conn.fetchrow(
                        "SELECT access_token FROM vk_accounts WHERE tg_user_id=$1", tg_user_id
                    )
                if st:
                    tok = _mask_token(str(acc["access_token"])) if acc and acc.get("access_token") else "(нет)"
                    await tg_send_message(
                        bot_token=settings.telegram_bot_token,
                        chat_id=chat_id,
                        text=(
                            f"VK group_id: {st['selected_vk_group_id']}\n"
                            f"Токен: {tok}\n"
                            f"Автопостинг: {'вкл' if st['enabled'] else 'выкл'}"
                        ),
                    )
            elif text.startswith("/enable") and from_user and isinstance(from_user.get("id"), int):
                tg_user_id = int(from_user["id"])
                pool = await get_pool()
                async with pool.acquire() as conn:
                    await conn.execute(
                        "UPDATE user_settings SET enabled=TRUE, updated_at=now() WHERE tg_user_id=$1",
                        tg_user_id,
                    )
                await tg_send_message(
                    bot_token=settings.telegram_bot_token, chat_id=chat_id, text="Автопостинг включен."
                )
            elif text.startswith("/disable") and from_user and isinstance(from_user.get("id"), int):
                tg_user_id = int(from_user["id"])
                pool = await get_pool()
                async with pool.acquire() as conn:
                    await conn.execute(
                        "UPDATE user_settings SET enabled=FALSE, updated_at=now() WHERE tg_user_id=$1",
                        tg_user_id,
                    )
                await tg_send_message(
                    bot_token=settings.telegram_bot_token, chat_id=chat_id, text="Автопостинг выключен."
                )

        return {"ok": True}

    channel = extract_channel_from_update(update)
    if channel:
        tg_chat_id = int(channel["id"])
        title = channel.get("title")
        username = channel.get("username")
        pool = await get_pool()
        async with pool.acquire() as conn:
            await conn.execute(
                """
                INSERT INTO telegram_channels(tg_chat_id, title, username)
                VALUES ($1, $2, $3)
                ON CONFLICT (tg_chat_id) DO UPDATE SET title=excluded.title, username=excluded.username
                """,
                tg_chat_id,
                title,
                username,
            )

    text = extract_post_text(update)
    if not text:
        return {"ok": True}

    if not channel:
        return {"ok": True}

    tg_chat_id = int(channel["id"])
    pool = await get_pool()
    async with pool.acquire() as conn:
        # Naive ownership: if we haven't seen an owner yet, we bind to the first
        # tg_user_id we have in users table (minimal). In real SaaS you'd add an explicit /claim_channel.
        owner = await conn.fetchrow(
            "SELECT tg_user_id FROM channel_owners WHERE tg_chat_id=$1 ORDER BY created_at ASC LIMIT 1",
            tg_chat_id,
        )
        if not owner:
            first_user = await conn.fetchrow("SELECT tg_user_id FROM users ORDER BY created_at ASC LIMIT 1")
            if not first_user:
                return {"ok": True}
            await conn.execute(
                "INSERT INTO channel_owners(tg_chat_id, tg_user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING",
                tg_chat_id,
                int(first_user["tg_user_id"]),
            )
            owner = {"tg_user_id": int(first_user["tg_user_id"])}

        tg_user_id = int(owner["tg_user_id"])
        st = await conn.fetchrow(
            "SELECT selected_vk_group_id, enabled FROM user_settings WHERE tg_user_id=$1",
            tg_user_id,
        )
        if not st or not st["enabled"] or not st["selected_vk_group_id"]:
            return {"ok": True}

        acc = await conn.fetchrow("SELECT access_token FROM vk_accounts WHERE tg_user_id=$1", tg_user_id)
        if not acc:
            return {"ok": True}

        try:
            await vk_wall_post(
                token=str(acc["access_token"]),
                api_version=settings.vk_api_version,
                group_id=int(st["selected_vk_group_id"]),
                message=text,
            )
        except VkError:
            # Swallow to avoid retries storm
            return {"ok": True}

    return {"ok": True}


@app.get("/vk/callback")
async def vk_callback():
    return HTMLResponse(
        "OAuth VK отключён. В Telegram напиши боту: /set_vk &lt;id_группы&gt; &lt;токен сообщества&gt;",
        status_code=410,
    )


@app.get("/health")
async def health():
    return {"ok": True, "mode": "community_token"}
