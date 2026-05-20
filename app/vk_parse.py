"""Извлечение числового id сообщества VK из ссылки или текста пользователя."""

from __future__ import annotations

import re
from urllib.parse import parse_qs, urlparse


def parse_vk_group_id_from_text(text: str) -> int | None:
    """
    Понимает ссылки вида vk.com/club123, public123, event456, sel=-123 в query.
    Короткое имя без префикса (только буквы) не резолвим без API — вернёт None.
    """
    s = (text or "").strip()
    if not s:
        return None
    s = s.splitlines()[0].strip()

    if not re.match(r"^[a-zA-Z][-a-zA-Z0-9+.]*://", s):
        s = "https://" + s.lstrip("/")

    parsed = urlparse(s)
    host = (parsed.netloc or "").lower()
    if host.startswith("www."):
        host = host[4:]
    if host not in ("vk.com", "m.vk.com", "vk.ru", "m.vk.ru"):
        return None

    path = (parsed.path or "").strip("/")

    def _id_from_query() -> int | None:
        qs = parse_qs(parsed.query)
        for key in ("sel", "oid", "z"):
            for v in qs.get(key) or []:
                m = re.match(r"^-(\d+)$", (v or "").strip())
                if m:
                    return int(m.group(1))
        return None

    if not path:
        return _id_from_query()

    seg = path.split("/")[-1].lower()
    if not seg:
        return _id_from_query()

    m = re.match(r"^club(\d+)$", seg)
    if m:
        return int(m.group(1))
    m = re.match(r"^public(\d+)$", seg)
    if m:
        return int(m.group(1))
    m = re.match(r"^event(\d+)$", seg)
    if m:
        return int(m.group(1))

    if seg.isdigit():
        return int(seg)

    qid = _id_from_query()
    if qid is not None:
        return qid

    return None
