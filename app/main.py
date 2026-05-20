from __future__ import annotations

import os
from urllib.parse import urlparse

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
    tg_set_my_commands,
    tg_set_webhook,
)
from app.vk import VkError, vk_verify_community_token, vk_wall_post
from app.vk_parse import parse_vk_group_id_from_text


DATABASE_URL = (os.environ.get("DATABASE_URL") or "").strip()
if not DATABASE_URL:
    raise RuntimeError("Нужна переменная окружения DATABASE_URL")
# Railway отдаёт postgres:// — asyncpg понимает postgresql://
if DATABASE_URL.startswith("postgres://"):
    DATABASE_URL = "postgresql://" + DATABASE_URL[len("postgres://") :]

app = FastAPI(title="TG → VK Autoposter")


async def get_pool() -> asyncpg.Pool:
    pool = getattr(app.state, "pool", None)
    if pool is None:
        raise RuntimeError("DB pool not initialized")
    return pool


@app.on_event("startup")
async def _startup() -> None:
    host = urlparse(DATABASE_URL).hostname or "?"
    try:
        app.state.pool = await asyncpg.create_pool(DATABASE_URL, min_size=1, max_size=10)
    except Exception as e:
        raise RuntimeError(
            f"Не подключился к Postgres. В DATABASE_URL хост: {host!r}. "
            f"На Railway возьми DATABASE_URL из сервиса PostgreSQL (не @db: из примера). Ошибка: {e}"
        ) from e
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
        await tg_set_my_commands(bot_token=settings.telegram_bot_token)
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


def _cmd_base(text: str) -> str:
    """Первое слово команды без @botname."""
    if not text:
        return ""
    return text.split(None, 1)[0].split("@", 1)[0]


# Тексты на кнопках (совпадение посимвольно с обработчиком)
BTN_CONNECT = "🔌 Как подключить"
BTN_FORMAT = "📝 Формат /set_vk"
BTN_STATUS = "📊 Статус"
BTN_ENABLE = "▶️ Включить"
BTN_DISABLE = "⏹ Выключить"
BTN_CLEAR = "🗑 Сбросить VK"
BTN_HOME = "🏠 Главная"
BTN_LINK_VK = "🔗 Подключить VK"

ALL_MENU_BUTTONS = frozenset(
    {
        BTN_LINK_VK,
        BTN_CONNECT,
        BTN_FORMAT,
        BTN_STATUS,
        BTN_ENABLE,
        BTN_DISABLE,
        BTN_CLEAR,
        BTN_HOME,
    }
)

VK_STEP_IDLE = 0
VK_STEP_LINK = 1
VK_STEP_TOKEN = 2

VK_MSG_ASK_LINK = (
    "Шаг 1 из 2. Пришли одной строкой ссылку на сообщество VK "
    "(скопируй из адресной строки браузера).\n\n"
    "Примеры:\n"
    "https://vk.com/club123456789\n"
    "https://vk.com/public123456789\n\n"
    "Если ссылка только с коротким именем без club/public — открой страницу сообщества "
    "и скопируй адрес, где в пути есть club… или public…."
)

VK_MSG_ASK_TOKEN = (
    "Шаг 2 из 2. Сообщество: id {gid}.\n\n"
    "Теперь отдельным сообщением пришли токен доступа сообщества одной строкой. "
    "Где взять ключ — кнопка «Как подключить», блок про API."
)


async def _clear_vk_connect_flow(conn: asyncpg.Connection, tg_user_id: int) -> None:
    await conn.execute(
        """
        UPDATE user_settings
        SET vk_connect_step=0, vk_connect_group_id=NULL, updated_at=now()
        WHERE tg_user_id=$1
        """,
        tg_user_id,
    )


async def _set_vk_connect_await_link(conn: asyncpg.Connection, tg_user_id: int) -> None:
    await conn.execute(
        """
        UPDATE user_settings
        SET vk_connect_step=$2, vk_connect_group_id=NULL, updated_at=now()
        WHERE tg_user_id=$1
        """,
        tg_user_id,
        VK_STEP_LINK,
    )


async def _set_vk_connect_await_token(conn: asyncpg.Connection, tg_user_id: int, group_id: int) -> None:
    await conn.execute(
        """
        UPDATE user_settings
        SET vk_connect_step=$2, vk_connect_group_id=$3, updated_at=now()
        WHERE tg_user_id=$1
        """,
        tg_user_id,
        VK_STEP_TOKEN,
        int(group_id),
    )


async def _persist_vk_token(*, tg_user_id: int, group_id: int, token: str) -> tuple[bool, str]:
    token = (token or "").strip()
    if len(token) < 12:
        return False, "Токен слишком короткий — пришли целиком ключ доступа сообщества одной строкой."
    try:
        gname = await vk_verify_community_token(
            token=token, api_version=settings.vk_api_version, group_id=group_id
        )
    except VkError as e:
        return (
            False,
            f"VK не принял токен или группу: {e}\nПроверь, что ключ выдан именно этому сообществу.",
        )
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
        await _clear_vk_connect_flow(conn, tg_user_id)
    label = f" «{gname}»" if gname else ""
    return (
        True,
        (
            f"Готово: сообщество {group_id}{label}, токен сохранён ({_mask_token(token)}).\n"
            "Добавь меня админом в канал и нажми «Включить»."
        ),
    )


def _main_menu_keyboard() -> dict:
    """Reply-клавиатура: панель кнопок над полем ввода (не под пузырём сообщения)."""
    return {
        "keyboard": [
            [{"text": BTN_LINK_VK}],
            [{"text": BTN_CONNECT}],
            [{"text": BTN_FORMAT}, {"text": BTN_STATUS}],
            [{"text": BTN_ENABLE}, {"text": BTN_DISABLE}],
            [{"text": BTN_CLEAR}, {"text": BTN_HOME}],
        ],
        "resize_keyboard": True,
        "one_time_keyboard": False,
    }


WELCOME_RU = (
    "✅ Бот на связи.\n\n"
    "Я переношу текст постов из Telegram‑канала на стену VK‑сообщества.\n\n"
    "Подключение VK: кнопка «Подключить VK» (сначала ссылка на сообщество, потом токен). "
    "Полная инструкция — «Как подключить»."
)


FORMAT_SET_VK_RU = (
    "Для опытных пользователей — всё одной строкой:\n\n"
    "/set_vk <числовой_id_группы> <токен>\n\n"
    "Пример:\n"
    "/set_vk 123456789 vk1.a.длинная_строка…\n\n"
    "Проще: кнопка «Подключить VK» — сначала ссылка vk.com/club…, потом токен вторым сообщением.\n"
    "Откуда взять токен — «Как подключить»."
)


CONNECT_GUIDE_RU = (
    "🔌 Как подключить бота и VK\n\n"
    "── Telegram ──\n"
    "1) Добавь меня в нужный канал администратором (без этого я не увижу посты канала).\n"
    "2) Нажми «Подключить VK»: первым сообщением пришли ссылку на сообщество "
    "(из адресной строки, например vk.com/club…), вторым — токен доступа сообщества.\n"
    "   Альтернатива: одной строкой /set_vk (кнопка «Формат /set_vk»).\n"
    "3) Нажми «Включить» — после этого начнётся автопостинг на стену VK.\n\n"
    "── VK: ключ сообщества ──\n"
    "Нужно быть администратором или создателем сообщества.\n\n"
    "Через сайт vk.com (с компьютера удобнее):\n"
    "1) Зайди на страницу своего сообщества.\n"
    "2) Слева открой «Управление» (или «Настройки» у сообщества).\n"
    "3) В меню слева найди «Работа с API», «API», «Для разработчиков» или похожий пункт "
    "(название зависит от типа сообщества и версии интерфейса).\n"
    "4) Открой раздел с ключами доступа («Ключи доступа», «Создать ключ»).\n"
    "5) Создай ключ и отметь права на публикацию на стене от имени сообщества (wall / «Стена»).\n"
    "6) Скопируй токен — часто показывается один раз; если не успела — создай новый ключ.\n\n"
    "Из ссылки vk.com/club123456789 или public… число в конце — это id группы (бот разберёт сам).\n\n"
    "Токен — секрет: не отправляй его в каналы и публичные чаты."
)


@app.get("/", response_class=HTMLResponse)
async def root():
    return HTMLResponse("OK")


async def _send_private(*, chat_id: int, text: str) -> None:
    await tg_send_message(
        bot_token=settings.telegram_bot_token,
        chat_id=chat_id,
        text=text,
        reply_markup=_main_menu_keyboard(),
    )


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
        text = msg.get("text") if isinstance(msg.get("text"), str) else ""
        chat = msg.get("chat") if isinstance(msg.get("chat"), dict) else None
        chat_id = int(chat["id"]) if chat and isinstance(chat.get("id"), int) else None
        from_user = msg.get("from") if isinstance(msg.get("from"), dict) else None

        if chat_id is None or not from_user or not isinstance(from_user.get("id"), int):
            return {"ok": True}

        tg_user_id = int(from_user["id"])
        raw = (text or "").strip()
        pool = await get_pool()
        async with pool.acquire() as conn:
            await ensure_user(conn, tg_user_id)
            st_row = await conn.fetchrow(
                "SELECT vk_connect_step, vk_connect_group_id FROM user_settings WHERE tg_user_id=$1",
                tg_user_id,
            )
        step = int(st_row["vk_connect_step"] or 0) if st_row else 0
        pending_gid = (
            int(st_row["vk_connect_group_id"])
            if st_row and st_row["vk_connect_group_id"] is not None
            else None
        )

        if raw == BTN_LINK_VK:
            async with pool.acquire() as conn:
                await _set_vk_connect_await_link(conn, tg_user_id)
            await _send_private(chat_id=chat_id, text=VK_MSG_ASK_LINK)
        elif raw == BTN_HOME:
            async with pool.acquire() as conn:
                await _clear_vk_connect_flow(conn, tg_user_id)
            await _send_private(chat_id=chat_id, text=WELCOME_RU)
        elif raw == BTN_CONNECT:
            await _send_private(chat_id=chat_id, text=CONNECT_GUIDE_RU)
        elif raw == BTN_FORMAT:
            await _send_private(chat_id=chat_id, text=FORMAT_SET_VK_RU)
        elif raw == BTN_STATUS:
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
                await _send_private(
                    chat_id=chat_id,
                    text=(
                        f"VK group_id: {st['selected_vk_group_id']}\n"
                        f"Токен: {tok}\n"
                        f"Автопостинг: {'вкл' if st['enabled'] else 'выкл'}"
                    ),
                )
            else:
                await _send_private(
                    chat_id=chat_id, text="Настроек пока нет. Нажми «Главная» или «Как подключить»."
                )
        elif raw == BTN_ENABLE:
            async with pool.acquire() as conn:
                await conn.execute(
                    "UPDATE user_settings SET enabled=TRUE, updated_at=now() WHERE tg_user_id=$1",
                    tg_user_id,
                )
            await _send_private(chat_id=chat_id, text="Автопостинг включен.")
        elif raw == BTN_DISABLE:
            async with pool.acquire() as conn:
                await conn.execute(
                    "UPDATE user_settings SET enabled=FALSE, updated_at=now() WHERE tg_user_id=$1",
                    tg_user_id,
                )
            await _send_private(chat_id=chat_id, text="Автопостинг выключен.")
        elif raw == BTN_CLEAR:
            async with pool.acquire() as conn:
                await conn.execute("DELETE FROM vk_accounts WHERE tg_user_id=$1", tg_user_id)
                await conn.execute(
                    """
                    UPDATE user_settings
                    SET selected_vk_group_id=NULL, enabled=FALSE,
                        vk_connect_step=0, vk_connect_group_id=NULL, updated_at=now()
                    WHERE tg_user_id=$1
                    """,
                    tg_user_id,
                )
            await _send_private(
                chat_id=chat_id,
                text="Токен и привязка к группе удалены. Автопостинг выключен.",
            )
        elif _cmd_base(text) == "/start":
            async with pool.acquire() as conn:
                await _clear_vk_connect_flow(conn, tg_user_id)
            await _send_private(chat_id=chat_id, text=WELCOME_RU)
        elif _cmd_base(text) == "/token_help":
            await _send_private(chat_id=chat_id, text=CONNECT_GUIDE_RU)
        elif _cmd_base(text) == "/set_vk":
            parts0 = text.strip().split(None, 1)
            rest = parts0[1].strip() if len(parts0) > 1 else ""
            parts = rest.split(None, 1)
            if len(parts) < 2:
                await _send_private(
                    chat_id=chat_id,
                    text=(
                        "Формат: /set_vk <id_группы> <токен>\n"
                        "Проще: кнопка «Подключить VK» — ссылка на сообщество, затем токен вторым сообщением.\n"
                        "Подробности — «Формат /set_vk» или «Как подключить»."
                    ),
                )
                return {"ok": True}
            try:
                group_id = int(parts[0].strip())
            except ValueError:
                await _send_private(
                    chat_id=chat_id,
                    text="id_группы должен быть числом (без минуса), например 123456789.",
                )
                return {"ok": True}
            token = parts[1].strip()
            if not token:
                await _send_private(
                    chat_id=chat_id,
                    text="Токен пустой. Формат: /set_vk <id_группы> <токен>",
                )
                return {"ok": True}
            ok, msgtext = await _persist_vk_token(
                tg_user_id=tg_user_id, group_id=group_id, token=token
            )
            await _send_private(chat_id=chat_id, text=msgtext)
        elif _cmd_base(text) == "/clear_vk":
            async with pool.acquire() as conn:
                await conn.execute("DELETE FROM vk_accounts WHERE tg_user_id=$1", tg_user_id)
                await conn.execute(
                    """
                    UPDATE user_settings
                    SET selected_vk_group_id=NULL, enabled=FALSE,
                        vk_connect_step=0, vk_connect_group_id=NULL, updated_at=now()
                    WHERE tg_user_id=$1
                    """,
                    tg_user_id,
                )
            await _send_private(
                chat_id=chat_id,
                text="Токен и привязка к группе удалены. Автопостинг выключен.",
            )
        elif _cmd_base(text) == "/status":
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
                await _send_private(
                    chat_id=chat_id,
                    text=(
                        f"VK group_id: {st['selected_vk_group_id']}\n"
                        f"Токен: {tok}\n"
                        f"Автопостинг: {'вкл' if st['enabled'] else 'выкл'}"
                    ),
                )
            else:
                await _send_private(
                    chat_id=chat_id, text="Настроек пока нет. Нажми «Главная» или «Как подключить»."
                )
        elif _cmd_base(text) == "/enable":
            async with pool.acquire() as conn:
                await conn.execute(
                    "UPDATE user_settings SET enabled=TRUE, updated_at=now() WHERE tg_user_id=$1",
                    tg_user_id,
                )
            await _send_private(chat_id=chat_id, text="Автопостинг включен.")
        elif _cmd_base(text) == "/disable":
            async with pool.acquire() as conn:
                await conn.execute(
                    "UPDATE user_settings SET enabled=FALSE, updated_at=now() WHERE tg_user_id=$1",
                    tg_user_id,
                )
            await _send_private(chat_id=chat_id, text="Автопостинг выключен.")
        elif (
            step == VK_STEP_TOKEN
            and raw
            and not raw.startswith("/")
            and raw not in ALL_MENU_BUTTONS
        ):
            if pending_gid is None:
                async with pool.acquire() as conn:
                    await _clear_vk_connect_flow(conn, tg_user_id)
                await _send_private(
                    chat_id=chat_id,
                    text="Не удалось продолжить подключение VK — нажми «Подключить VK» и начни с шага 1.",
                )
            else:
                new_gid = parse_vk_group_id_from_text(raw)
                if new_gid is not None:
                    async with pool.acquire() as conn:
                        await _set_vk_connect_await_token(conn, tg_user_id, new_gid)
                    await _send_private(
                        chat_id=chat_id,
                        text=VK_MSG_ASK_TOKEN.format(gid=new_gid),
                    )
                else:
                    ok, msgtext = await _persist_vk_token(
                        tg_user_id=tg_user_id, group_id=pending_gid, token=raw
                    )
                    await _send_private(chat_id=chat_id, text=msgtext)
        elif (
            step == VK_STEP_LINK
            and raw
            and not raw.startswith("/")
            and raw not in ALL_MENU_BUTTONS
        ):
            gid = parse_vk_group_id_from_text(raw)
            if gid is None:
                await _send_private(
                    chat_id=chat_id,
                    text=(
                        "Не вижу в сообщении ссылку на сообщество VK. Пришли, например:\n"
                        "https://vk.com/club123456789\n"
                        "или https://vk.com/public123456789"
                    ),
                )
            else:
                async with pool.acquire() as conn:
                    await _set_vk_connect_await_token(conn, tg_user_id, gid)
                await _send_private(chat_id=chat_id, text=VK_MSG_ASK_TOKEN.format(gid=gid))
        elif raw and not raw.startswith("/") and raw not in ALL_MENU_BUTTONS:
            await _send_private(
                chat_id=chat_id,
                text="Не понял сообщение. Жми кнопку снизу или «Главная».",
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
        "OAuth VK отключён. В Telegram: кнопка «Подключить VK» или команда /set_vk.",
        status_code=410,
    )


@app.get("/health")
async def health():
    return {"ok": True, "mode": "community_token"}
