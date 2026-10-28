# CLI output style

Human (non-`--json`) output of the read verbs (`list`, `inspect`, `show`, `usage`, `log`) colors only what should draw the eye. Most text stays plain. The helper is `palette` in `internal/cli/style.go`; call it by role, never by color.

| Role | Code | Marks |
|---|---|---|
| `primary` | blue (34) | the identity of each record: aliases, IDs, key names, group / serve headings, log seq and author |
| `caution` | yellow (33) | in progress, may need someone: `open`, `waiting`, `signing`, `pending`, `unformed`, `stale`, serve `down` / not running, `no` |
| `concern` | red (31) | failed or terminal-bad: `aborted`, `declined`, `expired`, `error`, `session.abort`, `unknown(...)`, the stderr `defios:` prefix |
| `dim` | faint (2) | annotation: paths, timestamps, parentheticals, the `usage` detail column |

Status words go through `palette.status`, which holds the mapping above.

Rules:

- Basic ANSI codes only, so the terminal theme picks the shades. No bold, no green: healthy states and values are plain, and sizes have no thresholds.
- Color is on only when the stream is a TTY, `NO_COLOR` is unset, `TERM` is not `dumb`, and `--json` is not set.
- Plain stays plain: mutating verbs' confirmations, cobra help, passphrase prompts, `secret show` values.
- `tabwriter` counts escape codes toward cell widths. Align colored columns by hand, padding by the plain text's width (see `printUsage`).
