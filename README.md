# tg-radar

Single-binary Telegram radar MVP:

- SQLite storage.
- Telegram bot via `telego`.
- Telegram user API monitoring via `gotd/td`.
- Mini App compatible web UI via `net/http` and `html/template`.
- Fly.io deployment with a persistent `/data` volume.

## Run locally

```sh
go run ./cmd/tg-radar migrate
go run ./cmd/tg-radar serve
```

Required env for the bot:

```sh
export TELEGRAM_BOT_TOKEN="123:bot-token"
```

Local development also reads `.env.local` automatically. Keep real secrets there, not in tracked Go files.

Required env for user API monitoring:

```sh
export TELEGRAM_API_ID="123456"
export TELEGRAM_API_HASH="api_hash"
```

Optional env:

```sh
export TELEGRAM_PHONE="+15551234567"
export TELEGRAM_PASSWORD="account-password"
export TG_RADAR_ADMIN_CHAT_IDS="123456789"
```

The bot can authorize the gotd user session with `/login`. The bot only asks for the Telegram phone number and then sends a short-lived site login link. Enter the Telegram login code and 2FA password on the HTTPS site, not in the bot chat: Telegram blocks code-based sign-in after a code is shared in a bot chat. If `TG_RADAR_ADMIN_CHAT_IDS` is not set, the first chat that runs `/start` becomes the admin chat for MVP operations.

The CLI `login` command still works and stores the gotd user session in `data/telegram.session` by default. On Fly.io it uses `/data/telegram.session`.

## Bot commands

- `/start` subscribes the current chat to alerts.
- `/stop` unsubscribes it.
- `/add phrase` adds a monitored phrase.
- `/del phrase-or-id` deletes a monitored phrase.
- `/keywords` lists phrases.
- `/recent` shows latest matches.
- `/login` authorizes Telegram user API monitoring.
- `/loginstatus` shows user session status.
- `/cancel` cancels an in-progress bot login.

## Fly.io

Create the app and volume once:

```sh
fly apps create tg-radar
fly volumes create tg_radar_data -r fra -s 1
```

Set secrets:

```sh
fly secrets set TELEGRAM_BOT_TOKEN="123:bot-token"
fly secrets set TELEGRAM_API_ID="123456"
fly secrets set TELEGRAM_API_HASH="api_hash"
fly secrets set TG_RADAR_PUBLIC_URL="https://tg-radar.fly.dev"
```

Deploy:

```sh
fly deploy
```

Fast code-only redeploy:

```sh
scripts/deploy-fast.sh
```

This builds a Linux/amd64 binary locally, packs it with UPX, and deploys via
`fly deploy --local-only` using `fly.fast.toml`. It is meant for quick Go code
iterations; use plain `fly deploy` after changing Fly config, Docker base
images, mounts, or services.

For an extra post-deploy public health check:

```sh
WAIT_HEALTH=1 scripts/deploy-fast.sh
```

Authorize the user session once through the bot:

1. Open the bot in Telegram.
2. Send `/start`.
3. Send `/login`.
4. Share your phone if the bot asks for it.
5. Open the login link from the bot.
6. Enter the Telegram login code and 2FA password on the site.

The old SSH path is still available:

```sh
fly ssh console -C "tg-radar login"
```
