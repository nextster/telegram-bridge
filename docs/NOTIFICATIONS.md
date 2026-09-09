# HTTP notification API

`POST /notifications/v1/messages` sends plain text as the already-authorized
personal Telegram account. It reuses the live gotd client, not a bot or a second
session. MCP remains read-only and has no notification tool.

## Authentication and activation

Configure a dedicated random `TELEGRAM_BRIDGE_NOTIFICATION_TOKEN` (at least 32
characters; generate with `openssl rand -hex 32`) in the server secret store and
in the authorized caller's environment. Never reuse the MCP or worker token:
configuration rejects equal values. Keep credentials out of Git, command-line
arguments and logs. Requests require `Authorization: Bearer <notification-token>`
over HTTPS. No cookie, MCP credential or Telegram session file is accepted as
notification API authentication.

Set `TELEGRAM_BRIDGE_NOTIFICATION_CHAT_IDS` to a comma-separated list of verified
negative Bot API-style group IDs. `chat:123` maps to `-123`; `channel:123` maps to
`-1000000000123`, following the [Telegram ID specification](https://core.telegram.org/api/bots/ids).
These are only a numeric representation; delivery uses the personal account.
Resolve the actual group with `telegram_list_dialogs` first. For supergroups,
the existing account-scoped access hash must be known to the bridge. The account
must belong to the group and be allowed to write there. A bot need not be present.

The API route is absent unless its dedicated token, group allowlist, store and
user API service are configured. Do not enable client calls until the reviewed
server has been deployed and live delivery verified. Existing MCP access alone
cannot enable or invoke notification writes.

## Request and response

Content type must be `application/json`. Example for a verified allowed group:

```json
{"chat":"channel:123","event_id":"project:release:1.2.3:45","text":"Project 1.2.3 (45) completed processing."}
```

`chat`, `event_id` and `text` are required; extra fields and trailing JSON are
rejected. The body limit is 32 KiB. An event ID is 1-128 ASCII letters/digits/dots/
underscores/colons/hyphens and begins with a letter or digit. Text must contain
1-4096 UTF-16 code units. Private users and broadcast channels are not supported.
There is no parse mode, attachment, edit, forwarding or deletion operation.

A confirmed send or exact repeat returns HTTP 200:

```json
{"status":"sent","message_id":42}
```

Missing/wrong credentials return 401; invalid JSON returns 400, wrong content
type 415 and unsupported methods 405. A disallowed destination, invalid event,
conflicting payload, unavailable user session or uncertain delivery returns 422
with a redacted error code. Verify destination, event identity and the receipt
before retrying. An inactive API returns 404. Responses are marked `no-store`.

The notification key permits arbitrary text and new event IDs within the allowed
groups; it does not prove a release happened or request human approval for each
message. Keep it scoped to trusted release automation. Removing a group or rotating
this key revokes its notification access without changing read-only MCP credentials.

## Durable delivery and recovery

`notification_receipts` stores destination, event ID, SHA-256 text digest, status,
creation time and message ID, but no message body or credentials. Its primary key
is `(chat_id, event_id)`. An atomic reservation prevents concurrent sends; confirmed
repeats return the same receipt. Reusing an event ID with different text is rejected.

The user API verifies active group membership before sending. Delivery calls
[messages.sendMessage](https://core.telegram.org/method/messages.sendMessage) with
a stable `random_id` derived from account/group/event and explicitly uses
`send_as=self` for supergroups. SQLite receipts additionally protect across callers
and restarts. Telegram errors and transport/session details are not returned to callers.

A timeout or crash after reservation leaves `pending`. It is never automatically
resent. Repeating the same API request is safe: it returns the sent receipt or
rejects pending delivery. Never invent a new event ID to bypass this protection.
Inspect the group before recovering a pending receipt. If its message exists,
update only that row to `sent` with the verified message ID. If confirmed absent
and no sender is active, remove only that row and repeat the event. Back up the
database before repairs. Do not clear all receipts or re-upload a release.

## Deployment verification and rollback

Run `scripts/check.sh`, then deploy reviewed code using the repository deployment
instructions. The existing migration-on-open path adds the receipts table; no new
runtime dependency, Telegram login, Fly infrastructure or polling loop is needed.

Verify `/healthz`, an authenticated MCP `tools/list` with exactly the original three
read-only tools, and 401 from the notification API with missing or MCP credentials.
Send one explicitly agreed message to the verified group with the notification key,
inspect its returned ID in Telegram, then repeat the same request and verify no new
post appears. Confirm a non-allowlisted destination is rejected without a send.
Automated tests cover these boundaries with fake RPCs, senders and temporary databases;
they do not establish live membership or production delivery.

Disable by removing `TELEGRAM_BRIDGE_NOTIFICATION_TOKEN` or clearing the allowlist
and restarting. Older code ignores the additive receipt table; retain it for rollback
and later reactivation. The existing session, monitor, watch rules and worker remain
unchanged. Read-only plugin metadata should be refreshed with `scripts/codex-plugin.sh
reload` after removing the earlier experimental MCP notification capability.
