# telegram-bridge

Single-binary Telegram radar MVP:

- SQLite storage.
- Telegram bot via `telego`.
- Telegram user API monitoring via `gotd/td`.
- Mini App compatible web UI via `net/http` and `html/template`.
- Read-only MCP access to the logged-in Telegram account.
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
export TELEGRAM_BRIDGE_WORKER_TOKEN="a-different-random-bearer-token"
```

The bot can authorize the gotd user session with `/login`. The bot only asks for the Telegram phone number and then sends a short-lived site login link. Enter the Telegram login code and 2FA password on the HTTPS site, not in the bot chat: Telegram blocks code-based sign-in after a code is shared in a bot chat. If `TELEGRAM_BRIDGE_ADMIN_CHAT_IDS` is not set, the first chat that runs `/start` becomes the admin chat for MVP operations.

The CLI `login` command still works and stores the gotd user session in `data/telegram.session` by default. On Fly.io it uses `/data/telegram.session`.

## Telegram → Codex bridge

`/codex project :: prompt` creates, when needed, a private forum supergroup named `Codex · project`, adds it to the Telegram folder `Codex`, creates one forum topic per Codex task, and queues the prompt for a worker on the Mac. Further plain-text messages in that topic continue the same Codex task.

The Fly app only stores the queue and Telegram/Codex identifiers. Codex runs locally through `codex app-server`, so project files and the Codex login stay on the Mac. The worker API uses `TELEGRAM_BRIDGE_WORKER_TOKEN`, separate from the read-only MCP token.

Install or refresh the macOS LaunchAgent with one or more local project mappings:

```bash
scripts/install-codex-worker.sh telegram-bridge=/path/to/telegram-bridge
```

The worker starts Codex with `workspace-write` and `approvalPolicy=never`: normal edits inside the selected project are possible, while permission escalation is unavailable from Telegram. Its log is `~/Library/Logs/telegram-bridge-worker.log`; the final Codex message is posted to the originating Telegram topic.

Every 30 seconds the local worker reads the complete active and archived task lists from `codex app-server`. Existing active Codex tasks that were not created from Telegram are mirrored into the private `Codex · Active` forum, one topic per top-level task. The topic contains only the latest visible user or Codex message; changed mirror posts replace the previous mirror post and preserve common Markdown formatting. Codex user messages are sent through the authenticated Telegram user session, so they appear from the account rather than the bridge bot. A short neutral state is shown only when a task has no visible message. Sub-agent threads are not mirrored separately.

Read state is mirrored in both directions. Bot replies naturally make their topic unread; opening the task in Codex marks the Telegram topic read, and reading the topic in Telegram clears the task's unread marker in Codex. The Telegram user session also reconciles the actual unread count of every mapped topic every 15 seconds, so already-read topics and missed realtime updates converge automatically. Codex 0.149 does not expose this marker through `app-server`, so the local worker reads and updates only the `threads.has_user_event` column in `$CODEX_HOME/state_5.sqlite`. This narrow internal dependency should be rechecked when upgrading Codex.

A plain text post in a Codex forum's General topic is promoted into a new forum topic and queued as a Codex task. The original General post is removed after successful promotion. `Codex · Active` uses the only configured execution project automatically; when several projects exist, use `project :: task`.

Archiving a mapped task in Codex deletes its Telegram forum topic and all messages in that topic. The project forum remains. This deletion is intentionally one-way: unarchiving the Codex task does not recreate the Telegram topic.

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

## MCP

Set a dedicated bearer token to enable the Streamable HTTP endpoint at
`https://<your-host>/mcp`:

```sh
fly secrets set TELEGRAM_BRIDGE_MCP_TOKEN="$(openssl rand -hex 32)"
```

The MCP server reuses the live gotd client inside `telegram-bridge serve`, so it does
not create a second Telegram session or require another login. Clients must
send `Authorization: Bearer <token>` on every request.

Available read-only tools:

- `telegram_list_dialogs` lists recent dialogs and returns stable chat keys.
- `telegram_search_messages` searches globally or within one returned chat key.
- `telegram_get_history` reads and paginates a chat's message history.

No tools for sending, editing, forwarding, or deleting messages are exposed.

### Codex plugin

The canonical Codex plugin source and its marketplace manifest are tracked in
this repository under `plugins/telegram-bridge` and `.agents/plugins/marketplace.json`.
Install it from the repository marketplace with:

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
