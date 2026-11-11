# A Node library, hosted by its front ends

DeFiOS gets a desktop app, and later a mobile one. Today a Node is two kinds
of process: `defios serve`, which holds the unlocked Vault and runs the Node,
and the CLI verbs, which either reach `serve` over its unix socket or read and
write the Node's files themselves. The rules a front end must follow are split
between them: `internal/serve` holds the running Node, and `internal/cli`
holds the rest, such as config writes, Secret sealing, Roster reachability and
alias resolution. A GUI built on top would have to either shell out to the CLI
or copy those rules.

Instead, a new package, `internal/node`, becomes the one application layer: a
typed Go API for every operation a person can perform on a Node, plus a
subscription to what happens on it. The CLI becomes a thin front end on that
API, and so does the desktop app. A front end **hosts** the Node in its own
process. `defios serve` does this for the terminal, and the desktop app does
the same for its window. The unix socket stays, served by whichever process
hosts the Node, so the CLI keeps working against a Node the app is hosting.

## Considered options

- **The GUI shells out to the `defios` CLI** — rejected: two processes, one of
  which a phone cannot spawn, and every screen would parse text or `--json`
  output whose coverage differs between verbs. Errors would come back as exit
  codes and stderr rather than typed values.
- **The GUI talks only to `serve`'s socket** — rejected: the socket API is
  private, covers only the operations that need a running Node, and has no
  config, Secret, Agent Profile, Plugin, Network or Trigger operations. Widening
  it into a full API would still leave the GUI needing a separate `serve`
  process, which mobile cannot have.
- **The GUI imports `internal/serve` and `internal/cli` directly** — rejected:
  `internal/cli` mixes rules with cobra flags and terminal output, so the GUI
  would either depend on cobra or copy the rules out of it.
- **A pure-Go UI toolkit (Gio, Fyne)** — rejected for now: a chat transcript
  with rich cards, Markdown and forms is quicker to build and to theme as a web
  UI, and the same web UI can later sit in a mobile shell. Wails hosts a web UI
  around a Go process on desktop.

## Consequences

- **`internal/node` owns the rules.** Each operation takes the Group it acts on
  explicitly, never the Current Group implicitly, so a front end can edit one
  Group's config while another is served. Errors are typed values. The CLI maps
  them to its exit codes and messages, and the app maps them to its own.
- **Hosting.** `node.Open` reads a Node directory. `Unlock(passphrase, group)`
  starts serving that Group in-process, as `serve` does today, and `Lock` stops
  it. Switching Group is a `Lock` followed by an `Unlock` of the other Group
  using the passphrase the host already holds, since a running Node keeps it in
  memory to read Secrets on each use. `Lock` forgets it.
- **One host at a time.** A Node directory is served by at most one process.
  A host that finds the socket already bound reports that another process is
  serving this Node, and does not serve it too.
- **Events.** One subscription carries everything a front end shows live:
  committed talk and Outbox changes, agent chain output, Sign Request and
  Session status changes, Trigger firings and their Jobs' outcomes, and Node
  status (tailnet, leader, Roster reachability, whether writes can commit).
  `conversation follow` and the socket's stream become adapters over it.
- **Room for a remote host.** The API is a Go interface whose in-process
  implementation is the only one for now. A later client mode, where an app
  front-ends a Node hosted elsewhere such as an always-on cloud Node, adds a
  second implementation over a connection, without changing the front ends.
  Nothing in the API may assume a unix socket, a terminal or a desktop OS.
- **Moved slice by slice.** Each area moves its CLI verbs onto `internal/node`
  in the same change that adds what the app needs from it. A verb's output and
  exit codes do not change when it moves. Trigger config gains write
  operations (Jobs, Hooks, Schedules) that the CLI does not have yet.
- **The desktop app** is a Wails v3 project at `desktop/`, in this module,
  with a web frontend, for Linux, macOS and Windows. Wails v3 has the system
  tray built in, which v2 lacks. Closing its window hides it to the system
  tray and the Node keeps serving; Lock or Quit stops the Node. Its own
  preferences (start at login, notifications, the keychain option) belong to
  the machine, not the Node: they are kept outside the Node directory and
  never go into a Backup.
- **The keychain option** stores the Vault passphrase in the OS keychain so the
  app can unlock at login. It is off by default, because it moves the Node's
  protection from a passphrase someone must type to the OS account: a stolen
  disk or Backup stays sealed, but any program running as that user can
  unlock the Node.
- **Mobile is later.** It would host the same `internal/node` through a
  gomobile binding, serving only while the app is in the foreground. A phone,
  like a laptop, is a part-time Node; always-on duties such as Triggers belong
  on a headless `defios serve`.
