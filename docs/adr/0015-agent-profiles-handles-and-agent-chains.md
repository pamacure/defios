# Agent Profiles answer `@handle` on the Node that mentioned them

An **Agent Profile** is a Node-local profile directory holding a
`SYSTEM.md`, which a Group's `config.toml` binds to a **Handle** together with
the Model it runs on. A Conversation's `config.toml` (or the Group's, for
Group-level talk) includes Handles by name. When a person's `app.freeform`
mentions `@handle`, the serve of the Node that wrote it runs that agent: it
streams the reply to local watchers as it is generated and commits it once,
finished, as an `app.freeform` that names the Handle. No other Node acts on the
mention. An agent's reply may summon further agents, up to a depth the config
sets. This amends [ADR 0013](0013-group-config-workspaces-and-named-secrets.md):
`[agent] model` and `token_window` are gone, and agent markdown lives in the
Agent Profile, not in a Workspace. Neither change had shipped behaviour,
because nothing read them, so nothing is migrated.

## Considered options

- **Profiles defined per Group, with their markdown inside the Group
  Workspace.** Rejected: one Node may belong to several Groups, and an
  operator who wants the same `auditor` in `personal` and `custody-with-b`
  would have to copy it. Profile directories are Node-wide, and each Group
  binds its own Handles to them.
- **Agent markdown in the Conversation Workspace as well** (ADR 0013's
  `SOUL.md`). Rejected: an agent's persona would then be split across two
  places with merge rules between them. Workspaces hold settings only.
- **Every Node that defines a Handle answers a mention of it.** Rejected: N
  replies to one question, each billed to a different party's key, and
  nothing guarantees two Nodes' `@auditor` are the same agent. A **Handle
  owned by one Node**, recorded on the COSL, was rejected too: it would need
  coordination, and every mention would become a request for another party to
  spend their credential. Resolving a mention on the Node whose message
  carries it needs neither.
- **A new message type for agent replies** (`app.agent`). Rejected in favour
  of two fields on `AppFreeform`. An agent's reply is talk like any other: a
  Recap covers it and Pruning destroys it, and every reader of talk already
  reads `app.freeform`.
- **Streaming tokens onto the COSL.** Rejected: each row is a Raft commit
  replicated to every Node, and a Recap and Pruning would have to account for
  fragments. The stream is local to the running Node, and the log holds only
  finished replies.
- **Only an external runner** (a script or another agent harness that reads
  `conversation log --lean` and posts back). Rejected as the only path:
  streaming, cancel and retry belong where the provider connection is held,
  and the serve already holds Model configs and provider credentials. It is
  kept as a second path through `send --handle`.
- **Agents that cannot summon agents.** Rejected: delegation is useful. Loops
  are bounded by depth, not forbidden.
- **Committing a cancelled reply's partial text.** Rejected: a half-written
  answer on a permanent shared log is worse than none.

## Consequences

- **Config.** In the Group `config.toml` only:

  ```toml
  [agents.auditor]
  dir = "agents/auditor"  # optional; default agents/<handle>
  model = "claude"        # required: a Model alias
  ```

  The table key is the Handle, which follows the alias rule
  (`[a-z0-9][a-z0-9_-]*`, 1 to 64 characters). `user` is reserved.
  *Amended by [ADR 0016](0016-what-a-mention-is.md): a Handle is runs of
  `[a-z0-9]` joined by single `-` or `_`.* `dir`
  expands `~`, and a relative path resolves against the config directory
  (`~/.config/defios/`). `[agent]` holds `profiles = ["auditor", ...]` and
  `max_depth` (default 1). It appears in Group and Conversation configs and
  merges as before: a Conversation that sets `profiles` replaces the Group's
  list. A `profiles` entry that names no `[agents.*]` table is an error when
  the config is read. A Model alias the COSL does not hold, or a profile
  directory without `SYSTEM.md`, is an error when the agent is summoned.
- **Credentials are unchanged.** The profile's Model alias gives its provider
  on the COSL, and the key comes from the Conversation's merged
  `[providers.<provider>] api_key`, from the environment or the Group's Vault,
  under ADR 0013's endpoint rule. The same profile runs on each Group's own
  key.
- **Wire.** `AppFreeform` gains `handle` (field 3), empty when a person wrote
  the message and shown as `user`, and `depth` (field 4), 0 for a person.
  Rows written before this decode as a person's at depth 0. `conversation log`
  shows the Handle in human output and JSON, and `--lean` prints
  `<display name>/@handle: ...` for an agent.
- **Mentions.** A mention is `@` followed by a Handle, where the `@` is not
  preceded by a letter, digit or `@`. `conversation send` refuses a person's
  message whose mention names no Handle included in that Conversation, and
  nothing is appended. A mention inside an agent's reply that cannot summon,
  because its Handle is unknown or not included or because the reply is at
  `max_depth`, is skipped and logged, and the reply still commits. An agent
  that mentions itself is ignored. *Amended by
  [ADR 0016](0016-what-a-mention-is.md), which gives the full rule for
  finding mentions: case-insensitive, ending at a word boundary, with `\@`
  as an escape.*
- **Depth.** A message at depth *d* summons agents only if *d* < `max_depth`,
  and each summoned agent replies at *d*+1. With `max_depth = 1` a person can
  summon agents and agents cannot. Each branch of a chain counts its own depth.
- **Chains.** The mentions in one message run one after another, in mention
  order, each Handle once. Nested mentions run depth-first: if a person
  summons `@a @b` and `@a` summons `@c`, the order is a, c, b. Each reply
  commits before the next agent starts, so each agent sees the replies before
  it. On one Node, one chain runs at a time per Conversation, and a mention
  that commits during a chain waits behind it in order. Chains in different
  Conversations run concurrently. A chain cut off by a serve restart is lost,
  never resumed or replayed.
- **What an agent sees.** The system prompt is `SYSTEM.md` followed by a
  preamble from the serve: its Handle, the Conversation's Key, its depth, and either the
  Handles it may summon or that it may summon none at its depth. Then come the
  live Recap and the talk after it, in the `--lean` format. Nothing trims the
  context to a window yet.
- **Stream.** The serve's local socket gains `GET /v1/conversation/stream`,
  newline-delimited JSON events: `chain.start`, `agent.start {handle,
  depth}`, `agent.delta {text}`, `agent.reset` (a retry starts the reply
  over), `agent.skipped {handle, reason}`, `agent.commit {seq}`,
  `agent.error`, `chain.end`. It also gains `POST /v1/conversation/cancel`.
  These are the contract for any later TUI or GUI client, as they are for the
  CLI.
- **CLI.** `conversation send` stays attached after the person's message
  commits and streams the chain until it ends. Ctrl-C cancels the chain, and
  `--detach` returns once the message commits. `conversation follow` watches
  a Conversation's chains on this Node, and Ctrl-C only detaches it.
  `conversation cancel` cancels the running chain. `send --handle @handle
  [--depth n]` lets an external runner post as an agent. The Handle must be
  defined and included, and `--depth` defaults to 1.
- **Cancel.** Cancel ends the provider request, commits nothing for the
  running agent, and drops the rest of the chain. The person's message stays
  committed.
- **Retry.** Rate limits, overload, 5xx responses and dropped connections are
  retried up to 3 times with backoff, honouring `Retry-After`. A retry after
  the stream has started generates the reply again from the start
  (`agent.reset`). Once the retries are used up, or on any other error, the
  chain stops and commits nothing for that agent. The error goes to watchers
  and the serve log. There is no retry verb: the person mentions the agent
  again.
- **Text only.** An agent replies with text. It has no tools: it cannot
  propose a Sign Request or read chain state. Giving it any is a later
  decision with its own ADR.
