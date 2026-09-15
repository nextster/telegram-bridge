# Cloud media recognition

The existing `telegram-bridge serve` process downloads Telegram attachments
through its live authorized gotd client. One background worker pool processes
explicitly queued jobs on Fly, with up to three concurrent provider requests.
SQLite and originals live on the existing `/data`
volume. No Mac process, Telegram Desktop, second Telegram session, or local
inference model is involved. FFmpeg performs only media probing/conversion.

## Provider contract

Provider documentation and the public model/endpoint catalogs were checked on
2026-09-09. Recognition uses **OpenRouter** with `OPENROUTER_API_KEY`, never the
direct OpenAI endpoint:

- Speech: `openai/gpt-transcribe`, `POST https://openrouter.ai/api/v1/audio/transcriptions`.
- Images: `openai/gpt-4.1-mini`, `POST https://openrouter.ai/api/v1/chat/completions`.

The [OpenRouter STT API](https://openrouter.ai/docs/guides/overview/multimodal/stt)
accepts base64 JSON `input_audio`. The bridge sends normalized MP3, JSON output,
and OpenAI-specific prompt/keyword/language hints under
`provider.options.openai`. Top-level multipart `prompt` is documented as ignored,
so the bridge does not use it. Routing preferences are not applied to STT;
the checked GPT-Transcribe endpoint tag is `openai`.

[OpenAI's file transcription guide](https://developers.openai.com/api/docs/guides/speech-to-text)
documents a 25 MB cap and MP3/MP4/MPEG/MPGA/M4A/WAV/WebM input. The
[API reference](https://developers.openai.com/api/reference/python/resources/audio/subresources/transcriptions/methods/create)
also lists OGG and FLAC. We normalize voice OGG and video-note audio to MP3
instead of depending on format differences between routing layers. GPT-Transcribe
uses plural `languages`, plus `keywords` and `prompt`. The bridge does not request
Whisper-only timestamp output or run a summarization/rewrite step.

The [OpenRouter model page](https://openrouter.ai/openai/gpt-transcribe) listed
$0.0045/minute. The bridge conservatively reserves $0.006/minute, rounded up to a
whole second. Model availability, prices, hint forwarding, and actual language
fields must still be checked in the authorized cloud smoke test. OpenRouter's
normalized STT response may omit detected languages: the result then explicitly
returns `language: null`, `languages: []`, `language_source: unavailable`.
`language` is set only when exactly one provider-detected code is available.
Hints are never relabeled as detections. No text-based guess, second AI call,
or change to the existing STT request is made. OpenRouter's documented JSON STT
response promises text/usage, not detected language; generic `verbose_json`
examples for other models do not establish support for `openai/gpt-transcribe`.

Images use private base64 data, following
[OpenRouter image inputs](https://openrouter.ai/docs/guides/overview/multimodal/image-understanding),
with a strict JSON schema and `require_parameters: true` as described in
[structured outputs](https://openrouter.ai/docs/guides/features/structured-outputs).
One response contains `description`, `ocr_text`, and `languages`. Description and
OCR are stored in **separate SQLite columns** (`image_description`, `image_text`),
not concatenated into one text field. Speech uses a third column, `transcript`.
Truncated, refused, malformed, or schema-incomplete responses are not reported
as completed results. Image requests have 8192 output tokens and reserve $0.02.

An explicit image setting `description_language: "ru"` (ISO code), or `"source"`
(dominant language of visible text; English if unknown), controls only the
description. OCR remains verbatim in the source language. Omission preserves
the exact legacy prompt and cache key; its description language is unspecified.
The option and its conditional `image-description-language-v1` prompt revision
participate in the cache key. Do not add it to existing jobs merely to retry.
Cached descriptions are never silently translated or invalidated.

## MCP workflow

1. Resolve a chat with existing Bridge tools; read its messages and media kinds.
2. Use `telegram_get_attachment` with the exact `chat` and `message_id`.
3. Optionally use `telegram_download_attachment` to cache/download the original.
4. Only following an explicit processing request, call `telegram_transcribe_media`
   or `telegram_analyze_image` with `confirm_paid: true`.
5. Poll the matching result tool with `chat`, `message_id`, and `job_id`.

Example speech arguments:

```json
{
  "chat": "channel:123",
  "message_id": 456,
  "confirm_paid": true,
  "keywords": ["Realize", "тема", "подтема", "таймлайн"],
  "languages": ["ru", "en"]
}
```

The IDs above are illustrative; only use real IDs returned by Bridge. Hints are
bounded vocabulary, never inferred requirements or surrounding chat contents.
The result retains the full source wording, author, UTC date, original IDs,
available message URL, `reply_to_id`, optional `reply_to_chat`, and `topic_id`.
Legacy groups and private dialogs have no invented per-message URL. Telegram
photos are the largest downloadable Telegram representation; Telegram may have
compressed the image before Bridge receives it.

The full MCP schema and status contract is in the plugin's
[tool contract](../plugins/telegram-bridge/skills/telegram-bridge/references/tool-contract.md).

### Batch and media-aware history

`telegram_process_media_batch` accepts `items: [{chat, message_id}, ...]` (1..100),
`confirm_paid: true`, optional `audio`/`image` settings, and `wait_seconds`.
Voice, video notes, and image kinds are selected from authenticated Telegram
metadata, never a caller URL or file path. Repeated items preserve input order
but share one job. One bad/unsupported attachment gets an item-level error and
does not hide the remaining results. Invalid references/settings or a batch
larger than 100 are rejected before anything is queued.

`telegram_get_history` now accepts inclusive `min_date`, exclusive `max_date`,
and an explicit `process_media: true, confirm_paid: true` mode. This mode requires
`min_date`, queues every supported attachment **in the returned history page**,
and waits for their results. Ordinary history/search still never sends media to
OpenRouter. A processing request covering the chat/date scope authorizes the
page's media; separate calls/confirmation for each attachment are unnecessary.

For example (IDs are illustrative):

```json
{
  "chat": "channel:123",
  "min_date": "2026-09-01T00:00:00+04:00",
  "max_date": "2026-09-09T00:00:00+04:00",
  "limit": 100,
  "process_media": true,
  "confirm_paid": true,
  "audio": {"keywords": ["Realize", "тема", "подтема", "таймлайн"]}
}
```

History remains bounded to 100 messages per page. `has_more` and
`next_offset_id` describe older pages within the same date range, including
pages containing only service messages. Repeat with `offset_id: next_offset_id`
until `has_more: false` for complete range coverage. Reuse the returned
`max_date`; when omitted on the first paid request it is pinned to the current
second boundary. Otherwise a later read may legitimately include new messages.
Media results are in `media.items`, each linked by `chat`/`message_id` and carrying
the complete job/source metadata. Message captions are not replaced by transcripts.
Image documents now have `media_kind: image`; stickers and unsupported formats
remain excluded from recognition.

Batch/history waiting defaults to 480 seconds, configurable per call from 0 to
480. Zero queues without waiting. `telegram_get_media_batch` accepts
`items: [{chat, message_id, job_id}, ...]` and optionally waits without enqueuing
anything (default wait zero). Batch output preserves per-item job/error details:
`settled` means every item is terminal, `all_succeeded` means every item completed
successfully, and `timed_out` means the wait ended with pending work. Terminal
failures/uncertain submissions do not cause infinite waits or imply success.
`settled` applies only to those items, not to unread history pages.

HTTP/client timeout, disconnection, or cancellation stops only the wait. Queued
jobs remain on Fly. Resume via the batch result tool or repeat the same request
with the same bounds/settings; neither creates another paid job. Single starts,
overlapping batches, and history share the same database uniqueness constraint
and atomic job claim. No separate Telegram client or request-owned worker is
created. A canceled batch during metadata lookup may have queued only some items;
repeating it queues the remainder while reusing those already saved.

## Durability and duplicate charges

`media_jobs` holds durable identity, state, options, and results. `media_charges`
holds original per-submission reservations; `media_charge_settlements` holds
verified costs for those attempts, including zero. `media_files` tracks expiring originals. These tables
are additive migrations; existing Telegram/session tables are untouched.

Cache identity includes authorized account, chat/message ID, immutable Telegram
document/photo identity, operation, model, normalized hints, pipeline version,
and administrator cache revision. A replacement attachment gets a new identity.
Caption edits, author renames, and refreshed Telegram file references do not
invalidate recognition. Each downloaded original also has a verified SHA-256.
Changes between enqueue and download stop the old job as `source_changed`.

The same request returns its existing job/result, including terminal failures.
There is no automatic force-reprocess flag. The model allowlist is deliberately
restricted to the two verified models. Future model support requires review of
parameters and budgets; model IDs already participate in the cache key. Bump
`TELEGRAM_BRIDGE_MEDIA_CACHE_REVISION` deliberately after an alias/prompt/pipeline
change that requires fresh results, never as an automatic retry mechanism.

| Status | Meaning |
| --- | --- |
| `queued` | Explicit request saved; waiting for the account and worker |
| `preparing` | Downloading, probing, or converting on Fly |
| `submitting` | Budget and submission intent committed before calling OpenRouter |
| `retry_wait` | Safe transient failure; next attempt time is returned |
| `budget_wait` | No submission; waits for released reservations or next UTC day |
| `budget_blocked` | No submission; lifetime limit or single request exceeds a cap; no daily automatic reset |
| `completed` | Full validated result committed |
| `failed` | Unsupported media, limits, rejected request, or exhausted retries |
| `uncertain` | Provider may have charged; result was not safely obtained |

There are at most three failure attempts per job; budget deferrals do not consume
an attempt. Both budget states include `budget` with scope, required reservation,
used amounts and limits in USD millionths; daily waits include `next_attempt_at`.
They are not completed/failed: batches have `settled: false` and may time out.
Internally both use `retry_wait` plus `budget_exceeded`, preserving the DB CHECK
constraint and old-binary compatibility. Blocked jobs remain within queue caps.

Telegram transient errors and explicit
OpenRouter HTTP 429 rejections use bounded attempts and respect retry/flood-wait
delays. Access errors, insufficient credits, and malformed requests fail without
automatic retries. Network errors, ambiguous HTTP errors, and lost submission
responses become `uncertain`. Unusable HTTP-200 outputs (truncated, refused or
invalid structured image output) also become `uncertain`, not retryable failures.
Historical paid image validation failures are projected as `uncertain` on reads
without rewriting or restarting them. Both statuses were already terminal to
the old worker; absence of an automatic retry was deliberate, not a missed retry.

An unsuccessful provider attempt exposes optional `job.provider` metadata:
request ID, HTTP status, allowlisted finish/failure reason, received byte count
and validated cost when available. It never exposes partial text or raw bodies.
`provider_output_incomplete` means a parsed completion did not finish with `stop`;
`provider_response_incomplete` means a body read error or the 1 MiB envelope cap.
`failure_reason` distinguishes those causes for new attempts. Known costs are
settled even when the recognition output is unusable.

An OpenRouter 429 persists a shared cooldown: other workers stop claiming new
jobs and already prepared jobs wait before submission. Requests already in flight
may finish. This follows the provider's [retry/backoff guidance](https://openrouter.ai/docs/api/reference/limits)
without switching models, adding fallback calls, or increasing the spending cap.

Restart recovery requeues interrupted preparation (unless attempts are exhausted)
and marks interrupted submission `uncertain`. Completed results survive restart.
No documented cross-provider STT idempotency contract is assumed. This avoids
automatically paying twice, at the cost of operator reconciliation after an
ambiguous response. Do not reset an uncertain job or change hints to bypass it.
Use the saved provider request ID when available to reconcile provider records.

## Limits and retention

| Setting | Default / hard boundary |
| --- | --- |
| `TELEGRAM_BRIDGE_MEDIA_CONCURRENCY` | 3 provider requests / configurable 1..3 |
| Download and FFmpeg concurrency | One preparation at a time |
| Batch and history wait | 480 seconds / configurable 0..480 per call |
| Queue | 100 active jobs; 1000 retained job records |
| `TELEGRAM_BRIDGE_MEDIA_MAX_BYTES` | 20 MiB / at most 24,000,000 bytes |
| `TELEGRAM_BRIDGE_MEDIA_MAX_SECONDS` | 600 seconds / at most 600, including MP3 padding |
| Image upload | At most 8 MiB, 20 million pixels, 8192 pixels per side |
| `TELEGRAM_BRIDGE_MEDIA_MAX_DISK_BYTES` | 200 MiB / at most 1 GiB |
| `TELEGRAM_BRIDGE_MEDIA_RETENTION_HOURS` | 24 hours / at most 168 |
| `TELEGRAM_BRIDGE_MEDIA_DAILY_BUDGET_MICROUSD` | 1,000,000 ($1), UTC day |
| `TELEGRAM_BRIDGE_MEDIA_TOTAL_BUDGET_MICROUSD` | 5,000,000 ($5), lifetime; hard config cap $100 |

The file quota reserves room for normalization. Downloads are serialized, and
both Telegram-declared and actually written sizes are bounded. FFprobe and
FFmpeg have timeouts, restricted demuxers/protocols, one encoder thread, and a
hard duration bound. Video notes contribute only their audio stream to STT.
The worker pool overlaps cloud requests while keeping local preparation serial;
Fly stays at 512 MB, with no extra Machines. It does not add inference requests
for the same source/settings or change models, token limits, or reservations.

Budget reservations are committed atomically immediately before paid calls,
not at enqueue. Effective usage sums each attempt's verified cost, or its full
reservation when cost is unknown. Settlement can reduce a reservation or record
a higher actual cost. Amounts round upward to USD millionths; zero is valid.
The daily window is UTC and uses the submission's original day, including when
settlement arrives the next day. Lifetime usage includes all days and attempts.
An actual price above the reserve is accounted for but cannot be prevented after
submission; these caps do not guarantee against future provider price changes.

Startup backfills only the last attempt of completed legacy jobs with validated
stored `cost_usd`. Unknown prior attempts and incomplete responses retain their
reserves. Original reservation rows remain intact for audit/rollback. Settlement
wakes existing budget waiters; daily exhaustion otherwise waits until UTC
midnight. Historical `failed/budget_exceeded` jobs are not automatically requeued.
Neither free result reads nor reconciliation create new paid jobs. Limits, key,
models, concurrency, and cache settings are unchanged by this accounting fix.

## Result shape and unsupported attachments

Speech results contain `text` (including a valid empty string), not `description`
or `ocr_text`. Image results contain `description` and `ocr_text` (including
empty strings), not `text`. Common language/cost/request metadata remains shared;
storage already used separate columns. Existing cached results get this
operation-specific projection without inference or cache invalidation.

Paid history returns `skipped_media` with chat/message ID, kind and
`reason: unsupported_attachment` for attachment kinds outside voice, video note,
photo and supported image document (text and link previews are excluded). These are not counted as successfully
recognized; `media.all_succeeded` covers only selected supported media.
Ordinary history remains free and unchanged. Direct batch calls still return
per-item `unsupported_attachment` errors.

Ordinary videos, animated files, generic audio documents, stickers and arbitrary
documents are not enabled by this change. Supporting ordinary video audio would
require explicit Telegram video classification, refreshed download identity,
allowlisted demuxing and a verified audio stream, actual duration <=600 seconds,
original size <=20 MiB, normalized upload <=8 MiB, and the same FFmpeg/download
serialization and budgets. It must be a separately authorized extension of
paid-history scope, with silent/video-only and malformed-container tests.

Originals expire after the configured TTL. Download access stops at expiry even
before deletion. A sweep runs every 15 minutes and before new downloads; startup
also removes abandoned temporary/orphan files. Normalization files are deleted
as soon as preparation finishes or fails. Expired originals may be downloaded
again without repeating recognition.

Recognition results, identities, and budget records are retained on the private
volume, capped at 1000 jobs. They are not silently pruned, which preserves
deduplication. Reaching the record limit fails closed and requires a deliberate
retention/export decision. Deleting database state can remove deduplication and
must not be treated as routine cache cleanup.

All MCP calls and every original-file GET/HEAD require the caller's OAuth access
token or personal MCP token. Jobs, batches and cached originals belong to one
account and are never returned to another.
Files have private filesystem permissions and `Cache-Control: private, no-store`.
No arbitrary URL/path input, public directory listing, token-in-URL link, or user
filename is accepted. Service errors expose stable codes; request bodies, media,
keys, transcripts, OCR, and descriptions are not written to operational logs.

## Deployment after authorization

This change is not a code-only redeploy: both Dockerfiles install FFmpeg and the
Fly memory setting becomes 512 MB. Use the regular Dockerfile/deploy path.

1. Record the current deployed image and Fly configuration for rollback. Verify
   exactly one `serve` Machine owns the existing Telegram session volume. Do not
   start a local `serve` using a copy of that database.
2. Check available volume space and preserve an application-consistent backup
   of the SQLite database/session using the existing single-writer maintenance
   procedure. Do not copy only the main SQLite file while ignoring a live WAL.
3. Import a dedicated, spending-capped `OPENROUTER_API_KEY` into Fly Secrets from
   secure stdin. Never put its value in a command argument, Git, or logs:

   ```sh
   fly secrets import --stage --app telegram-bridge
   ```

   Supply `OPENROUTER_API_KEY=...` through the approved secret-manager/stdin flow.
   This PR leaves `TELEGRAM_BRIDGE_MEDIA_ENABLED=false` in both Fly configs.
4. Build/deploy to the existing single Machine, avoiding an extra HA instance:

   ```sh
   fly deploy --app telegram-bridge --config fly.toml --ha=false --strategy immediate
   ```

5. Verify `/healthz`, MCP authorization, all eleven discovered tools, original
   download authorization, and `ffmpeg`/`ffprobe` in the running image. Set
   `TELEGRAM_BRIDGE_MEDIA_ENABLED=true` through an approved Fly config/secret
   update only when the paid smoke test is authorized. Restart/new MCP task
   discovery is required; an existing task does not acquire new schemas.
   If this flag is stored in Fly Secrets, it overrides the value in `fly.toml`.
   Apply staged secrets to the existing Machine before testing. Verify the live
   OpenRouter key with `GET /api/v1/key`, including its spending cap, before the
   first recognition call. Never log its value or copy surrounding UI text.
6. For the requested smoke test, resolve an actual chat whose title starts with
   `<prefix>`, retrieve one voice message, download its original, submit one explicit
   transcription, and poll the full text through MCP. Verify IDs, author/date,
   reply link, measured duration, provider/model and language provenance. Repeat
   the same request and verify the job ID and provider-attempt count do not change.
   Exercise concurrent single/batch/history requests for that same source and
   settings; check that they reuse the same result. Then test a small mixed-media
   batch and a date-bounded history page with `process_media: true`, including
   duplicate calls and bounded waiting, within the separately approved scope.
7. After that successful check, enumerate the authorized `<prefix>` chats, paginate
   their histories, deduplicate `(chat.key, message.id)`, and queue discovered
   `voice`/`video_note` messages within the configured budget. Persist the inventory
   and outputs as private artifacts with the original source metadata; record
   unsupported/limited/failed/uncertain items. Do not claim exhaustive coverage
   until dialog discovery and history pagination are exhausted. Images are
   processed only within an explicitly requested image scope, not automatically
   as a side effect of this voice batch.

Once queued, jobs run on Fly without any local process. Agent-driven discovery/export can
be resumed separately using the durable job IDs. The single-voice smoke test and
the `<prefix>` batch are separate live checks, not covered by local tests.

## Rollback

Disable new paid processing with `TELEGRAM_BRIDGE_MEDIA_ENABLED=false`; already
committed uncertain submissions must still be reconciled rather than replayed.
When enabled through Fly Secrets, stage the disabling value there as well:

```sh
fly secrets set TELEGRAM_BRIDGE_MEDIA_ENABLED=false --stage --app telegram-bridge
```

Apply it with the existing-Machine deployment below; staging alone does not change
the running process. Merely changing `fly.toml` cannot override an enabled secret.
Deploy the recorded previous image with the recorded prior configuration and
`--ha=false --strategy immediate`. Keep the existing volume, session, and new
SQLite tables; the previous binary ignores the additive media tables. Do not
restore an older database over newly committed job/charge records, delete the
session, or start a second session owner. A rollback to the old binary disables
media endpoints; cached files remain private on disk. After fixing the issue,
redeploying this version resumes safe queued work and preserves completed results.
Retain the dedicated key's spending cap when enabling again. A confirmed HTTP
401/403 rejection after a credential setup error can be reconciled by an operator
after the key is repaired and verified, preserving the original job identity,
attempt counts, and charge reservations. Never apply that procedure to uncertain
submissions, completed jobs, or errors whose billing outcome is unknown.

## Verification boundary

`scripts/check.sh` runs Go tests/vet, existing Python/script checks, plugin
validation, Fly config validation, and diff whitespace checks. FFmpeg is required;
CI installs it. Added tests use mock Telegram RPC, local HTTP provider fixtures,
temporary SQLite databases, generated audio/video, and synthetic images.

These checks cover authorization, bounded inputs/downloads, hints, rate limits,
provider failures and redaction, no redirects, structured output, separate image
storage, deduplication, budgets, account isolation, restart recovery, and cleanup.
Concurrent tests cover the three-request ceiling, shared budgets/cooldown,
duplicate batches/single/history calls, canceled waiters, pending/failed items,
and date/pagination boundaries. They do not benchmark live provider throughput.
They do **not** establish deployed availability, live hint/language forwarding,
real recognition accuracy, provider charges, or the completed `<prefix>` batch.
