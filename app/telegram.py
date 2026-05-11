from __future__ import annotations

import httpx


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


async def tg_send_message(
    *,
    bot_token: str,
    chat_id: int,
    text: str,
    reply_markup: dict | None = None,
    disable_web_page_preview: bool = True,
) -> None:
    payload: dict = {
        "chat_id": chat_id,
        "text": text,
        "disable_web_page_preview": disable_web_page_preview,
    }
    if reply_markup:
        payload["reply_markup"] = reply_markup

    async with httpx.AsyncClient(timeout=20) as client:
        r = await client.post(f"https://api.telegram.org/bot{bot_token}/sendMessage", json=payload)
        r.raise_for_status()
        data = r.json()
    if not data.get("ok"):
        raise TelegramError(f"sendMessage failed: {data}")


