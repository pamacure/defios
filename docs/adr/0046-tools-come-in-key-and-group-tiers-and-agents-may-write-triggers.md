# Tools come in Key and Group tiers, and agents may write Triggers

An operator wants an agent that turns their spec ("every weekday at 9, have
@auditor summarise the talk") into Jobs, Hooks and Schedules. Writing a
Node's Trigger config is an elevated privilege: an `exec` Job runs any
program as the Node, and a Hook can wire the default `sign_accept` Job to
every request. Instead of restricting what such an agent may write, this
keeps it apart from signing. Built-in Tools come in two tiers. A **Key Tool**
acts on one Key, and a **Group Tool** acts on the Group as a whole. A profile
grants Tools from at most one tier, and its tier fixes where it can be
included. The guarantee: **no agent that answers in a Key's Conversation can
change Triggers, and no agent in Group-level talk can sign.** This amends
[ADR 0032](0032-triggers-run-jobs-live-only.md),
[ADR 0038](0038-an-agent-jobs-agent-stays-included-where-it-answers.md),
[ADR 0044](0044-a-key-is-marked-with-the-network-type-a-node-uses-it-for.md) and
[ADR 0041](0041-an-agent-summons-by-a-tool-and-is-granted-no-tool-by-default.md).

## Decisions

- **Tiers come from the Tools a profile grants.** These are the tiers:
  - Key Tools: `sign_list`, `sign_inspect`, `sign_propose`, `sign_accept`,
    `sign_refuse`, `chain_fill` and `key_address`.
  - Group Tools: the Trigger-config Tools, `job_runs` and `key_list`.
  - The `talk_` Tools, Skills and MCP Servers have no tier.

  A profile that grants Tools of both tiers is refused when saved, by the
  CLI, the desktop and the loader. The desktop disables the other tier's
  Tools once one is ticked. A Tool's name prefix says what it is about, not
  its tier: `key_list` is a Group Tool.
- **Where a tier may be included.** A Key-tier profile may be included only
  in Key's Conversations, and a Group-tier profile only in Group-level talk.
  An untiered profile may go anywhere. `IncludeAgent` and `SetIncludedAgents`
  refuse a profile included in the wrong place, and so does a profile save
  that would put an already-included profile there. Each refusal names every
  place it breaks, as `AnswersError` does. A run that still finds itself in
  the wrong place, after a hand edit, fails and says why. It never silently
  drops Tools, which is what `agentTools` used to do with sign Tools in
  Group-level talk. Nothing is migrated.
- **A Conversation no longer follows the Group's list.** Most Conversation
  agents, such as signers and auditors, are Key-tier and could never come
  from the Group's list, so following had little left to offer. An unset
  `[agent] profiles` in a Conversation's Workspace now means no agents, the
  same as `profiles = []`. A new Key's Conversation starts with none, and one
  that followed the Group's list loses its agents. `ErrFollowsGroup` and the
  "follows Group" state in the CLI and desktop go. The Workspace's other
  settings still merge over the Group's.
- **The Trigger-config Tools.** `job_`, `hook_` and `schedule_` each get
  `list`, `add`, `edit` and `delete`, which call `internal/node`'s `AddJob`,
  `EditJob`, `DeleteJob` and the rest for the Group the agent answers in.
  - Add and edit take `{name, <job|hook|schedule>: {...}}`, the entry as JSON
    with its TOML keys. A loader refusal comes back as a Tool error the model
    can correct. Add refuses a name already defined, and edit refuses one
    that is not.
  - The list Tools return the same shape, plus `by`, and `next` for a
    Schedule.
  - There are no limits on the kind of Job or on which Jobs a Trigger runs.
    What is written takes effect at once.
  - Keys are given by Key alias or Workspace name. The Workspace name is what
    is stored, since it never changes and an alias can be renamed or absent.
    Lists show both.
  - ADR 0038's inclusion checks apply unchanged. The Tools never change who
    is included: inclusion is a grant made on Talk, and a refused `job_add`
    names where the agent is missing, so the agent can ask a person.
- **`job_runs`** reads this Group's Job Runs (ADR 0040), so an agent can check
  what its Jobs did. There is no `job_run`: running a Job from inside a run
  would start a second Chain, and could collide with the
  one-Chain-per-Conversation rule.
- **`key_list`** returns, for each of the Group's Keys, its alias (or none),
  Workspace name, curve, public key, Network type, its addresses, and the
  Handles included in its Conversation. It also returns one `group` entry
  with the Handles of Group-level talk. A Key's addresses are those on this
  Node's Networks of its Network type alone: an Ethereum Key's EIP-55
  address on each Ethereum Network, a Bitcoin Key's P2WPKH address on each
  Bitcoin Network, a Solana Key's base58 public key on each Solana Network.
  With no Network of its type, a Key still has its type's own address, on
  no Network: the EIP-55 address, the base58 public key, or the P2WPKH
  address on bitcoin mainnet. `key_address`, `key inspect` and the desktop
  give a Key's addresses from one function, so they always agree; which
  Networks exist is for a Networks Tool to tell.
  Every Key has a Network type, since one with none set takes its curve's
  default (below). This tells a Group agent how the Node uses the
  Key without implying any limit on what it signs: the Network type still
  restricts nothing (ADR 0044). `key_address` stays a Key Tool that takes no
  input and reads only its own Conversation's Key, and its addresses are
  filtered the same way, so both Tools give one Key the same addresses.
- **`by = "<handle>"`** on a Job, Hook or Schedule names the agent that last
  wrote it. The Tools set it and refuse a `by` passed to them. A person's
  save, from the desktop, the CLI or by editing the text, clears it. An agent
  Job with `by` set gives its prompt as `A Job written by @<by> asks:`, so the
  agent it runs weighs it as another agent's words, not the operator's. An
  agent's edit to a person's Job sets `by`. Hooks and Schedules carry no
  prompt, so their `by` only tells the desktop who wrote them.
- **No briefing text.** The DeFiOS Briefing is unchanged. A `write-triggers`
  Skill in the built-in `defios` Plugin explains how to write Jobs, Hooks and
  Schedules: their fields, Hook filters and events, depth and loops, that
  what is written runs unattended, how `by` frames a prompt, and checking
  results with `job_runs`. Like any Skill, it is granted per profile.
- **An ECDSA Key with no Network type is for Ethereum**, as an EdDSA Key
  with none is for Solana. ADR 0044 left it unset. Every Key now has a
  Network type, so `key_list` always has one to filter by, and the desktop
  shows an unset ECDSA Key's Conversation with the Ethereum icon. Bitcoin is
  still chosen by setting it, when the Key is made or later from its About
  panel.
- **No `sign_broadcast` Tool.** Since ADR 0045, a signed request is broadcast
  automatically. A rebroadcast sends the same signed bytes again, which fixes
  only a transient rejection or a slow Ethereum transaction. `sign broadcast`
  stays as the manual retry in the CLI and desktop.

## What the guarantee leaves open

It separates who reads a Key's Conversation from who writes Triggers. It does
not make Trigger Tools safe:

- A Group-tier agent can sign indirectly, by writing an `exec` Job that calls
  `defios sign accept`, or a Hook that runs the default `sign_accept` Job.
- A Hook can fire a Group-tier agent's Job on a Key's event, putting that
  event, including a peer's free-text reason, in the agent's turn.
- Group-level talk is written by every Node of the Roster, so another party
  can still try to steer a builder.

Granting the Trigger-config Tools is granting the Node, and the operator
grants them knowingly.

## Considered options

- **Agents write only `agent` Jobs, and their Triggers run only `agent`
  Jobs.** Rejected: it blocks real specs, such as a webhook notification. An
  operator who grants these Tools has already chosen to trust the agent with
  the Node.
- **Agent-written Triggers stay disabled until a person enables them.**
  Rejected: it adds a new state to every Trigger. The operator asked for the
  Trigger, and granting the Tools is the opt-in.
- **Job prompts read as the operator's whoever wrote them.** Rejected: a
  builder could then speak with operator authority to an agent holding Tools
  it lacks.
- **A filtered follow: Conversations follow only the Group's untiered
  agents.** Rejected as one more rule for little value.
- **`key_list` gives every address of the Key's curve, whatever its Network
  type, or when it has none.** Rejected: a Group agent writing Jobs needs the address the Node
  uses the Key for, and an ECDSA Key's unused Bitcoin addresses next to its
  Ethereum ones invite a Job aimed at the wrong chain.
- **`key_address` takes a `key` in Group-level talk.** Rejected: one name with
  two schemas. `key_list` gives Group agents the addresses instead.
- **12 Tools merged into `_set`, or 3 `trigger_` Tools with a `kind`.**
  Rejected: add and edit keep the already-defined check that stops an agent
  overwriting an entry by accident, and a profile can be granted just the
  list Tools.

## Consequences

- ADR 0032's "the CLI does not write them" now reads: people write Trigger
  config through front ends and by hand, and agents through these Tools.
- ADR 0038 loses following: an exclude from Group-level talk no longer counts
  for any Conversation.
- ADR 0041's default for a new profile, `talk_read` and `talk_write`, is
  untiered and unchanged.
- ADR 0044's "an ECDSA Key for nothing in particular" now reads "for
  Ethereum".
