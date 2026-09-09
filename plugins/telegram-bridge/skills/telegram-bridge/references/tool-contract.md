# telegram-bridge MCP tool contract

The server exposes three history/search tools and six media tools over Streamable HTTP at `https://telegram-bridge.fly.dev/mcp`. Authentication uses `Authorization: Bearer ...`; the plugin obtains that value from `TELEGRAM_BRIDGE_MCP_TOKEN`. Media tools appear after the new server is deployed. Start a new task after deployment to refresh MCP discovery.

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

Use this for surrounding conversational context or recent-chat summaries. Use message search for discovery.

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
- `media_kind`: stable attachment category. Text-only messages use `text`; other current values are `photo`, `document`, `voice`, `audio`, `video_note`, `animation`, `video`, `sticker`, `custom_emoji`, `web_page`, `location`, `live_location`, `venue`, `contact`, `poll`, `dice`, `game`, `invoice`, `story`, `giveaway`, `giveaway_results`, `paid_media`, `todo`, `video_stream`, `unsupported`, or the forward-compatible fallback `media`.
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

`telegram_analyze_image` additionally accepts `confirm_paid` and optional `model`
(currently `openai/gpt-4.1-mini`). One call produces scene description and OCR;
these are stored in distinct database columns. There is no text-model rewrite
of a voice transcript.

Both start tools return a durable job. `telegram_get_transcription` and
`telegram_get_image_analysis` additionally require its `job_id`. These result
reads never enqueue/retry or call a provider, even when processing is disabled.

Job fields: `job_id`, `operation`, `status`, `error_code`, `attempts`,
`provider_attempts`, `next_attempt_at` (retry wait only), `created_at`,
`updated_at`, `source`, `settings`, original-file `sha256`, measured
`duration_seconds`, and `result` after completion. Source metadata includes
`account_id`, `chat`, `message_id`, author, original UTC date, channel message
link when available, and optional reply/topic identifiers.

Result fields: `text` (speech), `description` (image scene), `ocr_text` (visible
image text), `languages`, `language_source`, optional `provider_request_id`,
and optional `cost_usd`. Fields not used by an operation are empty strings.
OpenRouter may omit language detection for STT; this returns an empty array
and `unavailable`, never a guessed value.

Statuses: `queued`, `preparing`, `submitting`, `retry_wait`, `completed`, `failed`,
`uncertain`. `preparing` downloads and normalizes/probes media. `submitting`
marks a committed budget reservation before the paid request. At most three
attempts are allowed; only Telegram transient errors and explicit HTTP 429
responses are retried. Network loss, ambiguous provider errors, and interrupted
submissions become `uncertain` and are never automatically recharged.

Defaults: one paid job at a time, 100 pending jobs, 1,000 retained jobs,
20 MiB originals, 600 seconds including normalized audio padding, 8 MiB image
uploads, 20 million pixels / 8192 per side, 200 MiB file cache, 24-hour original
retention, $1 daily and $5 lifetime reservation budgets. Transcripts and image
outputs persist with their deduplication records. Changed media, operation,
model, normalized hints, or administrator cache revision create a new key.
