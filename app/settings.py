from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=".env", env_file_encoding="utf-8", extra="ignore")

    app_base_url: str = "http://localhost:8000"

    telegram_bot_token: str
    telegram_webhook_secret: str

    vk_api_version: str = "5.199"

    # VK ID / OAuth (web). Пустые значения допускаются, чтобы сервис поднимался на Railway
    # до того, как ты добавишь переменные в Variables.
    vk_client_id: str = ""
    vk_client_secret: str = ""
    vk_redirect_path: str = "/vk/callback"

    @property
    def vk_oauth_configured(self) -> bool:
        return bool(self.vk_client_id.strip() and self.vk_client_secret.strip())


settings = Settings()

