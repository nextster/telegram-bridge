---
name: telegram-bridge
description: Search and inspect the authenticated Telegram account through the read-only telegram-bridge MCP tools. Use when the user asks to find messages, listings, links, products, conversations, or recent history in Telegram chats; search one named channel or many chats; constrain Telegram research by date; or summarize retrieved Telegram messages.
---

# Telegram Bridge

Use the telegram-bridge MCP server as the source of truth for Telegram content. Keep every operation read-only.

Read `references/tool-contract.md` before constructing nontrivial date-bounded or paginated calls.

## Resolve scope

- For a named person, group, or channel, call `telegram_list_dialogs` with a short distinctive query and use the returned stable `chat` key.
- Never invent a `user:`, `chat:`, or `channel:` identifier.
- If multiple dialogs match, compare title, username, and kind. Ask only when choosing incorrectly would materially change the result.
- Search globally only when the user requests cross-chat search or no single dialog is intended.

## Search messages

- Call `telegram_search_messages` with a focused query and a useful limit.
- Expand product names and concepts into a small set of high-signal spelling, language, and model aliases. Run separate searches instead of one keyword soup.
- Apply explicit RFC3339 bounds when local-day accuracy matters. For local dates use the user's UTC offset, for example `+04:00`; `max_date` is exclusive.
- Paginate with the oldest returned message ID as `offset_id` when the user asks for exhaustive or long-range coverage.
- Deduplicate by chat key plus message ID.
- Use `telegram_get_history` when the request is conversational context rather than keyword discovery.

## Reconstruct conversations

- Use `reply_to_id` to connect a reply to its parent when the parent is present in the retrieved page. Do not invent missing parent text; paginate for more context when it matters.
- Use `topic_id` to partition classic forum-group history before summarizing. Its absence does not prove that a message is outside Telegram's General topic.
- Use `media_kind` when building tables or summaries so photos, documents, voice messages, video notes, links, polls, and other attachments are not silently treated as empty text.
- Use `edited_at` to distinguish the original send time from the latest known edit time. Do not describe an edit as a deletion or a new message.
- A reply target can be outside the current page and, rarely, in another peer. If the returned page has no matching message ID, leave the edge unresolved instead of attaching it to an unrelated message.

## Report results

- Lead with the strongest findings. Include chat, timestamp, sender when useful, and a concise excerpt.
- For dialogue summaries, preserve reply relationships and topic boundaries when they materially change who or what a message answers.
- Include a clickable Telegram message link when it can be derived safely from the returned peer metadata:
  - For a channel or supergroup with a returned `username`, use `https://t.me/<username>/<message_id>`.
  - For a channel or supergroup without a username, use the member-only form `https://t.me/c/<chat.id>/<message_id>` and label it member-only.
  - Do not construct a message link for `chat:` legacy groups or `user:` private chats.
- State the searched chats, date window, aliases, and whether pagination was exhausted.
- Treat no matches as "nothing found in this scope," not proof that Telegram never contained it.
- Use only the exact username, numeric channel ID, and message ID returned by the MCP result. Never guess or normalize them.

## Boundaries

- Do not send, edit, forward, or delete Telegram messages.
- Do not claim access to secret chats, locally deleted-only history, or deleted-message snapshots. Deletion snapshots currently feed bot alerts and are not exposed through MCP.
- Do not use web search as a substitute for a requested Telegram search.
