# Cloud media recognition

The existing `telegram-bridge serve` process downloads Telegram attachments
through its live authorized gotd client. A single background consumer processes
explicitly queued jobs on Fly. SQLite and originals live on the existing `/data`
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
returns `languages: []`, `language_source: unavailable`. Hints are never relabeled
as detections.

Images use private base64 data, following
[OpenRouter image inputs](https://openrouter.ai/docs/guides/overview/multimodal/image-understanding),
with a strict JSON schema and `require_parameters: true` as described in
[structured outputs](https://openrouter.ai/docs/guides/features/structured-outputs).
One response contains `description`, `ocr_text`, and `languages`. Description and
OCR are stored in **separate SQLite columns** (`image_description`, `image_text`),
not concatenated into one text field. Speech uses a third column, `transcript`.
Truncated, refused, malformed, or schema-incomplete responses are not reported
as completed results. Image requests have 8192 output tokens and reserve $0.02.

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

## Durability and duplicate charges

`media_jobs` holds durable identity, state, options, and results. `media_charges`
holds budget reservations. `media_files` tracks expiring originals. These tables
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
| `completed` | Full validated result committed |
| `failed` | Unsupported media, limits, rejected request, or exhausted retries |
| `uncertain` | Provider may have charged; result was not safely obtained |

There are at most three attempts per job. Telegram transient errors and explicit
OpenRouter HTTP 429 rejections use bounded attempts and respect retry/flood-wait
delays. Access errors, insufficient credits, and malformed requests fail without
automatic retries. Network errors, ambiguous HTTP errors, and lost submission
responses become `uncertain`.

Restart recovery requeues interrupted preparation (unless attempts are exhausted)
and marks interrupted submission `uncertain`. Completed results survive restart.
No documented cross-provider STT idempotency contract is assumed. This avoids
automatically paying twice, at the cost of operator reconciliation after an
ambiguous response. Do not reset an uncertain job or change hints to bypass it.
Use the saved provider request ID when available to reconcile provider records.

## Limits and retention

| Setting | Default / hard boundary |
| --- | --- |
| Paid concurrency | One consumer inside `serve` |
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

Budget reservations are committed atomically before paid calls and retained for
failed/uncertain attempts. Reported costs larger than the reservation increase
the ledger; lower costs do not free it. This is a conservative application budget,
not a guarantee against future provider pricing changes. Use a dedicated capped
OpenRouter key for the service and recheck prices before increasing limits.

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

All MCP calls and every original-file GET/HEAD require `TELEGRAM_BRIDGE_MCP_TOKEN`.
Files have private filesystem permissions and `Cache-Control: private, no-store`.
No arbitrary URL/path input, public directory listing, token-in-URL link, or user
filename is accepted. Service errors expose stable codes; request bodies, media,
keys, transcripts, OCR, and descriptions are not written to operational logs.

## Deployment after authorization

This change is not a code-only redeploy: both Dockerfiles install FFmpeg and the
Fly memory setting becomes 512 MB. Use the regular Dockerfile/deploy path.

1. Record the current deployed image and Fly configuration for rollback. Verify
   exactly one `serve` Machine owns the existing Telegram session volume. Do not
   start a local `serve` or CLI login using a copy of that session.
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

5. Verify `/healthz`, MCP authorization, all nine discovered tools, original
   download authorization, and `ffmpeg`/`ffprobe` in the running image. Set
   `TELEGRAM_BRIDGE_MEDIA_ENABLED=true` through an approved Fly config/secret
   update only when the paid smoke test is authorized. Restart/new MCP task
   discovery is required; an existing task does not acquire new schemas.
6. For the requested smoke test, resolve an actual chat whose title starts with
   `<prefix>`, retrieve one voice message, download its original, submit one explicit
   transcription, and poll the full text through MCP. Verify IDs, author/date,
   reply link, measured duration, provider/model and language provenance. Repeat
   the same request and verify the job ID and provider-attempt count do not change.
7. After that successful check, enumerate the authorized `<prefix>` chats, paginate
   their histories, deduplicate `(chat.key, message.id)`, and queue discovered
   `voice`/`video_note` messages within the configured budget. Persist the inventory
   and outputs as private artifacts with the original source metadata; record
   unsupported/limited/failed/uncertain items. Do not claim exhaustive coverage
   until dialog discovery and history pagination are exhausted. Images are
   processed only within an explicitly requested image scope, not automatically
   as a side effect of this voice batch.

Once queued, jobs run on Fly with the Mac off. Agent-driven discovery/export can
be resumed separately using the durable job IDs. The single-voice smoke test and
the `<prefix>` batch remain outstanding until deployment/paid processing is authorized.

## Rollback

Disable new paid processing with `TELEGRAM_BRIDGE_MEDIA_ENABLED=false`; already
committed uncertain submissions must still be reconciled rather than replayed.
Deploy the recorded previous image with the recorded prior configuration and
`--ha=false --strategy immediate`. Keep the existing volume, session, and new
SQLite tables; the previous binary ignores the additive media tables. Do not
restore an older database over newly committed job/charge records, delete the
session, or start a second session owner. A rollback to the old binary disables
media endpoints; cached files remain private on disk. After fixing the issue,
redeploying this version resumes safe queued work and preserves completed results.

## Verification boundary

`scripts/check.sh` runs Go tests/vet, existing Python/script checks, plugin
validation, Fly config validation, and diff whitespace checks. FFmpeg is required;
CI installs it. Added tests use mock Telegram RPC, local HTTP provider fixtures,
temporary SQLite databases, generated audio/video, and synthetic images.

These checks cover authorization, bounded inputs/downloads, hints, rate limits,
provider failures and redaction, no redirects, structured output, separate image
storage, deduplication, budgets, account isolation, restart recovery, and cleanup.
They do **not** establish deployed availability, live hint/language forwarding,
real recognition accuracy, provider charges, or the completed `<prefix>` batch.
