from __future__ import annotations

import httpx


class VkError(RuntimeError):
    pass


async def vk_api_call(*, method: str, token: str, api_version: str, params: dict[str, str]) -> dict:
    data = {"access_token": token, "v": api_version, **params}
    async with httpx.AsyncClient(timeout=20) as client:
        r = await client.post(f"https://api.vk.com/method/{method}", data=data)
        r.raise_for_status()
        payload = r.json()
    if "error" in payload:
        err = payload["error"]
        raise VkError(f"VK error {err.get('error_code')}: {err.get('error_msg')}")
    resp = payload.get("response")
    if not isinstance(resp, dict) and not isinstance(resp, list):
        raise VkError("VK: unexpected response format")
    return payload["response"]


async def vk_verify_community_token(*, token: str, api_version: str, group_id: int) -> str:
    """Проверяет, что токен сообщества подходит к group_id. Возвращает короткое имя группы или ''."""
    gid = str(abs(int(group_id)))
    resp = await vk_api_call(
        method="groups.getById",
        token=token,
        api_version=api_version,
        params={"group_ids": gid, "fields": "name"},
    )
    groups = resp if isinstance(resp, list) else []
    if not groups or not isinstance(groups[0], dict):
        raise VkError("groups.getById: пустой ответ")
    name = groups[0].get("name")
    return str(name) if name else ""


async def vk_wall_post(
    *,
    token: str,
    api_version: str,
    group_id: int,
    message: str,
    link: str | None = None,
) -> int:
    """
    Posts to a community wall using community token.

    Notes:
    - VK expects owner_id to be negative for communities.
    - from_group=1 posts as the community.
    - This implementation posts text (+ optional link attachment).
    """
    owner_id = -abs(int(group_id))
    params: dict[str, str] = {
        "owner_id": str(owner_id),
        "from_group": "1",
        "message": message,
    }
    if link:
        params["attachments"] = link

    resp = await vk_api_call(method="wall.post", token=token, api_version=api_version, params=params)
    post_id = resp.get("post_id") if isinstance(resp, dict) else None
    if not isinstance(post_id, int):
        raise VkError("VK: unexpected response format")
    return post_id

