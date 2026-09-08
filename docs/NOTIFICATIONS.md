# Notification service

The optional `telegram_send_notification` MCP tool sends plain text through the
existing bridge bot to operator-allowlisted Telegram groups. It reuses `/mcp` and
its bearer authentication. Enabling notifications grants holders of
`TELEGRAM_BRIDGE_MCP_TOKEN` this write capability for those groups. No new bot,
Telegram login, polling loop or public webhook is needed.

## Enable and verify

1. Resolve the target with `telegram_list_dialogs` and confirm its group type.
   Add the existing bridge bot to that group and allow it to send messages.
2. Configure `TELEGRAM_BRIDGE_NOTIFICATION_CHAT_IDS` on the server as a
   comma-separated list of negative Bot API group IDs. Leave it empty to keep
   MCP read-only. A basic `chat:123` maps to `-123`; a supergroup `channel:123`
   maps to `-1000000000123`, following the
   [Telegram ID specification](https://core.telegram.org/api/bots/ids).
   Broadcast channels and private-user destinations are not supported.
3. Run `scripts/check.sh` and deploy the reviewed code using the repository's
   deployment instructions. No new dependency or Fly infrastructure is needed.
   The additive SQLite table is created by the existing migration-on-open path.
4. Check `/healthz`, confirm an unauthenticated `/mcp` request returns 401, and
   initialize an authenticated MCP client. `tools/list` must show
   `telegram_send_notification` with `readOnlyHint=false`, `idempotentHint=true`
   and `destructiveHint=false`. Without a bot or allowlist, it must be absent.
5. Call the tool with an explicitly agreed test message and a unique event ID.
   Inspect the returned `message_id` in the intended group. Repeat the identical
   request and verify the same ID is returned without a second post. Verify a
   non-allowlisted group is rejected without sending.
6. Reload the Codex plugin after plugin/skill changes with
   `scripts/codex-plugin.sh reload`, and start a new task to pick up the schema.
   Existing tasks do not dynamically acquire new tools.

Example input, only after resolving and allowing this actual supergroup:

```json
{
  "chat": "channel:123",
  "event_id": "project:release:1.2.3:45",
  "text": "Project 1.2.3 (45) completed processing."
}
```

`chat`, `event_id` and `text` are required; additional fields are rejected.
An event ID has 1-128 ASCII letters/digits/dots/underscores/colons/hyphens and
starts with a letter or digit. Text must contain 1-4096 UTF-16 code units.
There is no parse mode, forwarding, attachment upload, edit or deletion tool.
The dev MCP proxy admits this specific idempotent write tool and continues
rejecting unknown write tools.

## Durable delivery

`notification_receipts` stores the destination, event ID, SHA-256 text digest,
status, creation time and returned message ID. Message bodies and bot credentials
are not stored in this table. Its primary key is `(chat_id, event_id)`.
Receipts are retained to keep deduplication valid across restarts and callers.

A `getChat` preflight verifies that the bot sees the exact destination as a group
or supergroup. Failures before reservation are retryable. An atomic insertion
reserves the event before sending, so concurrent callers cannot both send it.
A confirmed response is stored as `sent`. An existing sent event returns its
receipt; a changed text under the same event ID is rejected.

Telegram [sendMessage](https://core.telegram.org/bots/api#sendmessage) has no
idempotency key. The notification client deliberately bypasses the polling bot's
retry wrapper and makes one bounded send attempt. Errors, response loss or a
process crash after reservation leave `pending`, which is treated as uncertain.
A pending event is not automatically retried. An HTTP caller can safely repeat
the same event, but must never invent a new ID to bypass pending protection.

For manual recovery, inspect the group first. If the message exists, update only
that `(chat_id, event_id)` row to `status='sent'` with the verified message ID.
If the message is confirmed absent and no sender remains active, delete only that
row, then repeat the same event. Do not clear the entire table. Perform any repair
against a backup and keep the original event identity. A failed notification
does not mean a release failed or needs re-uploading.

## Rollback and boundaries

Unset `TELEGRAM_BRIDGE_NOTIFICATION_CHAT_IDS` and restart the service to remove
the write tool. Older application versions ignore the additive receipts table;
retain the table during rollback. The existing user session, monitor, watch rules,
private-message archive and Codex worker do not change.

The feature is inactive until the bot/allowlist are configured and the reviewed
server is deployed. Client projects must document that activation dependency.
Tests use temporary databases and fake senders; they prove code behavior, not
membership in a live group or production delivery.
