# A Key alias starts on the log and then is the Node's

A Key had no name a person could remember, only its curve and public key. A
**Key alias** is a label in the top-level `alias` field of that Key's
Conversation Workspace `config.toml`, and with none the Key goes by
`curve:hex`. Whoever starts a keygen Session may propose an alias on
`session.start`, so every Node party to the Key begins with the same name
without being asked for one; from then on the alias is each Node's own, and
the log is not read for it again. Public-key prefix lookup is removed, so a Key
is named by `curve:hex`, its Conversation ID or its alias. This amends
[ADR 0013](0013-group-config-workspaces-and-named-secrets.md). This is
pre-release, so nothing is migrated.

## Considered options

- **An alias on the Node alone, set at each Node separately** — rejected:
  every Node of a Group would have to name each new Key by hand, and the
  usual outcome is that only one of them does.
- **A Group-wide alias kept in sync on the log** — rejected: any member could
  rename a Key under another Node's scripts and habits, and every other local
  label (Group, Network, Provider) already belongs to the Node that uses it.
- **Reading the proposal back whenever config.toml has no alias** — rejected:
  a cleared alias would return. config.toml alone decides, so the proposal is
  written once, when the Node first creates the Key's Workspace.
- **One table of aliases in the Group's `config.toml`** — rejected: it would
  make uniqueness structural, but an alias is a fact about one Key and belongs
  in the Workspace that is already that Key's. Duplicates from a hand-edit
  are caught where the alias is used instead.
- **Keeping prefix lookup and restricting what an alias may be** — rejected:
  `cafe` could be an alias on one Node and another Key's prefix on the next,
  so the same `sign propose --key cafe` would pick different Keys. Forbidding
  hex-looking aliases would have stopped that, at the cost of restricting names
  to protect a shortcut that an alias replaces.

## Consequences

- **What an alias may be.** 1 to 63 Unicode code points after NFC
  normalization, with no control characters and no leading or trailing
  whitespace. Compared case-insensitively, shown as typed. Unique per Group on
  a Node. The Group's `config.toml` refuses the field, and it is not inherited
  through the merge.
- **The cap keeps the name spaces apart.** The shortest other way to name a Key
  is the 64-character Conversation ID, so a query over 63 code points is never
  looked up as an alias, and no character needs banning to prevent a clash.
  Raising the cap past 63 would need a new rule.
- **A string that could mean two Keys is refused**, never guessed. Without
  prefixes that is only a duplicate alias made by hand-editing.
- **A broken `config.toml`** fails an alias lookup, naming the file, because
  the alias might be in it. `curve:hex` and the Conversation ID read no
  Workspace, and still work.
- **Proposal, then override.** `session start --alias` writes the proposal, on
  keygen only. `session accept --alias` overrides it for that Node alone. Each
  fails at once if the alias is invalid or taken here, and an alias waiting on
  a mint counts as taken. It waits in `serve`'s memory, because a restart
  already aborts Sessions this Node has accepted.
- **A proposal cannot block a keygen.** One that is invalid or taken on a Node
  leaves the Key with no alias there and a warning. A proposal comes from
  another Node, so an invalid one is treated as absent and never printed.
- **`curve:hex` stays visible.** Text output shows the alias with the
  reference beside it, because an alias is local and easily edited and a
  custody tool should not let it hide which Key this is. In `--json`, `key`
  remains `curve:hex`.
- **Job and Hook definitions do not take an alias.** They name the Workspace,
  whose name never changes, so renaming a Key cannot silently break one.
