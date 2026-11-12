# DeFiOS desktop

The desktop app hosts a Node in its own process, as `defios serve` does for
the terminal (ADR 0034). It is a Wails v3 app: Go in this directory, a plain
HTML/JS frontend in `frontend/` with no build step. `internal/shell` holds the
app's state and is tested without a window.

## Build

```bash
# Linux: Wails' cgo backend needs GTK 4 and WebKitGTK 6.0 development packages
sudo apt-get install pkg-config libgtk-4-dev libwebkitgtk-6.0-dev
go build -tags desktop -o defios-desktop ./desktop

# Linux without a C toolchain: the libraries are loaded at run time
CGO_ENABLED=0 go build -tags purego -o defios-desktop ./desktop

# macOS (Xcode command line tools) and Windows
go build -o defios-desktop ./desktop
```

On Linux the package builds only with one of those tags, so `go test ./...`
needs no GTK. Running needs the GTK 4 and WebKitGTK 6.0 runtime libraries
(`libgtk-4-1`, `libwebkitgtk-6.0-4`).

`defios-desktop --dir <path>` opens the Node at that directory, as
`defios --dir` does; without it the Node directory resolves as the CLI's.

## What it keeps where

The app's own preferences (start at login, keychain, notifications, the last
served Group, the window size) are in `defios-desktop/prefs.json` under the OS
user config directory, never in the Node directory, so no Backup carries them.
With the keychain option on, the passphrase is in the OS keychain under the
service `DeFiOS`, keyed by Node ID. Start at login registers the app with
`--login`.

## Checking it by hand

The tray, start at login, keychain and notifications are the OS's, so each OS
needs checking by hand:

- Onboarding, on a fresh `--dir`: create a device (progress shows while the
  keys are made), join a tailnet on a real headscale, and form a Group with a
  second machine: one invites, the other accepts, and both land on Talk.
  Restore from a Backup through the file dialog, once with a wrong passphrase.
  From a served Group, "+ Add Group" asks for no passphrase.
- Unlock from the lock screen; `defios group list` in a terminal shows the
  Group served.
- Talk, with two devices: send in Group talk and see it on the other's left;
  "+ New Key" on one shows a provisional row on both, and Accept on the other
  turns it into a Key. Include an agent, mention it, watch its output stream,
  Cancel one chain and expand another's Transcript. Quit the other device:
  the header turns red, the banner says messages will queue, and Drop removes
  a queued one. The window's look is checked on the PR.
- Start the app while `defios serve` runs: it says DeFiOS is running elsewhere,
  then shows the lock screen once `serve` stops.
- Close the window: the tray icon stays and the Node keeps serving. The tray's
  Lock returns to the lock screen; Quit stops serving.
- Switch Group from the header without a passphrase.
- Restart: the lock screen preselects the last served Group.
- Settings, then log out and back in, for each combination:
  - start at login off: nothing runs;
  - on, keychain off: hidden in the tray, with a notification that it is locked;
  - on, keychain on: hidden in the tray, the last Group served;
  - turning the keychain off removes the `DeFiOS` entry from the keychain.
