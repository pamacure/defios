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

The app's own preferences (start at login, keychain, notifications and the
opt-in new-message notifications, the appearance, the last served Group, the window size,
when the app last wrote a Backup and whether a Key was minted since) are in `defios-desktop/prefs.json` under the OS
user config directory, never in the Node directory, so no Backup carries them.
On Linux the app icon is also written to `defios-desktop/notification-icon.png`
under the OS user cache directory, for notifications to show as their icon.
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
  Stop one chain (beside Send) while its text streams, and expand another's
  Transcript. Quit the other device: the header turns red, the banner says
  messages will queue, and Drop removes a queued one. Clear on one device
  says how many messages go and warns that agents forget them and that a
  Group with no "Delete old talk after" can get them back; once cleared,
  that transcript is empty there and unchanged on the other device. The window's look is
  checked on the PR.
- Sign Requests, with two devices and a Key: "+" proposes one (Fill, review,
  Propose) and its card shows on both, on the right only for the proposer.
  The other device gets a notification; clicking it opens the card. Refuse
  asks for a reason and warns a stance is final; Accept signs it, and
  Broadcast lists a receipt beneath on both. With the other device quit,
  Accept shows Queued until it is back. Add a Hook on `sign.request` with an
  agent Job: the notification says "@agent is on it", and a failing Job
  notifies again that the request needs you. Turn new-message notifications
  on in Settings: another device's message notifies.
- Settings, with two devices: rename this device in the Group, Lock and
  Unlock, and the other device's Settings lists the new name. A headscale
  URL such as `headscale.example` is refused beside its field and nothing
  changes. Devices shows "2 of 2 devices online", then 1 of 2 with the
  other quit. Mention an agent: its Token Usage goes up, shown apart from
  Node Usage ("Device Usage"). Choose a Group that is not online: no device shows online.
- Agents: add a Provider from a preset with its API key through "New
  Secret…", then add an agent on it. Include it on Talk, mention it, and it
  answers; change its SYSTEM.md and the next answer follows it. Grant an
  MCP Server with one of its Secrets, then revoke it: `defios agent show`
  agrees each time. Delete warns where the agent is included. A max turns
  of 0, or an API path such as `/v1/foo`, is refused beside its field and
  nothing changes. Choose a Group that is not online: its Providers show,
  and edits note they apply the next time it is.
- Vault, with two devices: "+ New Key" on Talk and accept it on the other;
  once it is made, "Back up now" shows above every page. Click it, save the
  file through the dialog, and the banner goes; the Vault page says when.
  Restore that file with `defios init --from-backup` and the same
  passphrase in an empty `--dir`. Saving over an existing file is refused.
  Change the passphrase: the device goes offline, comes back, and Lock then
  Unlock refuses the old one. With the keychain on, the next login unlocks
  with the new one. Each Secret lists its source and the fields that use
  it; Reveal asks for the passphrase each time and shows nothing for a
  wrong one. A Secret set through "New Secret…" on the Agents page lists
  here as In the Vault.
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
- Settings, Appearance: Light and Dark override the OS's setting at once,
  on every page and the lock screen, and stay after a restart; System
  follows the OS again.
