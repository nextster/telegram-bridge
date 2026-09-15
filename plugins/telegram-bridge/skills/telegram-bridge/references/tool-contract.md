# telegram-bridge MCP tool contract

The server exposes three history/search tools and eight media tools over Streamable HTTP at `https://telegram-bridge.fly.dev/mcp`. Authentication uses `Authorization: Bearer ...`; the plugin obtains that value from `TELEGRAM_BRIDGE_MCP_TOKEN`, a personal MCP token created on the telegram-bridge dashboard. Tools act only on the account that owns the token. Media tools appear after the new server is deployed. Start a new task after deployment to refresh MCP discovery.

## `telegram_list_dialogs`

Find dialogs and resolve their stable chat keys before doing a chat-specific search.

Inputs:

- `query` (optional string): case-insensitive title or username filter.
- `limit` (optional integer): 1 through 100.

Use the returned `chat` key, such as `channel:123`, `chat:123`, or `user:123`, in later calls. Do not construct a key from a display title or a guessed numeric ID.

## `telegram_search_messages`

Search messages globally or within one resolved chat.

Inputs:

- `query` (required string): Telegram server-side search query.
- `chat` (optional string): stable key returned by a dialog or message result. Omit for global search.
- `limit` (optional integer): 1 through 100.
- `min_date` (optional string): inclusive lower bound in RFC3339 or `YYYY-MM-DD`.
- `max_date` (optional string): exclusive upper bound in RFC3339 or `YYYY-MM-DD`.
- `offset_id` (optional integer): paginate toward older messages.

For a local-day window, prefer explicit offsets. Example: search August 1 through August 4, 2026 with `min_date: 2026-08-01T00:00:00+04:00` and `max_date: 2026-08-05T00:00:00+04:00`.

For exhaustive coverage, repeat the same query and bounds with the oldest message ID from the previous page as `offset_id`. Stop when no older results are returned or the relevant range is exhausted. Deduplicate repeated pages by `(chat.key, message.id)`.

Telegram search is lexical. For products and marketplace research, make separate calls for a small set of useful aliases, for example exact model, brand plus model, transliteration, and a local-language category term.

## `telegram_get_history`

Read recent or older history from one resolved chat without a search query.

Inputs:

- `chat` (required string): stable key returned by a dialog or message result.
- `limit` (optional integer): 1 through 100.
- `offset_id` (optional integer): paginate toward older messages.
- `min_date` / `max_date` (optional strings): inclusive/exclusive date bounds in RFC3339 or `YYYY-MM-DD`.
- `process_media` (optional boolean, default false): recognize all supported media in this returned page and wait for results.
- `confirm_paid` (optional boolean): must be true for `process_media`, which also requires `min_date`.
- `audio` / `image` (optional objects): same shared model/hint settings as the batch tool below.
- `wait_seconds` (optional integer): 0..480, default 480 in processing mode.

Use this for surrounding conversational context or recent-chat summaries. Use message search for discovery.

Free history does not create jobs. Paid history returns `media.items` containing
cached jobs/results or individual errors, linked by chat/message ID. It never
replaces message captions with recognized text. `has_more` and `next_offset_id`
describe older pages within the bounds; use `offset_id: next_offset_id` until
`has_more` is false. Each page is at most 100 messages (default 30). Empty pages
can still have a continuation cursor when Telegram returns only service events.
If `max_date` is omitted in paid mode, the response pins it to the current second
boundary: reuse it for subsequent pages/retries so new messages do not widen the
scope. History is annotated non-read-only because of its optional paid mode;
the whole moving-window history call is not annotated idempotent, but its media
jobs are deduplicated with every other start path.

## Result fields and limits

Message results include:

- `id`: Telegram message ID.
- `chat`: stable peer metadata.
- `sender`: display metadata when known.
- `date`: original message timestamp in UTC.
- `edited_at`: latest known edit timestamp in UTC, omitted for messages with no edit date.
- `text`: message text or caption; it can be empty for media-only messages.
- `reply_to_id`: replied-to message ID, omitted when this is not a normal message reply. The target may be outside the retrieved page and can rarely belong to another peer.
- `topic_id`: classic forum topic root message ID, omitted outside those topics. Telegram's General topic may not be distinguishable from an ordinary message through this field.
- `media_kind`: stable attachment category. Text-only messages use `text`; other current values are `photo`, `image` (JPEG/PNG/WebP documents), `document`, `voice`, `audio`, `video_note`, `animation`, `video`, `sticker`, `custom_emoji`, `web_page`, `location`, `live_location`, `venue`, `contact`, `poll`, `dice`, `game`, `invoice`, `story`, `giveaway`, `giveaway_results`, `paid_media`, `todo`, `video_stream`, `unsupported`, or the forward-compatible fallback `media`.
- `outgoing`: true for messages sent by the authenticated account; omitted when false.

All optional fields are absent rather than populated with placeholder zero values. Message results do not include a ready-made Telegram message URL, but a link can be derived for channel peers:

- If `chat.type` is `channel` and `chat.username` is non-empty, use `https://t.me/<chat.username>/<message.id>`.
- If `chat.type` is `channel` and `chat.username` is empty, use `https://t.me/c/<chat.id>/<message.id>`. This form is member-only and may not open for an account that cannot access that chat.
- Do not derive a message link for `chat.type` values `chat` or `user`; the returned metadata is insufficient for a reliable per-message link.

Copy all path components exactly from the tool result. Do not derive them from titles, sender names, or guessed peer IDs.

The MCP surface cannot send, edit, forward, or delete messages. It does not expose secret chats or the bot's deleted-message snapshot archive.

## Media tools

All media inputs require `chat` (the exact returned chat key) and `message_id`
(a positive message ID). They do not accept arbitrary URLs, filenames, paths,
Telegram session files, file references, or provider keys.

`telegram_get_attachment` returns the attachment's `kind`, `mime_type`,
`size_bytes`, Telegram-declared `duration_seconds`, `supported`, fingerprint,
and source metadata. This is free of AI calls. Supported media: voice, video
notes, photos, and JPEG/PNG/WebP image documents. Ordinary videos, audio files,
stickers, ephemeral attachments, and other documents are not processed.

`telegram_download_attachment` returns `file_id`, `download_url`, `sha256`,
`size_bytes`, `mime_type`, `expires_at`, and `source`. GET/HEAD of the URL requires
the same bearer token as MCP; there are no public/signed-token links. This
caches the unmodified Telegram file without paying an AI provider.

`telegram_transcribe_media` additionally accepts:

- `confirm_paid` (required boolean): must be `true` after an explicit user request.
- `model` (optional): currently `openai/gpt-transcribe`, via OpenRouter.
- `keywords` (optional string array): at most 16, 80 UTF-8 bytes each, 512 bytes total. No control characters, angle brackets, or line breaks.
- `languages` (optional string array): at most four language hints, e.g. `ru`, `en`. The provider validates supported codes.

`telegram_analyze_image` additionally accepts `confirm_paid`, optional `model`
(currently `openai/gpt-4.1-mini`) and, when exposed by the live schema,
`description_language`: an ISO code or `source` (dominant visible-text language,
English when unknown). Omission preserves the legacy prompt/cache. Changing the
option creates a distinct paid job; it does not rewrite an existing result.
One call produces scene description and OCR;
these are stored in distinct database columns. There is no text-model rewrite
of a voice transcript.

Both start tools return a durable job. `telegram_get_transcription` and
`telegram_get_image_analysis` additionally require its `job_id`. These result
reads never enqueue/retry or call a provider, even when processing is disabled.

Job fields: `job_id`, `operation`, `status`, `error_code`, `attempts`,
`provider_attempts`, `next_attempt_at` (retry or daily budget wait), `created_at`,
`updated_at`, `source`, `settings`, original-file `sha256`, measured
`duration_seconds`, and `result` after completion. Source metadata includes
`account_id`, `chat`, `message_id`, author, original UTC date, channel message
link when available, and optional reply/topic identifiers.

Result fields: `text` (speech), `description` (image scene), `ocr_text` (visible
image text), `language`, `languages`, `language_source`, optional `provider_request_id`,
and optional `cost_usd`. Fields not used by an operation are omitted; old servers
return empty strings instead. Valid empty speech/OCR fields remain present.
`language` is null unless exactly one provider-detected code is available.
OpenRouter may omit language detection for STT; this returns null, an empty array
and `unavailable`, never a guessed value or a language hint.

Statuses: `queued`, `preparing`, `submitting`, `retry_wait`, `completed`, `failed`,
`uncertain`, `budget_wait`, `budget_blocked`. `preparing` downloads and normalizes/probes media. `submitting`
marks a committed budget reservation before the paid request. At most three
attempts are allowed; only Telegram transient errors and explicit HTTP 429
responses are retried. Network loss, ambiguous provider errors, and interrupted
submissions become `uncertain` and are never automatically recharged.
Unusable HTTP-200 responses also require reconciliation. Optional `job.provider`
retains request ID, HTTP status, finish/failure reason, received byte count and
known cost; never raw or partial output. Old errors may have no such metadata.

Budget states do not consume failure attempts. `job.budget` reports scope
(`daily`, `total`, `request`), required/used/limit amounts in USD millionths.
Daily waits resume on released reservations or UTC midnight; total/request
blocks have no automatic daily reset. Both remain unsettled in batch responses.
Do not reset old failed jobs or submit changed settings to evade these states.

Defaults: three provider requests at a time (configurable 1..3), serial Telegram
downloads/FFmpeg, 100 pending jobs, 1,000 retained jobs,
20 MiB originals, 600 seconds including normalized audio padding, 8 MiB image
uploads, 20 million pixels / 8192 per side, 200 MiB file cache, 24-hour original
retention, $1 daily and $5 lifetime budgets (known costs plus unresolved reserves). Transcripts and image
outputs persist with their deduplication records. Changed media, operation,
model, normalized hints, or administrator cache revision create a new key.

## Batch processing and waiting

`telegram_process_media_batch` inputs:

- `items` (required array, 1..100): `{chat, message_id}` references. Attachment kinds are resolved by the server; duplicates share a job while preserving input order.
- `confirm_paid` (required boolean): true only for an explicitly authorized batch scope.
- `audio` (optional object): `model`, `keywords`, `languages` with the same bounds as single transcription. Reuse identical settings to reuse the cache.
- `image` (optional object): `model` and `description_language`; speech hints are rejected. Omit the language option when reusing legacy jobs.
- `wait_seconds` (optional integer, 0..480): default 480, zero only enqueues.

`telegram_get_media_batch` inputs: `items` (1..100 `{chat, message_id, job_id}`
references), optional `wait_seconds` (0..480, default zero). It only reads/waits;
no new jobs or paid requests are started.

Both return `items` with original `chat`, `message_id`, and either `job` or
`error_code`. Invalid overall input fails before queuing; unsupported/inaccessible
items do not hide other results. `settled` means no jobs remain pending, including
terminal failures; `all_succeeded` means every item completed successfully;
`timed_out` means the wait expired or was canceled while jobs were still pending.
Jobs and results persist independently of MCP request lifetimes. Partial batches,
overlapping histories, single calls, and concurrent retries reuse the same unique
job key and atomic claim. An HTTP 429 persists a shared cooldown across the worker
pool. Cost reservations remain atomic/shared, with no per-worker budget increase.

Paid history additionally returns `skipped_media` for non-text attachments outside
the supported voice/video-note/photo/image kinds. Each entry has `chat`,
`message_id`, `kind`, `reason: unsupported_attachment`. Ordinary videos remain
unsupported; batch success never establishes that skipped media were recognized.
