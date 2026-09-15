# HTTP notification API

`POST /notifications/v1/messages` sends plain text to a group as the token
owner's own Telegram account. It reuses that account's live gotd client, not a
bot or a second session. MCP remains read-only and has no notification tool.

## Authentication and activation

Every user sets this up for their own account on the dashboard:

1. Create a **Notifications** token. It is shown once; SQLite keeps only its
   SHA-256 hash. Keep it in the caller's secret store, never in Git,
   command-line arguments or logs.
2. Allow a group by its key (`channel:123`, `chat:123`) or Bot API group ID
   (`-1000000000123`, `-123`), following the
   [Telegram ID specification](https://core.telegram.org/api/bots/ids). The
   bridge checks with the user's own account that it can post there before
   saving. Resolve the group with `telegram_list_dialogs` first.

Requests require `Authorization: Bearer <notification-token>` over HTTPS. A
token resolves to exactly one user: it posts only to that user's allowed groups
and only as that user's account. MCP tokens, OAuth access tokens, cookies and
session files are not accepted, and notification tokens are not accepted by
MCP. The account must belong to the group and be allowed to write there; a bot
need not be present.

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
type 415 and unsupported methods 405. A group the token owner has not allowed,
invalid event, conflicting payload, disconnected account or uncertain delivery
returns 422 with a redacted error code. Verify destination, event identity and
the receipt before retrying. Responses are marked `no-store`.

The notification key permits arbitrary text and new event IDs within the allowed
groups; it does not prove a release happened or request human approval for each
message. Keep it scoped to trusted release automation. Removing a group or deleting
the token on the dashboard revokes its access without changing MCP credentials;
`/logout` in the bot deletes all of the user's tokens.

## Durable delivery and recovery

`notification_receipts` stores destination, event ID, SHA-256 text digest, status,
creation time and message ID, but no message body or credentials. Its primary key
is `(account_id, chat_id, event_id)`, so users never share or see each other's receipts. An atomic reservation prevents concurrent sends; confirmed
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
instructions. Migrations run when the database opens.

Verify `/healthz`, an authenticated MCP `tools/list` without a send tool, and 401
from the notification API with missing or MCP credentials. Send one explicitly
agreed message to an allowed group with a notification token, inspect its returned
ID in Telegram, then repeat the same request and verify no new post appears.
Confirm a group allowed only by another user is rejected without a send.
Automated tests cover these boundaries with fake RPCs, senders and temporary
databases; they do not establish live membership or production delivery.

A user disables their notifications by deleting their notification tokens or
groups on the dashboard.
