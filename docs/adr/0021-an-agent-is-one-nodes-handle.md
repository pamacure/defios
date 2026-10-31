# An agent is one Node's Handle

[ADR 0018](0018-an-agent-says-one-message.md) made every message under a
Handle, from any Node, one assistant's words. That merges agents that
different parties configured on their own, with their own profiles, Models
and keys, into one voice, and hands each of them words it never chose.
DeFiOS signs only when enough Nodes each judge a Sign Request for
themselves, so an agent belongs to its Node: an **agent** is the pair (Node,
Handle) in one Group. A summoned agent's own turns are the messages its own
Node wrote under its Handle. The same Handle from another Node is another
agent, and its messages are talk from another party like any other. This
amends ADR 0018.

## Considered options

- **One assistant per Handle across Nodes**, as ADR 0018 had it. Rejected:
  each Node binds its Handles itself ([ADR 0015](0015-agent-profiles-handles-and-agent-chains.md)),
  so two `@auditor`s may run different prompts on different Models, and
  neither should answer for what the other said. It also ties an agent's
  context to Nodes that hold none of its Transcript.
- **A Group-wide agent, its profile on the COSL.** Rejected: agents that
  share one prompt and one Model fail together, so one injected `reason` or
  one member's edit to the shared prompt reaches every Node's agent at once,
  and the threshold no longer means t independent judgments. Copying a
  profile between Nodes one person runs, such as a laptop and a phone in a
  2-of-2, is a later and explicit step, and the copy is still a separate
  agent on each Node.

## Consequences

- **Turns.** As ADR 0018 gives them, except that an `assistant` turn is a
  message whose Handle is the summoned agent's and whose sender is the Node
  running it. Messages under the same Handle from other Nodes are `user`
  content in the `--lean` line format, `<display name>/@<handle>: <text>`,
  which names their Node by its Roster display name. The fixed `user` turns
  before and after the agent's own message are unchanged.
- **Preamble.** It tells the agent that its own earlier messages are those
  its Node wrote as `@<handle>`, and that the same Handle from another Node
  is another agent.
- **Mentions are unchanged.** A mention runs the agent of the Node whose
  message carries it ([ADR 0015](0015-agent-profiles-handles-and-agent-chains.md),
  [ADR 0016](0016-what-a-mention-is.md)), which was already this Node's
  agent. A message posted with `send --handle` is that Node's agent's too.
- **An agent outlives its profile.** Rebinding a Handle to another Agent
  Profile, Provider or Model keeps the agent: its earlier messages stay its
  own turns.
- **Context across summons is still the talk.** ADR 0018 rejected feeding an
  agent its earlier Transcripts partly because its context would depend on
  which Node ran it. Only one Node runs an agent now, so that reason is gone;
  the cost reason stands, and whether a summon carries on from the agent's
  earlier runs is a separate decision.
- **Nothing on the COSL changes.** Every `app.freeform` already records its
  sender Node and its Handle, so there is nothing to migrate.
