# Conversation retry is removed

[ADR 0018](0018-an-agent-says-one-message.md) added `conversation retry`,
which reruns the steps of a failed chain that did not say or stay silent.
Once agents call Tools with effects
([ADR 0029](0029-an-agent-profile-holds-the-whole-agent.md)), a step can fail
after it proposed, accepted or refused, and rerunning it acts as this Node a
second time. Rather than teach retry which effects are safe to repeat, it is
removed: a chain that fails or is cancelled ends there, and a person who
wants it again mentions the agent again. This amends ADR 0018 and
[ADR 0025](0025-an-outbox-holds-what-a-node-writes-without-a-quorum.md).

## Consequences

- `conversation retry`, its serve endpoint, outcome reuse and the `attempt`
  count go. Steps keep their numbers in a chain's Transcript.
- Provider retries stay: rate limits, overload, 5xx responses and dropped
  connections are retried with backoff inside one run, as ADR 0015 gives.
- A chain lost to a serve stop is lost, as ADR 0015 first had it.
- Transcripts written before this, with `attempt` on their lines, still read.
