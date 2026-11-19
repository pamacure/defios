# An agent's talk says when each message was written

An agent read the talk as `[seq] <display name>[/@handle]: <text>`, in its
turn and from `talk_read`. Neither said when anything was said, so an agent
could not tell how stale a message was or how far apart two were.

Both now carry the time a message was written. That is the envelope's
`created_at`, so it comes from the sender's clock, not the log's.

**A turn's lines** become `[<seq> <YYYY-MM-DD HH:MM>] <speaker>: <text>`, in
UTC to the minute, with `queued` in place of the seq. Gap lines are
unchanged. Earlier turns are replayed as they were recorded, so this costs
no cache miss: only new turns carry the time.

**`talk_read`** returns JSON Lines, one object per line:

- A message is `{"seq", "at", "from", "handle", "text"}`. `at` is Unix
  seconds, `from` the sender's display name, and `handle` is present only
  for an agent's message. A message with no seq yet has `"queued": true` in
  place of `seq`.
- A stretch this Node has pruned is one `{"pruned_from", "pruned_to"}` line.
- A page that does not reach the start of the talk ends with
  `{"earlier_before_seq": N}`.
- Text keeps its newlines as JSON escapes.

An empty result is still a plain note, `(No talk yet.)` or
`(No talk before seq N.)`, because there is no line to give.

## Considered options

- **The keys of `conversation log --json`** (`created_at`,
  `sender_display_name`) and an RFC 3339 time. These cost roughly 25–35
  tokens a message over the text line, against about half that for the short
  keys and Unix seconds. A `talk_read` result stays in the Transcript and is
  replayed on every later run.
- **Text lines for `talk_read` too, with the turn's time added.** These are
  the cheapest, but an agent that passes fields on to other Tools reads JSON
  more reliably.
