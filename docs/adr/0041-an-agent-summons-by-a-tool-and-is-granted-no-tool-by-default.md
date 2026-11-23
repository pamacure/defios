# An agent summons by a tool, and is granted no Tool by default

Since [ADR 0015](0015-agent-profiles-handles-and-agent-chains.md), an agent
brought another in the way a person does: by writing `@handle` in what it
said, with `\@` to write one without summoning
([ADR 0016](0016-what-a-mention-is.md)). In practice that failed both ways.
Models write `@auditor` when they mean only to name it, and they `@` the
person they are answering, which names no Handle and comes back as an
`agent.skipped` the model never sees. The rule that people mention stays; an
agent's text no longer mentions at all. An agent brings another in only by
calling a Tool, `talk_summon`, which is checked when it is called, so the
model hears at once why a summon is refused and can correct it.

While reworking what an agent is given, the `talk_` Tools stop being granted
by default ([ADR 0036](0036-an-agent-replays-its-own-transcript-not-the-talk.md)):
a profile is granted exactly the Tools it names.

## Decisions

- **A Mention is a person's or a Job's.** `@handle` in a person's message,
  or in a message a Job writes (such as an `exec` Job's `conversation send`),
  summons as ADR 0016 gives it. A mention that names no Handle included where
  it is sent is refused, a Job's as well as a person's (a Job's used to be
  skipped): the Job Run then shows the failed send, so someone can act on it.
- **An agent's text mentions nobody.** `@handle` in what an agent writes is
  only text; a client may still render and colour it as a Handle. The serve
  no longer scans an agent's said text for mentions. The preamble drops all
  `\@` advice and says nothing about `@` at all.
- **`talk_summon(handle)`.** An agent summons by calling it, once per
  Handle. It is refused, with a tool error the model can act on, when the
  Handle is its own, a person (`user`), not included where the agent
  answers, already summoned in this run, or when the agent is at the max
  depth. A refusal names the Handles the agent may summon, so the Tool works
  without `talk_parties`. It does not end the run: an accepted summon
  replies that the Handle will answer after the run ends.
- **The run ends as before.** On an accepted `talk_write`, or when the model
  ends its turn without one (silent). Its summons then run in the Chain,
  depth-first, in the order they were called, at the next depth. A Handle
  that is no longer included by the time its step comes is skipped, as
  today. The summoning agent never waits on what it summons: its run is
  over before theirs start.
- **A silent agent may summon.** A router that says nothing and summons
  another is allowed. The summoned agent's turn shows, as what summoned it,
  the nearest message up the Chain (for a router answering a person, that
  person's message), framed as `@router summoned you on account of:`.
- **A Summon stays on its Node.** It is recorded in the summoner's
  Transcript and published as a chain event, `agent.summoned {handle, by}`,
  and Talk on that Node shows it, the faded row of a silent run included.
  Nothing about it goes on the COSL: Handles resolve only on the Node that
  runs them, so a summon would mean nothing to a peer.
- **`talk_parties`.** Returns the Parties of the talk the agent answers in,
  as this Node knows them, at the time of the call:
  ```json
  {"parties": [
    {"name": "johnny",  "kind": "person", "node": "johnny"},
    {"name": "triage",  "kind": "agent",  "node": "johnny", "you": true},
    {"name": "auditor", "kind": "agent",  "node": "johnny", "summonable": true}
  ]}
  ```
  A person for each Roster Node, under the name talk lines show it by, and
  this Node's Agents included there. `summonable` is present only when the
  caller holds `talk_summon`, and true when a summon of that Agent would be
  accepted now. Other Nodes' Agents are not listed: they cannot be summoned
  from here, and this Node knows them only by what they have written.
- **The facts line keeps only the depth.** `(You are at depth 2 of at most
  3.)` A Handle can be included or dropped mid-Conversation, and a list of
  summonable Handles replayed in an old turn would contradict the current
  one; who can be summoned now is `talk_parties`' to say.
- **No Tool by default.** A profile with no `tools` is granted none, the
  same as `tools = []`; an agent so given can only think and write its
  Transcript. A profile made with the CLI or the GUI is written with
  `talk_read` and `talk_write`; `talk_summon` and `talk_parties` are never
  pre-ticked. No profile is migrated: one relying on the old default loses
  its `talk_` Tools until they are named.

## Considered options

- **Keep text mentions for agents, with a better preamble.** Rejected: the
  escape was already in the preamble and models still slipped, and a refusal
  after the run ends cannot be corrected.
- **A `summon` field on `talk_write`.** It would tie every summon to a
  message, but a router could not summon in silence, and a bad Handle would
  refuse the message with it. A separate Tool is checked on its own and is
  granted on its own.
- **`talk_summon` ends the run.** Rejected: `talk_write` already does, so an
  agent could do both only in one response, and a refused `talk_write`
  beside an accepted summon would leave the run half done.
- **Leave `talk_summon` out at the max depth.** Rejected: the set of Tools
  would change between runs, breaking the cached prefix
  ([ADR 0018](0018-an-agent-says-one-message.md)). It refuses instead.
- **Record summons on the COSL.** Rejected, as above: they mean nothing off
  the Node that resolves them.
- **Call it `talk_members`.** Rejected: a Roster's Nodes are not what it
  lists, and the GLOSSARY keeps "member" away from the Roster. Party is the
  word Livery already used.

## Consequences

- Amends ADR 0015 (Mentions, and how a Chain grows), ADR 0016 (Resolving
  mentions: a Job's unresolved mention is refused, an agent's text is not
  scanned), ADR 0018 (the facts line) and ADR 0036 (default Tools).
- `internal/chain` walks summons recorded by a run in place of the mentions
  in its said text; `internal/mention` and its vectors are unchanged.
- The `tools = []` marshalling that kept an empty list apart from an unset
  one goes, since both now mean none.
