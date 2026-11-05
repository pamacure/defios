# A Node refuses a Sign Request once

Until now a Node said no to a Sign Request by not accepting it
([ADR 0010](0010-signing-session-semantics.md)), so the log could not tell a
request nobody had looked at from one a Node had judged and turned down. Once
agents can act on Sign Requests, that gap lets one agent of a Node refuse in
words while another of the same Node accepts. A **Sign Refusal** is a new COSL
message, `sign.refuse` (id 13), carrying a required `reason`, and each Node
now takes one final stance on a request: it accepts or it refuses, never both,
and never changes its mind. This amends
[ADR 0009](0009-sign-request-on-cosl.md) and ADR 0010.

## Considered options

- **Refusing is saying so in talk.** Rejected: talk is pruned, is not tied to
  the request, and binds nothing, so a second agent or a person on the same
  Node could still accept.
- **`sign decline` for refusals too.** Rejected: declining aborts a signing
  Session already started, an emergency stop with a different effect; before
  a Session it has nothing to do. The two stay distinct verbs.
- **A refusal that can be withdrawn, or an accept that can.** Rejected: an
  accept already counts toward the signers in log order, and a stance that
  can flip is no judgment the Group can rely on.

## Consequences

- **One stance per Node.** A Node that refused cannot accept
  (`already-refused`), and one that accepted cannot refuse
  (`already-accepted`). The proposer's request is its acceptance, so it can
  never refuse its own. A refusal once the signer set is full fails with
  `signer-set-full`, as a late accept does. Every Node enforces this when it
  applies the log, so a refusal and an accept from one Node racing in the
  Outbox resolve by log order: the later one is ignored.
- **`refused` status.** A request is `refused` once its refusals exceed
  n − t, so t accepts can no longer exist. Below that it stays `open`, and its
  summary counts refusals beside accepts.
- **Handles on sign messages.** `sign.request`, `sign.accept` and
  `sign.refuse` gain a `handle` field, as `AppFreeform` has
  ([ADR 0015](0015-agent-profiles-handles-and-agent-chains.md)): the Handle of
  the agent that acted, empty when a person did. Other parties can then tell a
  model's judgment from a person's. Rows written before this decode as a
  person's.
- **Queued like an accept.** A `sign.refuse` may wait in the Outbox
  ([ADR 0025](0025-an-outbox-holds-what-a-node-writes-without-a-quorum.md)).
- **CLI.** `sign refuse <msg-id> --reason <text>`. `sign list` and
  `sign inspect` show this Node's own stance (`accepted`, `refused` or
  `unaddressed`) and every refusal with its Node, Handle and reason.
