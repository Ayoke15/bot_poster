CREATE TABLE IF NOT EXISTS poster_users (
 -- A Telegram user has one current VK binding, stored in this single row.
 user_id BIGINT PRIMARY KEY,
 group_id BIGINT,
 token_cipher TEXT,
 token_version BIGINT NOT NULL DEFAULT 0,
 enabled BOOLEAN NOT NULL DEFAULT FALSE,
 connect_step INT NOT NULL DEFAULT 0,
 pending_group BIGINT
);
CREATE TABLE IF NOT EXISTS poster_channels (
 chat_id BIGINT PRIMARY KEY,
 owner_id BIGINT NOT NULL REFERENCES poster_users(user_id),
 title TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS poster_events (
 update_id BIGINT PRIMARY KEY,
 received_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS poster_posts (
 chat_id BIGINT NOT NULL,
 message_id BIGINT NOT NULL,
 owner_id BIGINT NOT NULL REFERENCES poster_users(user_id),
 group_id BIGINT NOT NULL,
 token_version BIGINT NOT NULL,
 body TEXT NOT NULL,
 desired_version BIGINT NOT NULL,
 delivered_version BIGINT,
 vk_post_id BIGINT,
 guid TEXT NOT NULL UNIQUE,
 status TEXT NOT NULL DEFAULT 'pending',
 attempts INT NOT NULL DEFAULT 0,
 first_attempt_at TIMESTAMPTZ,
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 last_error TEXT,
 PRIMARY KEY(chat_id,message_id)
);
CREATE INDEX IF NOT EXISTS poster_posts_due ON poster_posts(next_attempt_at) WHERE status IN ('pending','processing');
CREATE TABLE IF NOT EXISTS poster_notifications (
 id BIGSERIAL PRIMARY KEY,
 owner_id BIGINT NOT NULL,
 body TEXT NOT NULL,
 attempts INT NOT NULL DEFAULT 0,
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 sent BOOLEAN NOT NULL DEFAULT FALSE,
 last_error TEXT
);
CREATE TABLE IF NOT EXISTS poster_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now());

ALTER TABLE poster_posts ADD COLUMN IF NOT EXISTS photo_file_id TEXT NOT NULL DEFAULT '';
ALTER TABLE poster_posts ADD COLUMN IF NOT EXISTS vk_attachment TEXT NOT NULL DEFAULT '';
ALTER TABLE poster_posts ADD COLUMN IF NOT EXISTS uploaded_photo_file_id TEXT NOT NULL DEFAULT '';
