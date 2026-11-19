# A Job Run is kept on the Node that ran it

A Job used to leave almost no trace of a run. An `exec` Job's output went
line by line to the serve log. A webhook's request and response were not
kept at all. `serve` remembered only the state and error of each Job a Hook
ran, in memory, for as long as that serve lived. Talk drew its "Trigger
fired" and "Job finished" lines from live events and lost them on restart.
So a person could not see what a Job did, or which talk messages it wrote.

Each time a Job runs, its Node now keeps a **Job Run**. It is kept in the
Group's directory on the Node that ran the Job and is never replicated,
like a Transcript.

## Decisions

- **Every Trigger records one.** A Hook firing and a Schedule firing each
  record a Job Run for every Job they run, including a Job never run because
  one before it failed (`not_run`). A Job Run holds what fired it and on
  what event, its state, and when it started and ended. It also holds:
  - **exec:** the command, its exit code, and its stdout and stderr.
  - **webhook:** the method and URL, each attempt's HTTP status, and the
    last response body.
  - **agent:** the Handle, where it answered, and the chain it ran as. The
    run itself stays in the agent's Transcript, so it is not copied.
- **Bounded.** A Group keeps its last 200 Job Runs, dropping the oldest
  first. Output and response bodies keep their last 64 KiB, with a marker
  where they were cut. A Job that loops cannot fill the disk.
- **No Secret is stored.** A webhook's URL is kept as configured, with
  `$NAME` references unexpanded. Request headers are not kept, and of the
  response headers only `Content-Type` is kept. Nothing in an exec Job's
  output is redacted: it is the operator's own command, and the serve log
  already saw it.
- **A Job Run records the messages its Job wrote.** An `exec` Job is given
  `DEFIOS_JOB_RUN`, a token naming its Job Run, alongside the other
  `DEFIOS_*` variables. When the CLI writes to talk with that token set, it
  records the message's msg_id on the Job Run. The token goes nowhere near
  the COSL: the message itself is unchanged. An agent Job's messages are
  found through its chain.
- **Readable from every front end.** `trigger runs [--job NAME] [--last N]`
  lists Job Runs, and `trigger run <id>` shows one in full. The desktop
  app's Talk page draws its Job lines from Job Runs instead of live events,
  so they survive a restart. Selecting one, or a message a Job wrote,
  shows its Job Run.
- **Left out of a Backup.** A Job Run is one machine's history, like a
  Transcript or the Outbox ([ADR 0027](0027-a-backup-is-the-node-minus-what-peers-give-back.md)).

## Considered options

- **Keeping only the serve log.** It is shared by everything `serve` does,
  is plain text, and is cut by log rotation. It cannot answer "what did this
  Job's run print" or "which Job wrote this message".
- **Replicating Job Runs, or marking a Job's message on the COSL with its
  run.** Rejected: a Trigger acts as one Node only
  ([ADR 0032](0032-triggers-run-jobs-live-only.md)), its output may hold
  anything the operator's command printed, and other Nodes need only what it
  said. Other Nodes see a Job's message as the Job's, and nothing more.
- **Keeping request headers with Secrets redacted.** Rejected: a header may
  carry a Secret's expanded value that redaction by name cannot find, such as
  a token built into a longer string. Dropping the headers is the only safe
  option.
- **No limit, with Pruning.** Rejected: Job Runs are diagnostic, not a
  record anyone relies on, so a fixed bound is simpler than another policy.
