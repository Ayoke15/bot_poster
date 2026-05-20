# TG → VK Autoposter (всё через Telegram‑бота)

Сервис без веб‑панели: настройка — командами боту в Telegram. Публичный URL нужен только для **Telegram webhook**.

## Как связать Telegram-бота с этим сервисом

1) В Telegram открой **@BotFather** → `/newbot` (или возьми уже созданного бота) → скопируй **HTTP API token** — это значение для переменной **`TELEGRAM_BOT_TOKEN`** (локально в `.env` или в Railway Variables).

2) Придумай секретную строку для пути webhook (любой длинный набор символов) — это **`TELEGRAM_WEBHOOK_SECRET`**. Она же будет в URL, который Telegram дергает при каждом событии.

3) Задеплой сервис и выставь **`APP_BASE_URL`** ровно твой публичный HTTPS-адрес (например `https://….up.railway.app`). После старта приложение само вызывает `setWebhook`: Telegram будет слать апдейты на  
   `APP_BASE_URL/telegram/webhook/TELEGRAM_WEBHOOK_SECRET`  
   Если домен или секрет поменялся — перезапусти сервис (webhook перезапишется).

4) Пользователи пишут **тому же боту**, чей токен ты указал: `/start`, кнопки меню и команды вроде `/set_vk` обрабатываются уже твоим кодом на сервере.

5) Чтобы бот видел посты **канала**, его нужно добавить в этот канал **администратором** (отдельно от пунктов 1–4).

Итого: отдельной «регистрации бота в проекте» кроме токена и URL не нужно — связка это **токен + публичный URL + webhook**.

## Что делает

- Слушает посты в Telegram‑канале через webhook (`channel_post`).
- Публикует текст в VK‑группу через `wall.post`.
- Настройка через бота (токен сообщества VK, без OAuth):
  - Кнопка **«Подключить VK»** — сначала ссылка на сообщество (бот вытащит id), вторым сообщением токен.
  - Кнопка **«Как подключить»** (и `/token_help`) — полная инструкция Telegram + VK.
  - В личке снизу **кнопки**: формат `/set_vk`, статус, включить/выключить, сброс VK, главная.
  - `/enable` / `/disable` — автопостинг.
  - `/status` / `/clear_vk` — посмотреть или сбросить токен.

## Запуск (Windows / PowerShell)

1) Установи зависимости:

```bash
python -m venv .venv
.\.venv\Scripts\Activate.ps1
pip install -r requirements.txt
```

2) Создай `.env` из примера:

- Скопируй `.env.example` → `.env`
- Заполни:
  - `APP_BASE_URL` — публичный URL сервиса (для Telegram webhook)
  - `DATABASE_URL` — строка подключения к Postgres
  - `TELEGRAM_BOT_TOKEN`
  - `TELEGRAM_WEBHOOK_SECRET`

3) Запусти:

```bash
uvicorn app.main:app --host 0.0.0.0 --port 8000
```

## Деплой на сервер (Docker Compose + Nginx)

На сервере должно быть установлено: Docker + Docker Compose plugin.

1) Склонируй репозиторий и создай `.env` (рядом с `docker-compose.yml`).

Минимально нужно:

- `APP_BASE_URL=https://your-domain` (обязательно **https** для Telegram/VK)
- `TELEGRAM_BOT_TOKEN=...`
- `TELEGRAM_WEBHOOK_SECRET=...`

2) Подними сервис:

```bash
docker compose up -d --build
```

После этого Nginx слушает `80` порт и проксирует в `app`.

### Про HTTPS (важно)

Telegram webhook обычно требует **HTTPS публично**.

#### Вариант B (Ubuntu + Let’s Encrypt, рекомендую)

1) Открой порты `80` и `443` на сервере (ufw):

```bash
sudo ufw allow 80
sudo ufw allow 443
```

2) В файле `deploy/nginx/conf.d/default.conf` замени:

- `YOUR_DOMAIN_HERE` на твой домен (например `example.com`) — **в двух местах**: `server_name` и пути `live/...`

3) Подними nginx+app, чтобы challenge работал:

```bash
docker compose up -d --build nginx app
```

4) Получи сертификат (через контейнер certbot):

```bash
docker compose run --rm certbot certonly --webroot -w /var/www/certbot \
  -d example.com \
  --email you@example.com \
  --agree-tos --no-eff-email
```

5) Перезапусти nginx:

```bash
docker compose restart nginx
```

6) Подними всё остальное:

```bash
docker compose up -d
```

Теперь выставляй `APP_BASE_URL=https://example.com`.

#### Вариант A (самый простой): Cloudflare / внешний HTTPS‑прокси

Если домен на Cloudflare — можно включить проксирование и SSL там, а до сервера гонять HTTP. Тогда `APP_BASE_URL` всё равно ставь `https://...`.

Сертификаты Let’s Encrypt нужно будет обновлять (renew). Для диплома обычно достаточно ручного обновления раз в 60–90 дней, но могу добавить авто-renew контейнером, если надо.

## Как пользоваться

1) Напиши боту `/start` (или «Главная»). Подробности — **«Как подключить»** или `/token_help`.
2) Подключи VK: кнопка **«Подключить VK»** — ссылка на сообщество одним сообщением, токен вторым (либо одной строкой `/set_vk`, см. «Формат /set_vk»).
3) Добавь бота админом в Telegram‑канал, который нужно читать.
4) Включи автопостинг: кнопка «Включить» или `/enable`.

## Важно про VK

Используется **ключ сообщества** (service/community token), не OAuth. У ключа в настройках VK должны быть права на **публикацию на стене** (`wall.post`).

## База

PostgreSQL (в docker-compose поднимется контейнер `db`).

## Деплой на Railway (быстро)

Railway даст тебе:
- **HTTPS домен** для webhook/callback
- **Postgres** как плагин (и переменную `DATABASE_URL`)

### Шаги

1) Залей проект в GitHub.

2) В Railway:
- **New Project → Deploy from GitHub Repo**
- Затем **Add → Database → PostgreSQL**

3) В переменных Railway (**Variables** сервиса с приложением) обязательно:
- `TELEGRAM_BOT_TOKEN`
- `TELEGRAM_WEBHOOK_SECRET`
- `APP_BASE_URL` (после генерации домена)
- `VK_API_VERSION` (опционально, по умолчанию `5.199`)

`DATABASE_URL` — из подключённого Postgres.

Токен VK пользователь передаёт **боту в Telegram** (`/set_vk`), в Railway его задавать не нужно.

4) После первого деплоя Railway покажет публичный домен вида `https://xxxxx.up.railway.app`.
Поставь:
- `APP_BASE_URL=https://xxxxx.up.railway.app`

5) Redirect URI в VK для этого режима **не нужен** (OAuth не используется).

Готово: `/start` → «Подключить VK» (ссылка + токен) или `/set_vk …` → добавить бота в канал → «Включить» (или `/enable`).

