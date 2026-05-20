import asyncpg


SCHEMA_SQL = """
CREATE TABLE IF NOT EXISTS users (
  id BIGSERIAL PRIMARY KEY,
  tg_user_id BIGINT NOT NULL UNIQUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS telegram_channels (
  id BIGSERIAL PRIMARY KEY,
  tg_chat_id BIGINT NOT NULL UNIQUE,
  title TEXT,
  username TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS channel_owners (
  tg_chat_id BIGINT NOT NULL REFERENCES telegram_channels(tg_chat_id) ON DELETE CASCADE,
  tg_user_id BIGINT NOT NULL REFERENCES users(tg_user_id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tg_chat_id, tg_user_id)
);

CREATE TABLE IF NOT EXISTS vk_accounts (
  id BIGSERIAL PRIMARY KEY,
  tg_user_id BIGINT NOT NULL UNIQUE REFERENCES users(tg_user_id) ON DELETE CASCADE,
  vk_user_id BIGINT,
  access_token TEXT NOT NULL,
  expires_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS user_settings (
  tg_user_id BIGINT NOT NULL PRIMARY KEY REFERENCES users(tg_user_id) ON DELETE CASCADE,
  selected_vk_group_id BIGINT,
  enabled BOOLEAN NOT NULL DEFAULT FALSE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS oauth_states (
  state TEXT NOT NULL PRIMARY KEY,
  tg_user_id BIGINT NOT NULL REFERENCES users(tg_user_id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS oauth_pkce (
  tg_user_id BIGINT PRIMARY KEY REFERENCES users(tg_user_id) ON DELETE CASCADE,
  code_verifier TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
"""


async def init_db(conn: asyncpg.Connection) -> None:
    await conn.execute(SCHEMA_SQL)
    await conn.execute(
        "ALTER TABLE user_settings ADD COLUMN IF NOT EXISTS vk_connect_step SMALLINT NOT NULL DEFAULT 0"
    )
    await conn.execute(
        "ALTER TABLE user_settings ADD COLUMN IF NOT EXISTS vk_connect_group_id BIGINT"
    )


async def ensure_user(conn: asyncpg.Connection, tg_user_id: int) -> None:
    await conn.execute(
        "INSERT INTO users(tg_user_id) VALUES ($1) ON CONFLICT (tg_user_id) DO NOTHING",
        int(tg_user_id),
    )
    await conn.execute(
        "INSERT INTO user_settings(tg_user_id) VALUES ($1) ON CONFLICT (tg_user_id) DO NOTHING",
        int(tg_user_id),
    )

