# telegram-bridge

Telegram MCP server and radar for a small group of people. Every user connects
their own Telegram account through the bot and gets access only to that
account: its MCP tools, watch rules, alerts, deleted-message archive,
notification API, tokens and dashboard. There is no administrator role.

- SQLite storage; Telegram sessions encrypted with AES-256-GCM.
- Telegram bot via `telego`.
- Telegram user API monitoring via `gotd/td`.
- Mini App compatible web UI via `net/http` and `html/template`.
- MCP access to the caller's own Telegram account, with explicit cloud media processing.
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

Required env for Telegram accounts:

```sh
export TELEGRAM_API_ID="123456"
export TELEGRAM_API_HASH="api_hash"
export TELEGRAM_BRIDGE_SESSION_KEY="$(openssl rand -base64 32)"
```

The session key encrypts every stored Telegram session. Keep it stable and
secret: losing it disconnects every user, and anyone holding it together with
the database can use the sessions.

Each user connects their own account with `/login` in a private chat with the
bot. The bot asks them to share their contact (typed phone numbers are refused,
so nobody can start a login for someone else's number) and sends a short-lived
site login link. The Telegram code and 2FA password are entered on the HTTPS
site, not in the bot chat: Telegram blocks code-based sign-in after a code is
shared in a bot chat. The session is saved only if the account that signs in is
the same Telegram user who asked the bot; otherwise it is logged out at once.
`/logout` ends the Telegram session and revokes that user's MCP connections and
tokens.

## Bot commands

The bot works only in private chats, and every command acts on the sender's
own account.

- `/start` turns alerts on.
- `/stop` turns them off.
- `/add phrase` adds a simple one-term watch rule.
- `/watch name :: any1, any2 :: required1 :: excluded1` adds a flexible watch rule.
- `/del phrase-or-id` deletes one of your rules.
- `/keywords` lists your rules.
- `/recent` shows your latest matches.
- `/login` connects your Telegram account.
- `/loginstatus` shows its status.
- `/logout` disconnects it and revokes your MCP connections and tokens.
- `/cancel` cancels an in-progress login.
- `/connections` lists and revokes your MCP clients connected through OAuth.

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
fly secrets set TELEGRAM_BRIDGE_SESSION_KEY="$(openssl rand -base64 32)"
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

Each user connects their account through the bot:

1. Open the bot in Telegram.
2. Send `/start`.
3. Send `/login` and share your contact.
4. Open the login link from the bot.
5. Enter the Telegram login code and 2FA password on the site.

### Upgrading a single-account deployment

On the first start of this version, an existing `TELEGRAM_BRIDGE_SESSION` file
is encrypted into the database under the account it belongs to, and the
plaintext file is deleted. Existing rules, matches, sources and the deleted-message archive
are assigned to that account. `TELEGRAM_BRIDGE_MCP_TOKEN`,
`TELEGRAM_BRIDGE_NOTIFICATION_TOKEN` and `TELEGRAM_BRIDGE_NOTIFICATION_CHAT_IDS`
become that account's personal tokens and groups in the same run only; remove
them from the environment afterwards. Set `TELEGRAM_BRIDGE_SESSION_KEY` before
upgrading.

## HTTP notification API

`POST /notifications/v1/messages` posts to a group as the token owner's own
Telegram account. Each user creates a notification token and allows groups on
their dashboard; a token can post only to its owner's groups, only as its owner.
Notification tokens are not accepted by MCP, and MCP tokens are not accepted
here. MCP exposes no send tool; its media recognition actions require explicit
paid-processing requests.

The JSON request contains `chat`, `event_id` and `text`. SQLite receipts deduplicate
an event across clients and server restarts. See [API setup and recovery](docs/NOTIFICATIONS.md)
for activation, verification, security boundaries and rollback.

## MCP

The Streamable HTTP endpoint lives at `https://<your-host>/mcp`. Clients
authenticate with OAuth approved in the bot, or with a personal MCP token from
the dashboard. Either way the tools act only on the account that owns the
credential.

### OAuth with bot approval

OAuth is opt-in. It needs `TELEGRAM_BRIDGE_OAUTH=on`, the bot token,
`TELEGRAM_BRIDGE_PUBLIC_URL`, and Telegram Login for the bot:

1. In @BotFather, open the bot, choose **Login Widget**, and add the allowed
   URL `https://<your-host>/oauth/telegram/callback`.
2. Store the client secret it shows as `TELEGRAM_LOGIN_CLIENT_SECRET`, for
   example with `fly secrets import`. The client ID is the bot ID from the token.

Clients discover OAuth from the `/mcp` 401 challenge, register dynamically, and
use PKCE:

```sh
codex mcp add telegram-bridge --url https://telegram-bridge.fly.dev/mcp
claude mcp add --transport http --scope user telegram-bridge https://telegram-bridge.fly.dev/mcp
claude mcp login telegram-bridge
```

Claude Desktop and claude.ai custom connectors use the same URL; their callback
is allowlisted.

The client opens an authorization page with **Войти через Telegram**. After
signing in on oauth.telegram.org, the page shows a two-digit number, and the bot
sends that user the client name, IP address, and browser with four numbers and
**Отклонить**. Pressing the number from the page approves; any other button
denies the request.

- Signing in binds the request to the Telegram user of the browser that opened
  it. Only that user is asked and can approve, for their own connected account.
  A link forwarded to someone else fails in their browser, and the bot warns
  them instead of showing the prompt, so a user cannot be talked into approving
  a request somebody else opened.
- The bot reports every new connection with its client name and IP address, so
  an unexpected one can be revoked at once.
- Codes and tokens never pass through Telegram. Only the browser that opened the
  page receives the single authorization code.
- Redirects are limited to loopback addresses and the Claude connector callback;
  add exact HTTPS callbacks with `TELEGRAM_BRIDGE_OAUTH_REDIRECT_URIS`.
- Access tokens last one hour. Refresh tokens rotate, expire after 90 days
  without use, and connections end after a year. Reusing an old refresh token
  revokes the connection and notifies its user. SQLite stores only SHA-256
  hashes.
- `/connections` lists active clients and revokes them.

### Personal MCP token

Clients without OAuth, such as the Codex plugin and the local dev adapter, use
a personal MCP token created on the dashboard and shown once. Put it in
`TELEGRAM_BRIDGE_MCP_TOKEN` for the plugin. Clients send
`Authorization: Bearer <token>` on every request; media download URLs need the
same credential.

The MCP server reuses each user's live gotd client inside `telegram-bridge serve`,
so it does not create a second Telegram session or require another login.

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

Each connected account keeps a private SQLite snapshot of its direct,
non-bot chats so the bot can report messages that later disappear. On the
first run it seeds up to the latest 100 messages from each direct dialog in
the 100 most recent Telegram dialogs, then keeps new messages and edits
current. Snapshots are retained for 90 days, with a 25,000-message safety cap
per account for the small Fly volume. Pending alerts are protected from pruning.

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

Dashboard reads and mutations require a validated Telegram Mini App session,
and every page and action is scoped to that Telegram user. Rules can be scoped
only to the user's own sources. Opening the public URL directly shows only an
authentication prompt; it does not render sources, rules, or recent match text.

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
  | telegram-bridge rules-import -owner 123456789
```

`-owner` is the Telegram user ID that owns the imported rules.

Imports may also define `default_exclude`, `default_sources`, and
`default_exclude_complete_bike`. A rule inherits the default sources when
`sources` is omitted; an explicit `sources` array replaces the defaults.
[`rules/example.json`](rules/example.json) shows the format, including deletions,
default exclusions, a numeric minimum, and a source-scoped rule:

```sh
telegram-bridge rules-import -owner 123456789 < rules/example.json
```

Keep personal rule packs in `rules/`: every JSON file there except the example
is ignored by git. Personal regression tests can live beside them in
`cmd/telegram-bridge/rules_private_test.go`, which is ignored as well.
