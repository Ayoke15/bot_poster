from __future__ import annotations

import json
import logging

import httpx

logger = logging.getLogger(__name__)


class TelegramError(RuntimeError):
    pass


def extract_private_message(update: dict) -> dict | None:
    msg = update.get("message")
    if not isinstance(msg, dict):
        return None
    chat = msg.get("chat")
    if not isinstance(chat, dict):
        return None
    if chat.get("type") != "private":
        return None
    return msg


def extract_channel_from_update(update: dict) -> dict | None:
    msg = update.get("channel_post") or update.get("edited_channel_post")
    if not isinstance(msg, dict):
        return None
    chat = msg.get("chat")
    if not isinstance(chat, dict):
        return None
    if chat.get("type") != "channel":
        return None
    return chat


def extract_post_text(update: dict) -> str | None:
    msg = update.get("channel_post") or update.get("edited_channel_post")
    if not isinstance(msg, dict):
        return None

    text = msg.get("text")
    if isinstance(text, str) and text.strip():
        return text.strip()

    caption = msg.get("caption")
    if isinstance(caption, str) and caption.strip():
        return caption.strip()

    return None


async def tg_set_webhook(*, bot_token: str, base_url: str, secret_path: str) -> None:
    url = f"{base_url.rstrip('/')}/telegram/webhook/{secret_path}"
    payload = {
        "url": url,
        "allowed_updates": ["message", "channel_post", "edited_channel_post", "my_chat_member"],
    }

    async with httpx.AsyncClient(timeout=20) as client:
        r = await client.post(f"https://api.telegram.org/bot{bot_token}/setWebhook", json=payload)
        r.raise_for_status()
        data = r.json()

    if not data.get("ok"):
        raise TelegramError(f"setWebhook failed: {data}")


async def tg_set_my_commands(*, bot_token: str) -> None:
    """Меню команд слева от поля ввода в Telegram."""
    commands = [
        {"command": "start", "description": "Приветствие и кнопки"},
        {"command": "token_help", "description": "То же, что «Как подключить»"},
        {"command": "set_vk", "description": "Сохранить id группы и токен"},
        {"command": "status", "description": "Что сохранено"},
        {"command": "enable", "description": "Включить автопостинг"},
        {"command": "disable", "description": "Выключить автопостинг"},
        {"command": "clear_vk", "description": "Удалить токен VK"},
    ]
    async with httpx.AsyncClient(timeout=20) as client:
        r = await client.post(
            f"https://api.telegram.org/bot{bot_token}/setMyCommands",
            json={"commands": commands},
        )
        r.raise_for_status()
        data = r.json()
    if not data.get("ok"):
        raise TelegramError(f"setMyCommands failed: {data}")


async def tg_send_message(
    *,
    bot_token: str,
    chat_id: int,
    text: str,
    reply_markup: dict | None = None,
    disable_web_page_preview: bool = True,
) -> None:
    # Telegram надёжнее принимает reply_markup как JSON-строку в form body, не вложенный dict.
    data: dict[str, str] = {
        "chat_id": str(chat_id),
        "text": text,
    }
    if disable_web_page_preview:
        data["disable_web_page_preview"] = "true"
    if reply_markup:
        data["reply_markup"] = json.dumps(reply_markup, ensure_ascii=False)

    async with httpx.AsyncClient(timeout=20) as client:
        r = await client.post(
            f"https://api.telegram.org/bot{bot_token}/sendMessage",
            data=data,
        )
        r.raise_for_status()
        body = r.json()
    if not body.get("ok"):
        logger.error("sendMessage failed: %s", body)
        raise TelegramError(f"sendMessage failed: {body}")


