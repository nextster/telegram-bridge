# telegram-bridge

Single-binary Telegram radar MVP:

- SQLite storage.
- Telegram bot via `telego`.
- Telegram user API monitoring via `gotd/td`.
- Mini App compatible web UI via `net/http` and `html/template`.
- MCP access to the logged-in Telegram account, with explicit cloud media processing.
- OpenRouter voice/video-note transcription and separately stored image description/OCR.
- Fly.io deployment with a persistent `/data` volume.

All authored source and desired configuration live in this repository,
including the Codex plugin, marketplace manifest, watch-rule packs, CI, and
Fly machine sizing. Real credentials, Telegram/SQLite state, generated build
artifacts, Codex registration/cache, and live Fly resources remain external.

## Run locally

```sh
test -e .env.local || cp .env.example .env.local
go run ./cmd/telegram-bridge migrate
go run ./cmd/telegram-bridge serve
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
export TELEGRAM_BRIDGE_ADMIN_CHAT_IDS="123456789"
export TELEGRAM_BRIDGE_MCP_TOKEN="a-long-random-bearer-token"
```

The bot can authorize the gotd user session with `/login`. The bot only asks for the Telegram phone number and then sends a short-lived site login link. Enter the Telegram login code and 2FA password on the HTTPS site, not in the bot chat: Telegram blocks code-based sign-in after a code is shared in a bot chat. If `TELEGRAM_BRIDGE_ADMIN_CHAT_IDS` is not set, the first chat that runs `/start` becomes the admin chat for MVP operations.

The CLI `login` command still works and stores the gotd user session in `data/telegram.session` by default. On Fly.io it uses `/data/telegram.session`.

## Bot commands

- `/start` subscribes the current chat to alerts.
- `/stop` unsubscribes it.
- `/add phrase` adds a simple one-term watch rule.
- `/watch name :: any1, any2 :: required1 :: excluded1` adds a flexible watch rule.
- `/del phrase-or-id` deletes a monitored phrase.
- `/keywords` lists phrases.
- `/recent` shows latest matches.
- `/login` authorizes Telegram user API monitoring.
- `/loginstatus` shows user session status.
- `/cancel` cancels an in-progress bot login.
- `/connections` lists and revokes MCP clients connected through OAuth.

## Fly.io

Create the app and volume once:

```sh
fly apps create telegram-bridge
fly volumes create telegram_bridge_data -r fra -s 1
```

Set secrets:

```sh
fly secrets set TELEGRAM_BOT_TOKEN="123:bot-token"
fly secrets set TELEGRAM_API_ID="123456"
fly secrets set TELEGRAM_API_HASH="api_hash"
fly secrets set TELEGRAM_BRIDGE_PUBLIC_URL="https://telegram-bridge.fly.dev"
```

Deploy:

```sh
fly deploy
```

Fast code-only redeploy:

```sh
scripts/deploy-fast.sh
```

This builds the deployment image locally with Apple `container`, pushes it to
the Fly registry, and deploys it using `fly.fast.toml`; OrbStack and a Docker
daemon are not required. Install the lightweight OCI registry client once with
`brew install regclient`; the script uses its `regctl` command to upload the
image produced by Apple `container` because Fly's registry is not compatible
with Apple `container image push`. Fly Machines currently run on x86_64, so
the default deployment target remains `linux/amd64` even on Apple Silicon. The
Dockerfiles and fast image are architecture-neutral, and a local ARM64 image
can be built with `TARGET_GOARCH=arm64 PLATFORM=linux/arm64 MODE=build`.

The fast path is meant for quick Go code iterations; use plain `fly deploy`
after changing Fly config, Docker base images, mounts, or services. Set
`BUILDER=docker` only when explicitly falling back to Docker Buildx.

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
fly ssh console -C "telegram-bridge login"
```

## HTTP notification API

`POST /notifications/v1/messages` sends authorized notifications through the
already-logged-in personal Telegram account. It uses a dedicated
`TELEGRAM_BRIDGE_NOTIFICATION_TOKEN` and a server-side group allowlist
(`TELEGRAM_BRIDGE_NOTIFICATION_CHAT_IDS`). The notification token must differ
from the MCP token. MCP exposes no send tool; its media recognition
actions require explicit paid-processing requests.

The JSON request contains `chat`, `event_id` and `text`. SQLite receipts deduplicate
an event across clients and server restarts. See [API setup and recovery](docs/NOTIFICATIONS.md)
for activation, verification, security boundaries and rollback.

## MCP

The Streamable HTTP endpoint lives at `https://<your-host>/mcp`. Clients
authenticate with OAuth approved in the bot, or with a static bearer token.

### OAuth with bot approval

OAuth is opt-in. Enable it with `TELEGRAM_BRIDGE_OAUTH=on` together with the bot
token and `TELEGRAM_BRIDGE_PUBLIC_URL`. Clients discover it from the `/mcp` 401
challenge, register dynamically, and use PKCE:

```sh
codex mcp add telegram-bridge --url https://telegram-bridge.fly.dev/mcp
claude mcp add --transport http --scope user telegram-bridge https://telegram-bridge.fly.dev/mcp
claude mcp login telegram-bridge
```

Claude Desktop and claude.ai custom connectors use the same URL; their callback
is allowlisted.

The client opens an authorization page with a two-digit number and a
**Подтвердить в Telegram** button. The button opens the bot, which shows the
client name, IP address, and browser with four numbers and **Отклонить**.
Pressing the number from the page approves; any other button denies the
request.

- The bot never sends approval prompts on its own, so nobody can push prompts
  to the owner by opening authorization pages.
- Only the Telegram account logged in to the bridge can approve, in its private
  chat with the bot. Other subscribers and admin chats cannot.
- Codes and tokens never pass through Telegram. Only the browser that opened the
  page receives the single authorization code.
- Redirects are limited to loopback addresses and the Claude connector callback;
  add exact HTTPS callbacks with `TELEGRAM_BRIDGE_OAUTH_REDIRECT_URIS`.
- Access tokens last one hour. Refresh tokens rotate, expire after 90 days
  without use, and connections end after a year. Reusing an old refresh token
  revokes the connection and notifies the owner. SQLite stores only SHA-256
  hashes.
- `/connections` lists active clients and revokes them.

### Static bearer token

A dedicated token still works for the Codex plugin, the local dev adapter, and
media download URLs:

```sh
fly secrets set TELEGRAM_BRIDGE_MCP_TOKEN="$(openssl rand -hex 32)"
```

The MCP server reuses the live gotd client inside `telegram-bridge serve`, so it does
not create a second Telegram session or require another login. Clients must
send `Authorization: Bearer <token>` on every request.

History and search tools:

- `telegram_list_dialogs` lists recent dialogs and returns stable chat keys.
- `telegram_search_messages` searches globally or within one returned chat key.
- `telegram_get_history` reads date-bounded/paginated history; explicit `process_media: true` with `confirm_paid: true` queues and waits for supported media in the returned page.

Media tools (available when the user API and MCP are configured):

- `telegram_get_attachment` reads metadata without downloading or calling AI.
- `telegram_download_attachment` caches an original and returns a bearer-protected download URL.
- `telegram_transcribe_media` explicitly queues paid voice/video-note transcription.
- `telegram_get_transcription` reads its status and full cached result.
- `telegram_analyze_image` explicitly queues paid image description and OCR.
- `telegram_get_image_analysis` reads both separately stored image outputs.
- `telegram_process_media_batch` queues up to 100 mixed-media references in one explicitly paid call and waits for shared cached jobs.
- `telegram_get_media_batch` reads/waits for multiple existing jobs without new paid work.

Cloud processing is disabled by default. It runs in the existing Fly `serve`
process using OpenRouter, SQLite, and FFmpeg; Telegram Desktop and local
inference models are not involved. Ordinary history reads never enqueue
media; only the explicit paid history mode does. Up to three provider requests run
concurrently with shared persistent deduplication and unchanged spending limits.
History remains paginated; timeout returns pending jobs, never a false completion.
See [media API, limits, deployment, and rollback](docs/MEDIA.md).

No tools for sending, editing, forwarding, or deleting messages are exposed.

### Codex plugin

The canonical Codex plugin source and its marketplace entry are tracked in
this repository under `plugins/telegram-bridge` and `.agents/plugins/marketplace.json`.
The install command syncs that source into the shared Nextster marketplace at
`~/.codex/marketplaces/nextster` without replacing other Nextster plugins:

```sh
scripts/codex-plugin.sh install
```

After changing the plugin or its skill, refresh its cachebuster and reinstall:

```sh
scripts/codex-plugin.sh reload
```

The reload command intentionally changes the tracked plugin version. Commit
that cachebuster with the plugin change. The Codex installation record and
generated plugin cache remain machine-local; start a new Codex task after a
reload so it picks up the new skill and MCP schema.

For local development, link future MCP processes to the current checkout once:

```sh
scripts/codex-plugin.sh dev:link
scripts/codex-plugin.sh dev:status
```

The dev override uses a stable bootstrap under `$CODEX_HOME/telegram-bridge-dev`
and runs the MCP adapter from this checkout. The adapter proxies the production
HTTP MCP and never opens the Telegram session. It permits the named, idempotent
media actions and non-destructive media-aware history in addition to read-only
tools. The normal workflow is:
edit → `scripts/check.sh` → open a new Codex task. Tool names and schemas are
fixed during MCP initialization, so an already-open task does not reload them.

MCP adapter changes need no reinstall or process restart. Return new tasks to the versioned production plugin with:

```sh
scripts/codex-plugin.sh dev:unlink
```

## Private message deletion alerts

The logged-in user session keeps a private SQLite snapshot of direct,
non-bot chats so the bot can report messages that later disappear. On the
first run it seeds up to the latest 100 messages from each direct dialog in
the 100 most recent Telegram dialogs, then keeps new messages and edits
current. Snapshots are retained for 90 days, with a 25,000-message safety cap
for the small Fly volume. Pending alerts are protected from pruning.

It stores text/captions and a media kind, not photo, video, voice, or file
bytes. The archive is not exposed on the dashboard or through MCP. Deletion
alerts are sent only to the private bot chat whose user ID owns the logged-in
Telegram session; that account must have sent `/start`. Failed sends stay in a
durable outbox with capped backoff, and large deletions are delivered in
small chunks.

Telegram deletion updates do not identify the actor or reason and do not
contain a distinct "whole chat deleted" flag. A batch is therefore reported
as several disappeared messages and may indicate a cleared history, without
attributing it to the other person. Clearing history only on the other
person's device is invisible; revoking it for both sides is observable.
Messages deleted before the first snapshot cannot be recovered. Secret chats
are outside the cloud user API and are not archived.

## Flexible watch rules

The dashboard can create grouped rules with:

- `any` aliases: at least one term must match;
- `all` terms: every term must match;
- `required_any`: one alias from every group must match (AND between groups,
  OR inside a group);
- `prefer` terms: matching terms add evidence to the score but never gate a
  result;
- `exclude` terms: any one suppresses the match;
- an operator note that is copied into a live alert;
- optional suppression of complete-bike listings, using the channel's
  `#bikes` taxonomy plus conservative multilingual/spec-sheet signals;
- optional Telegram source scoping;
- token-safe, normalized, and typo-tolerant phrase matching. Model names such
  as `XG1250`/`XG-1250` and `T25` normalize consistently without making `12`
  match `1200`.

Dashboard reads and mutations require a validated Telegram Mini App session
from the configured bot admin chat. Opening the public URL directly shows only
an authentication prompt; it does not render sources, rules, or recent match
text. Bot management commands and destructive callback buttons are likewise
restricted to the admin chat.

Exclusions are exact-only so `продан` cannot suppress a normal `продам`
listing. A required-any group can also contain a unit-aware numeric minimum,
for example `num>=1000:lm|lumen|lumens|люмен|лм`; the number and unit must be
adjacent, so a price or battery capacity cannot satisfy the light-output rule.

Existing keywords remain valid and behave like a rule with one `any` term. New
matches store a score and a short explanation of the matching alias.

Rules can also be migrated silently and atomically from JSON without sending
one bot status message per rule:

```sh
printf '%s' '{"delete":["old phrase"],"rules":[{"name":"front light","any":["front bike light","велофара"],"required_any":[["USB-C","Type-C"],["1000","1200","1600"]],"prefer":["daytime flash","Garmin mount"],"exclude":["sold","продано"],"note":"Verify the beam and underside mount."}]}' \
  | telegram-bridge rules-import
```

Imports may also define `default_exclude`, `default_sources`, and
`default_exclude_complete_bike`. A rule inherits the default sources when
`sources` is omitted; an explicit `sources` array replaces the defaults.
[`rules/example.json`](rules/example.json) shows the format, including deletions,
default exclusions, a numeric minimum, and a source-scoped rule:

```sh
telegram-bridge rules-import < rules/example.json
```

Keep personal rule packs in `rules/`: every JSON file there except the example
is ignored by git. Personal regression tests can live beside them in
`cmd/telegram-bridge/rules_private_test.go`, which is ignored as well.
