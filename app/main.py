from __future__ import annotations

import os
from typing import Iterable
import secrets

import asyncpg
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import HTMLResponse, RedirectResponse

from app.db import ensure_user, init_db
from app.settings import settings
from app.telegram import (
    extract_callback_query,
    extract_channel_from_update,
    extract_post_text,
    extract_private_message,
    tg_answer_callback_query,
    tg_send_message,
    tg_set_webhook,
)
from app.vk import VkError, vk_groups_get_admin, vk_wall_post
from app.vk_oauth import VkOAuthError, build_authorize_url, compute_expires_at, exchange_code_for_token, generate_pkce_pair


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


def _vk_redirect_uri() -> str:
    return f"{settings.app_base_url.rstrip('/')}{settings.vk_redirect_path}"


def _kb(button_rows: Iterable[list[tuple[str, str]]]) -> dict:
    return {
        "inline_keyboard": [
            [{"text": text, "callback_data": data} for (text, data) in row] for row in button_rows
        ]
    }


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
                        "Я связываю Telegram‑канал → VK‑группу и автопощу посты.\n\n"
                        "Команды:\n"
                        "/connect_vk — подключить VK и выбрать группу\n"
                        "/status — показать текущие настройки\n"
                        "/enable — включить автопостинг\n"
                        "/disable — выключить автопостинг\n\n"
                        "Важно: добавь меня админом в канал, который нужно слушать."
                    ),
                )
            elif text.startswith("/connect_vk"):
                if from_user and isinstance(from_user.get("id"), int):
                    tg_user_id = int(from_user["id"])
                    state = secrets.token_urlsafe(24)
                    verifier, challenge = generate_pkce_pair()

                    pool = await get_pool()
                    async with pool.acquire() as conn:
                        await ensure_user(conn, tg_user_id)
                        await conn.execute(
                            """
                            INSERT INTO oauth_states(state, tg_user_id)
                            VALUES ($1, $2)
                            ON CONFLICT (state) DO UPDATE SET tg_user_id=excluded.tg_user_id
                            """,
                            state,
                            tg_user_id,
                        )
                        await conn.execute(
                            """
                            INSERT INTO oauth_pkce(tg_user_id, code_verifier)
                            VALUES ($1, $2)
                            ON CONFLICT (tg_user_id) DO UPDATE SET code_verifier=excluded.code_verifier, created_at=now()
                            """,
                            tg_user_id,
                            verifier,
                        )
                        await conn.execute(
                            "UPDATE user_settings SET updated_at=now() WHERE tg_user_id=$1",
                            tg_user_id,
                        )

                    scope = "groups wall"
                    url = build_authorize_url(
                        client_id=settings.vk_client_id,
                        redirect_uri=_vk_redirect_uri(),
                        state=state,
                        code_challenge=challenge,
                        scope=scope,
                    )
                    await tg_send_message(
                        bot_token=settings.telegram_bot_token,
                        chat_id=chat_id,
                        text=f"Открой ссылку и разреши доступ VK:\n{url}\n\nПосле этого я покажу список твоих групп.",
                        disable_web_page_preview=True,
                    )
            elif text.startswith("/status") and from_user and isinstance(from_user.get("id"), int):
                tg_user_id = int(from_user["id"])
                pool = await get_pool()
                async with pool.acquire() as conn:
                    st = await conn.fetchrow(
                        "SELECT selected_vk_group_id, enabled FROM user_settings WHERE tg_user_id=$1",
                        tg_user_id,
                    )
                if st:
                    await tg_send_message(
                        bot_token=settings.telegram_bot_token,
                        chat_id=chat_id,
                        text=f"VK group_id: {st['selected_vk_group_id']}\nEnabled: {bool(st['enabled'])}",
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

    cq = extract_callback_query(update)
    if cq:
        cq_id = str(cq.get("id"))
        from_user = cq.get("from") if isinstance(cq.get("from"), dict) else None
        msg2 = cq.get("message") if isinstance(cq.get("message"), dict) else None
        chat2 = msg2.get("chat") if msg2 and isinstance(msg2.get("chat"), dict) else None
        chat_id = int(chat2["id"]) if chat2 and isinstance(chat2.get("id"), int) else None
        data = cq.get("data")
        if chat_id is None or not isinstance(data, str) or not from_user or not isinstance(from_user.get("id"), int):
            return {"ok": True}

        tg_user_id = int(from_user["id"])
        if data.startswith("vk_group:"):
            group_id = int(data.split(":", 1)[1])
            pool = await get_pool()
            async with pool.acquire() as conn:
                await ensure_user(conn, tg_user_id)
                await conn.execute(
                    "UPDATE user_settings SET selected_vk_group_id=$1, updated_at=now() WHERE tg_user_id=$2",
                    group_id,
                    tg_user_id,
                )

            await tg_answer_callback_query(
                bot_token=settings.telegram_bot_token, callback_query_id=cq_id, text="Группа выбрана"
            )
            await tg_send_message(
                bot_token=settings.telegram_bot_token,
                chat_id=chat_id,
                text=f"Ок, выбрал VK‑группу {group_id}. Теперь добавь меня админом в Telegram‑канал и включи /enable.",
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
async def vk_callback(code: str | None = None, state: str | None = None, error: str | None = None):
    if error:
        return HTMLResponse(f"VK auth error: {error}", status_code=400)
    if not code or not state:
        return HTMLResponse("Missing code/state", status_code=400)

    pool = await get_pool()
    async with pool.acquire() as conn:
        row = await conn.fetchrow("SELECT tg_user_id FROM oauth_states WHERE state=$1", state)
        if not row:
            return HTMLResponse("Unknown state", status_code=400)
        tg_user_id = int(row["tg_user_id"])
        pk = await conn.fetchrow("SELECT code_verifier FROM oauth_pkce WHERE tg_user_id=$1", tg_user_id)
        if not pk:
            return HTMLResponse("Missing PKCE verifier", status_code=400)
        verifier = str(pk["code_verifier"])

    try:
        token_payload = await exchange_code_for_token(
            client_id=settings.vk_client_id,
            client_secret=settings.vk_client_secret,
            code=code,
            redirect_uri=_vk_redirect_uri(),
            code_verifier=verifier,
        )
    except (VkOAuthError, Exception) as e:
        return HTMLResponse(f"Token exchange failed: {e}", status_code=400)

    access_token = str(token_payload["access_token"])
    vk_user_id = token_payload.get("user_id")
    expires_at = compute_expires_at(token_payload.get("expires_in"))

    async with pool.acquire() as conn:
        await ensure_user(conn, tg_user_id)
        await conn.execute(
            """
            INSERT INTO vk_accounts(tg_user_id, vk_user_id, access_token, expires_at)
            VALUES ($1, $2, $3, $4)
            ON CONFLICT (tg_user_id) DO UPDATE SET vk_user_id=excluded.vk_user_id,
                                                 access_token=excluded.access_token,
                                                 expires_at=excluded.expires_at
            """,
            tg_user_id,
            vk_user_id,
            access_token,
            expires_at,
        )
        await conn.execute("DELETE FROM oauth_states WHERE state=$1", state)

    # Fetch admin groups and send choices to user in Telegram
    try:
        groups = await vk_groups_get_admin(token=access_token, api_version=settings.vk_api_version)
    except VkError:
        groups = []

    if not groups:
        # Still notify user
        await tg_send_message(
            bot_token=settings.telegram_bot_token,
            chat_id=tg_user_id,
            text="VK подключен, но я не смог получить список админских групп (нет прав groups или VK не отдал список).",
        )
        return RedirectResponse("https://t.me/", status_code=302)

    # Build inline keyboard with up to 10 groups per page (minimal: first 10)
    buttons: list[list[tuple[str, str]]] = []
    for g in groups[:10]:
        gid = g.get("id")
        name = g.get("name") or f"group {gid}"
        if isinstance(gid, int):
            buttons.append([(str(name)[:40], f"vk_group:{gid}")])

    await tg_send_message(
        bot_token=settings.telegram_bot_token,
        chat_id=tg_user_id,
        text="Выбери VK‑группу для автопостинга:",
        reply_markup=_kb(buttons),
    )

    return HTMLResponse("VK connected. You can return to Telegram.")


@app.get("/health")
async def health():
    return {"ok": True}
