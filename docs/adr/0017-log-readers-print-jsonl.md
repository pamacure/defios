# Log readers print JSONL under --json

Under `--json`, a verb that reads a log prints one compact JSON object per
row, one row per line (JSONL). These are `cosl log` and `conversation log`,
with or without `--full` or `--lean`. An empty log prints nothing. Every
other verb's `--json` is still one indented JSON document. That includes
`cosl decode`, which shows one row.

A log is an ordered run of rows, and its human output is already one line
per row. JSONL is the same shape as data. It is also how attached
`conversation send` and `conversation follow` already print their events,
so anything that prints rows over time now prints them the same way. A
later `log --follow` would just keep writing lines.

It suits the consumers too. `head`, `tail`, `grep` and `wc -l` work on
rows, and `jq` takes them with no `.[]`. `jq -s` turns them back into an
array. Plain `--lean` stays what goes into a prompt: `serve` hands an agent
that, and its `<display name>: <text>` lines cost a few tokens of framing
per entry where a JSON object's keys and quotes cost several times that.
`--lean --json` is for a program that builds a prompt, such as a runner
mapping entries to chat turns by `handle`, and JSONL lets it take them a
line at a time. A reader can also print
each row as it folds it rather than holding the whole log, though nothing
needs that yet.

This narrows `docs/spec/mvp.md`'s "`--json` on any verb": every verb still
takes `--json`, but a log reader's is JSONL. The arrays are dropped with no
compatibility flag or migration.

## Considered options

- **Keep the indented array.** Rejected: it is the only row stream not
  printed as lines, it cannot be printed until the log is read in full, and
  its indentation makes it the bulkiest form.
- **JSONL for every `--json` verb.** Rejected: `inspect`, `show` and
  `usage` return one document, and a list verb returns a small, bounded
  collection. Neither is a stream, and an array or object is the natural
  form for both.
- **An envelope** (`{"rows": [...]}`, room for a cursor or the live Recap's
  base). Rejected for now: nothing needs the metadata. If something does, it
  gets a typed first line or a verb of its own.
