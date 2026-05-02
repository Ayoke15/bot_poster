from __future__ import annotations

import base64
import hashlib
import secrets
import time
import urllib.parse

import httpx


class VkOAuthError(RuntimeError):
    pass


def _b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).decode("ascii").rstrip("=")


def generate_pkce_pair() -> tuple[str, str]:
    verifier = _b64url(secrets.token_bytes(32))
    challenge = _b64url(hashlib.sha256(verifier.encode("ascii")).digest())
    return verifier, challenge


def build_authorize_url(
    *,
    client_id: str,
    redirect_uri: str,
    state: str,
    code_challenge: str,
    scope: str,
) -> str:
    # VK ID OAuth 2.1 authorize endpoint (web)
    # Docs: id.vk.com / id.vk.ru
    base = "https://id.vk.ru/authorize"
    q = {
        "response_type": "code",
        "client_id": client_id,
        "redirect_uri": redirect_uri,
        "state": state,
        "code_challenge": code_challenge,
        "code_challenge_method": "S256",
        "scope": scope,
    }
    return base + "?" + urllib.parse.urlencode(q)


async def exchange_code_for_token(
    *,
    client_id: str,
    client_secret: str,
    code: str,
    redirect_uri: str,
    code_verifier: str,
) -> dict:
    # VK ID token endpoint (OAuth 2.1)
    url = "https://id.vk.ru/oauth2/auth"
    data = {
        "grant_type": "authorization_code",
        "client_id": client_id,
        "client_secret": client_secret,
        "redirect_uri": redirect_uri,
        "code": code,
        "code_verifier": code_verifier,
    }
    async with httpx.AsyncClient(timeout=20) as client:
        r = await client.post(url, data=data)
        r.raise_for_status()
        payload = r.json()
    if "access_token" not in payload:
        raise VkOAuthError(f"token exchange failed: {payload}")
    return payload


def compute_expires_at(expires_in: int | None) -> str | None:
    if not expires_in:
        return None
    return time.strftime("%Y-%m-%d %H:%M:%S", time.gmtime(time.time() + int(expires_in)))

