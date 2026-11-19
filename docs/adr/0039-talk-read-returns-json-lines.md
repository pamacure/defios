# talk_read returns JSON Lines with timestamps

`talk_read` used to return the same lines as an agent's turn,
`[seq] <display name>[/@handle]: <text>`. That gave an agent no way to tell
when anything was said, so it could not reason about how stale a message is
or how far apart two were.

`talk_read` now returns one JSON object per line:

- A message is `{"seq", "created_at", "sender_display_name", "handle", "text"}`.
  `handle` is present only for an agent's message. A message with no seq yet
  has `"queued": true` in place of `seq`. `created_at` is RFC 3339 in UTC, to
  the second. It is the envelope's `created_at`, so it comes from the sender's
  clock, not the log's. The field names match `conversation log --json`.
- A stretch this Node has pruned is one `{"pruned_from", "pruned_to"}` line.
- A page that does not reach the start of the talk ends with
  `{"earlier_before_seq": N}`.
- Text keeps its newlines as JSON escapes. It no longer goes through
  `cosl.EscapeNewlines`.

The talk in an agent's turn keeps the text lines of ADR 0036. That talk is
prompt, not data the agent queries, and changing it would also change every
replayed Transcript's prefix.

An empty result is still a plain note, `(No talk yet.)` or
`(No talk before seq N.)`, because there is no line to give.
