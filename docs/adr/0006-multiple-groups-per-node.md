# Multiple Groups per Node: shared identity, one serve and one focused socket per Group

A Node may belong to many Groups at once, each on its own tailnet, presenting
the **same** cryptographic identity (auth keypair, encryption keypair, Node ID)
on all of them. Each Group runs its own `serve` process with its own
`tsnet.Server`, Raft, and COSL; a single Node-level socket means exactly one
Group is **focused** and live at a time, and switching focus stops one `serve`
and starts another. Supersedes the MVP "one Group per Node" constraint
([mvp.md §1.2](../spec/mvp.md), §10 non-goal). A Group may now also span
different parties, not only one operator's own devices.

## Considered options

- **One `serve` multiplexing every Group** (N `tsnet.Server`s and N Rafts in one
  process) — rejected: it shares a single failure domain across Groups and holds
  every Group's shares in one address space, whereas the activity of one Group
  must have no influence over its peers. A per-Group `serve` is also almost
  exactly today's `serve` pointed at one Group's subtree, so it is the smaller
  change.
- **A distinct cryptographic identity per Group** (its own auth keypair / Node
  ID) — rejected: that is many Nodes per host, not one Node in many Groups. A
  Node is one device holding one auth keypair; the Groups a host joins are the
  same operator's choice to join, so isolation is about networks, processes, and
  storage, not about minting an unlinkable identity per counterparty.
- **Per-Group sockets with concurrent serves** (every Group reachable at once) —
  rejected for now: the operator works one Group at a time, and a single shared
  socket yields one-live-serve-at-a-time for free (the second bind fails). The
  accepted cost is that an **unfocused Group is unreachable to its peers** — it
  cannot append, keygen, or receive Conversation messages until refocused, at
  which point Raft catch-up reconverges it.
- **Enforcing distinct tailnets per Group in code** — rejected: assumed and
  documented instead. Each Group has its own `tsnet.Server` and its own hostname
  regardless, and discovery is scoped to one tailnet, so an accidental shared
  headscale does not cross-wire two Groups.

## Consequences

- `node list` and formation are scoped to the focused Group's tailnet; a Node's
  Groups never share a discovery surface.
- A persisted **Current Group** pointer selects what `serve` serves and what
  read verbs default to; `--group <alias>` overrides reads, which need no
  `serve`.
- Storage is reorganized Group by Group; the layout and the Vault change are
  [ADR 0007](0007-group-scoped-storage-alias.md).
- `GLOSSARY.md` gains **Current Group**; **Group** and **Node** are generalized
  (a Group may span parties; a Node may belong to several Groups, presenting one
  identity in each).
