# Triggers run Jobs, live only

Schedules are how an always-on Node starts Jobs unprompted; Hooks are how it
answers what others do. Both are **Triggers** for a Node that serves around
the clock, such as one in the cloud, standing in for an operator who is not
watching; a laptop or phone Node, which serves only while its person is
there, gains little from them. A Trigger is defined in the Group's
`config.toml`, alongside the **Jobs** it runs, and is Node-local: it acts as
this Node only, as [ADR 0022](0022-members-are-trusted-for-their-own-messages.md)
requires. Triggers are stateless and fire only on what happens live: a Hook
on a COSL event this serve applies while running, a Schedule at a time that
passes while this serve serves its Group. Nothing is caught up, replayed or
made up later.

## Considered options

- **Triggers on the COSL, shared by the Group.** Rejected: a Trigger acts as
  one Node, and no Node may decide for another (ADR 0022).
- **Catching up missed events and Schedule firings** (a cursor per Hook, a
  grace window per Schedule). Rejected: a Node back from a week away would
  start hundreds of agent runs on stale events, and the cursor would be state
  to keep, back up and reason about. An always-on Node misses little, and a
  Node that is not always-on should not be acting unattended.
- **Built-in `sign_accept` and `sign_refuse` Jobs gated by a policy check.**
  Rejected: an agent Job granted `sign_accept` and `sign_refuse`, or an
  `exec` Job calling the CLI, already decides and acts. The two sign Jobs
  ship as ordinary `exec` tables in each new Group's config, where the
  operator can read, change or delete them, rather than as behaviour an
  upgrade could change unseen.
- **Hooks that gate this Node's own accept**, running before it, whoever
  starts it (a person, an agent or a Job), and able to veto it. Deferred, not
  a Hook: a guardrail rather than a Trigger, with its own failure semantics,
  kept as a possible future **Policy**.
- **Branching on a Job's outcome.** Rejected as a workflow engine. A Hook
  runs its Jobs in order and stops at the first that fails, as `&&` does.

## Consequences

- **Config.** In the Group `config.toml` only, never in a Conversation
  Workspace, and reread when the file changes, as for `prune`:

  ```toml
  [jobs.make_sign_decision]
  agent = "auditor"         # in the target's profiles, or a config error
  prompt = "Review this Sign Request. Call sign_accept, sign_refuse, or neither."

  [jobs.notify]
  webhook = "$NTFY_URL"     # Secret references allowed, as for RPC URLs
  headers = { Authorization = "$NTFY_TOKEN" }

  [jobs.daily_summary]
  agent = "auditor"
  target = "group"          # or a Conversation, e.g. "ecdsa-02a1b2c3"
  prompt = "Summarise yesterday's talk and any open Sign Requests."

  [hooks.review_requests]
  events = ["sign.request"]
  from = "others"
  jobs = ["notify", "make_sign_decision"]

  [schedules.morning]
  cron = "0 9 * * 1-5"      # or at = "2026-12-01T09:00:00Z"
  tz = "Europe/Paris"       # default: the Node's local zone
  jobs = ["daily_summary"]
  ```

  A Job has exactly one of `agent`, `exec` or `webhook`. Names follow the
  alias rule. A new Group's config is written with two Jobs and nothing
  else: `sign_accept` (`defios sign accept "$DEFIOS_SIGN_REQUEST_ID"`) and
  `sign_refuse`. Groups formed before this get no defaults.
- **Hook filters** are ANDed, every field optional: `events` (message types
  such as `sign.request`, and Sign Request statuses as `request.<status>`,
  including those no message marks, such as `request.expired`), `from`
  (`"self"`, `"others"` or Node IDs), `handles` (`user` for a person),
  `conversation` (a Workspace name or `"group"`) and `text` (a regular
  expression over `app.freeform`). Transaction content is not filterable:
  that is a Job's to judge.
- **Live.** A Hook fires on rows applied by the log after the serve's
  startup catch-up, including rows that arrive after a partition heals
  mid-run, and on status changes the running serve sees happen. The startup
  backlog and a snapshot restore never fire. A queued Outbox message never
  fires. A Schedule fires only if the serve is serving its Group as the
  Current Group at that time, so an always-on Node in several Groups runs
  only the Current Group's Triggers.
- **Depth bounds loops**, as for mentions. A Hook fires only on an event
  below the Group's `[agent] max_depth`. An agent a Hook runs writes at the
  triggering depth + 1; one a Schedule runs, at depth 1. `SignRequest` gains
  a `depth` field, written by an agent as its own depth, and a `sign.*`
  event has the depth of its request. Accepts and refusals are final stances
  and need none. `exec` Jobs get `DEFIOS_DEPTH`, and `conversation send` and
  `sign propose` write at that depth when it is set.
- **`agent` Jobs** run a chain that no person's message started: the Job's
  prompt and the Trigger's event (or firing time) form the last user turn
  and are never committed. The target defaults to the event's Conversation
  (a Sign Request's Key), and a Schedule's Jobs must name one. The usual
  one-chain-per-Conversation rule, Transcript and stream apply. A Job's
  chain is named by a random id, a Hook's too: the event's msg_id may
  already name a person's chain, two Jobs may answer one event, and a
  status change has no message. The Handle must be in the target's
  profiles: checked as the config is read for `target = "group"`, and as
  the Job runs elsewhere, where a Conversation's Workspace may replace them.
- **`exec` Jobs** take an argument list, not a shell string, with the event
  as JSON on stdin. Their environment holds `PATH`, the `DEFIOS_*` variables
  (Group alias, Trigger, event type, msg id, Sign Request id, Conversation,
  depth, and the config and data directories and socket the CLI finds this
  Node by) and the Secrets the Job names, nothing else. serve expands
  `$DEFIOS_*` and `${DEFIOS_*}` in the arguments, and nothing else, so the
  default Jobs need no shell. They run in the Group
  Workspace, with a `timeout` (default 60s), and their output goes to the
  serve log. Firings of one Hook run one at a time.
- **`webhook` Jobs** POST the same JSON, with a `timeout` (default 10s). A
  2xx response succeeds. 5xx responses and dropped connections are retried up
  to 3 times with backoff, 4xx is not.
- **CLI.** `hook list`, `schedule list` (with next firing) and
  `job run <name> [--event file.json]`. Definitions are edited in
  `config.toml` only. The stream gains `trigger.fire` and `job.end`.
