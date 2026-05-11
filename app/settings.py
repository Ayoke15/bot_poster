from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=".env", env_file_encoding="utf-8", extra="ignore")

    app_base_url: str = "http://localhost:8000"

    telegram_bot_token: str
    telegram_webhook_secret: str

    vk_api_version: str = "5.199"


settings = Settings()

