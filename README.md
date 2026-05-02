# TG → VK Autoposter (всё через Telegram‑бота)

Сервис без веб‑панели: управление подключением делается командами боту. Веб‑часть нужна только как VK OAuth callback.

## Что делает

- Слушает посты в Telegram‑канале через webhook (`channel_post`).
- Публикует текст в VK‑группу через `wall.post`.
- Настройка через бота:
  - `/connect_vk` → открыть ссылку VK‑авторизации → бот покажет список админских групп → выбрать группу кнопкой.
  - `/enable` / `/disable` — включить/выключить автопостинг.
  - `/status` — посмотреть текущие настройки.

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
  - `APP_BASE_URL` — публичный URL сервиса (для VK callback и Telegram webhook)
  - `DATABASE_URL` — строка подключения к Postgres
  - `TELEGRAM_BOT_TOKEN`
  - `TELEGRAM_WEBHOOK_SECRET`
  - `VK_CLIENT_ID`, `VK_CLIENT_SECRET`

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
- `VK_CLIENT_ID=...`
- `VK_CLIENT_SECRET=...`

2) Подними сервис:

```bash
docker compose up -d --build
```

После этого Nginx слушает `80` порт и проксирует в `app`.

### Про HTTPS (важно)

Telegram webhook и VK OAuth callback обычно требуют **HTTPS публично**.

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

1) Напиши боту `/start`.
2) Подключи VK: `/connect_vk` → открой ссылку → разреши доступ → вернись в Telegram → выбери группу кнопкой.
3) Добавь бота админом в Telegram‑канал, который нужно читать.
4) Включи автопостинг: `/enable`.

## Важно про VK права

Этот прототип использует токен, полученный через VK ID OAuth. В зависимости от текущих ограничений VK, доступ к `groups.get`/`wall.post` может требовать дополнительных прав/разрешений приложения.

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

3) В переменных Railway (Variables) добавь:
- `TELEGRAM_BOT_TOKEN`
- `TELEGRAM_WEBHOOK_SECRET`
- `VK_CLIENT_ID`
- `VK_CLIENT_SECRET`
- `VK_API_VERSION` (опционально, по умолчанию `5.199`)

`DATABASE_URL` Railway обычно добавит сам после подключения Postgres.

4) После первого деплоя Railway покажет публичный домен вида `https://xxxxx.up.railway.app`.
Поставь:
- `APP_BASE_URL=https://xxxxx.up.railway.app`

5) В VK кабинете в Redirect URI укажи:
- `https://xxxxx.up.railway.app/vk/callback`

Готово: пиши боту `/start`, потом `/connect_vk`.

