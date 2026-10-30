# An agent says one message; its Transcript stays on its Node

The COSL should read like a group chat, not a human–LLM chat. A summoned
agent may generate as much as it needs (thinking, text, tool calls), but the
only thing it commits is what it passes to its one tool, `say`: a message of
a paragraph or two, like a longer text in a group chat. Everything it
generates is written to a **Transcript** on the Node that ran it and is never
replicated. Other parties see only the message, so the quality of what an
agent says is what matters. This amends
[ADR 0015](0015-agent-profiles-handles-and-agent-chains.md): an agent no
longer has no tools, its context is split into turns, the stream carries the
whole output, and there is a retry verb.

## Considered options

- **Commit the reply as generated, capped to fit an `app.freeform`.**
  Rejected: it either cuts the model off or fills the log with long markdown,
  and it caps what the model may think and do to what the log can hold.
- **The final text block is the message.** Rejected: its length cannot be
  enforced without truncating, and an agent cannot choose silence. A tool call
  can be refused and rewritten.
- **Force a tool call (`tool_choice`).** Rejected: it forbids silence, and
  Anthropic refuses it with extended thinking.
- **Feed an agent its own earlier Transcripts on the next summon.**
  Rejected: an agent's context would then depend on which Node runs it, and
  its cost would grow with every summon. A summon starts from the group chat.
- **Keep Transcripts in the Vault.** Rejected for now: they are working
  output, not secrets, and today hold no tool results. Revisit when tools that
  read sensitive state exist.
- **A failure recorded on the COSL.** Rejected: an error is this Node's
  business. The log holds only what was said.
- **Resume a failed agent mid-run from its Transcript.** Rejected for now:
  retry starts that agent over. Simple and robust first.

## Consequences

- **`say`.** Every summon is offered exactly one tool, `say(text)`. When a
  `say` is accepted the serve commits `text` as the agent's `app.freeform`
  and the run ends: nothing is generated after it. A `say` over 4096 bytes,
  or empty, is refused with a tool error saying why, and the model may try
  again. If the model ends its turn without calling `say`, the agent is
  **silent**: nothing commits, and the chain goes on. The preamble tells the
  agent all of this. A person's message keeps the 64 KB `app.freeform`
  limit. `say` has no external effect; any further tool is still a later
  decision with its own ADR.
- **Context within a summon.** A run is a loop of model calls. Each call
  carries everything the run has generated so far, thinking, text, tool calls
  and tool results, refused `say`s included. The next summon of the same
  agent sees none of it, only the talk.
- **Turns by Handle.** The talk is sent as turns, not as one user message.
  Every message whose Handle is the summoned agent's, from any Node, is an
  `assistant` turn holding its text alone. Everything else (people, other
  agents, the live Recap) is `user` content in the `--lean` line format, and
  consecutive lines merge into one `user` turn. All `@auditor`s across Nodes
  are one assistant, whichever profile or Model each Node runs. If the talk
  opens with the agent's own message, or ends with it, the serve adds a fixed
  `user` turn before or after it. `conversation log --lean` is unchanged.
- **Caching.** For Anthropic, the system prompt is sent as blocks:
  `SYSTEM.md` and the stable preamble first, the depth and summonable
  Handles last. Cache breakpoints sit after the stable blocks and on the
  latest turn, so each call of a run, and each summon after the last Recap,
  reads the prefix from cache. A new Recap starts the prefix over. OpenAI
  caches prefixes itself and needs only the same order.
- **Profile options.** `[agents.<handle>]` gains optional `max_tokens`
  (per model call, default 8192), `temperature` (omitted when unset) and
  `max_turns` (model calls per summon, default 8). They are named for what
  they mean, not for a provider's field: OpenAI gets `max_completion_tokens`.
  A value a provider refuses is an error that is not retried. Thinking is a
  later option. A call that stops at `max_tokens`, or a run that reaches
  `max_turns` before `say`, is an error: nothing commits.
  *Amended by [ADR 0019](0019-providers-replace-models-on-the-cosl.md): `max_tokens` and
  `temperature` are gone; they, and every other request field, go in
  `[agents.<handle>.body]` under the shape's own names. `max_turns` stays.*
- **Transcript.** Each chain writes one JSONL file under the Group's data
  directory, `group/<alias>/transcripts/<conversation>/<chain>.jsonl`, where
  `<conversation>` is the hex Conversation ID or `group` for Group-level
  talk and `<chain>` the seq of the person's message that started the chain.
  Every line names its `step` (the run's place in the chain's depth-first
  order, from 1), `handle`, `depth` and `attempt` (from 1, counting
  retries of that step). A run records each request as sent (without
  credentials), each output block, the usage, and one closing **outcome**:
  `said {seq}`, `silent`, `error {error}` or `cancelled`. A run cut off by a
  serve stop has no outcome. A retry appends to the same file. One chain runs
  at a time per Conversation, so a file has one writer. Transcripts are
  plaintext, outside the Vault, and survive a replica wipe or restore;
  Pruning does not touch them, and nothing yet deletes them.
- **Stream.** The stream is for whoever follows the whole output. It carries
  every block as it is generated, `agent.delta {kind, text}` with `kind`
  `thinking`, `text` or `tool` (a `say` streams as a `tool` like any other),
  and `agent.commit {seq, text}` with the message once it commits.
  `agent.silent {handle}` replaces a commit for a silent agent. The other
  events are as ADR 0015 gives them.
- **CLI.** An attached `conversation send` prints each agent's message as it
  commits, with skips, silences and errors; `conversation follow` streams the
  whole output and `--json` prints raw events.
- **Retry.** Provider retries are unchanged. `conversation retry [--key]
  [--chain <seq>]` (requires serve) reruns a chain of this Node's that failed,
  was cancelled or was cut off by a stop; without `--chain` it takes the
  latest such chain in that Conversation, as the Transcripts show. It walks
  the chain as it first ran, depth-first in mention order, and for each step
  whose latest attempt ended `said` or `silent` it reuses that outcome (and
  the summons in the said text) instead of running it again. Reusing the
  earlier outcomes keeps every step's number the same on a retry; every
  Handle the walk reaches takes a step, even one skipped, so a change to
  what is included renumbers nothing. A run whose message committed records
  `said` even if what follows fails, so it is never posted twice. Every
  other step runs again as a new attempt, against the talk as it is now. A
  chain with no Transcript, such as one still queued when the serve stopped,
  runs from the start. Retrying a chain that is running or queued is refused. Retry appends
  nothing itself, and its errors stay Node-local like any other.
- **A chain lost to a stop is not resumed on its own.** Only chains running
  or queued at that moment are lost, since `send` needs a serve; switching
  the Current Group or a restore counts as a stop. A detached sender learns of
  it only from the missing reply. `conversation retry --chain <seq>` recovers
  it, and no crash loop ever spends a key unasked.
- **What other parties see.** Only the message, never the Transcript. When
  agents can later propose a Sign Request, its `reason` must carry the why.
