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


async def vk_groups_get_admin(*, token: str, api_version: str) -> list[dict]:
    resp = await vk_api_call(
        method="groups.get",
        token=token,
        api_version=api_version,
        params={
            "filter": "admin",
            "extended": "1",
            "fields": "name,screen_name",
            "count": "200",
        },
    )
    items = resp.get("items") if isinstance(resp, dict) else None
    if not isinstance(items, list):
        return []
    return [i for i in items if isinstance(i, dict)]


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

