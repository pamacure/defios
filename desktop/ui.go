//go:build windows || (darwin && (cgo || purego)) || (linux && (purego || desktop))

package main

import (
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"

	"defios.com/desktop/internal/shell"
)

// stateEvent carries each new shell.State to the frontend.
const stateEvent = "shell:state"

// UI is the service the frontend calls, as main.UI.<Method>. Every method
// returns the State after it; the same State also arrives as stateEvent.
type UI struct {
	shell *shell.Shell
	app   *application.App
	win   *application.WebviewWindow

	mu       sync.Mutex
	lockItem *application.MenuItem
	traySeq  uint64 // the State the tray shows
}

func (u *UI) State() shell.State                          { return u.shell.State() }
func (u *UI) Refresh() shell.State                        { return u.shell.Refresh() }
func (u *UI) Unlock(group, passphrase string) shell.State { return u.shell.Unlock(group, passphrase) }
func (u *UI) Lock() shell.State                           { return u.shell.Lock() }
func (u *UI) Switch(group string) shell.State             { return u.shell.Switch(group) }
func (u *UI) SetEditGroup(group string) shell.State       { return u.shell.SetEditGroup(group) }
func (u *UI) SetStartAtLogin(on bool) shell.State         { return u.shell.SetStartAtLogin(on) }
func (u *UI) SetKeychain(on bool) shell.State             { return u.shell.SetKeychain(on) }
func (u *UI) SetNotifications(on bool) shell.State        { return u.shell.SetNotifications(on) }

// Onboarding.

func (u *UI) CheckDisplayName(name string) *shell.Problem { return shell.CheckDisplayName(name) }
func (u *UI) Create(name, passphrase string) shell.State  { return u.shell.Create(name, passphrase) }
func (u *UI) CheckBackup(file string) shell.State         { return u.shell.CheckBackup(file) }
func (u *UI) Restore(file, passphrase string) shell.State { return u.shell.Restore(file, passphrase) }
func (u *UI) AddGroup(alias, headscaleURL, authKey, passphrase string) shell.State {
	return u.shell.AddGroup(alias, headscaleURL, authKey, passphrase)
}
func (u *UI) Discover() shell.State                { return u.shell.Discover() }
func (u *UI) Propose(nodeIDs []string) shell.State { return u.shell.Propose(nodeIDs) }
func (u *UI) Accept() shell.State                  { return u.shell.Accept() }
func (u *UI) Decline(reason string) shell.State    { return u.shell.Decline(reason) }

// ChooseBackup asks for a Backup file with the OS's file dialog; "" when
// none was chosen.
func (u *UI) ChooseBackup() (string, error) {
	d := u.app.Dialog.OpenFile().
		SetTitle("Choose a DeFiOS Backup").
		CanChooseFiles(true).
		CanChooseDirectories(false)
	if u.win != nil {
		d = d.AttachToWindow(u.win)
	}
	return d.PromptForSingleSelection()
}

// show brings the window back from the tray.
func (u *UI) show() {
	if u.win == nil {
		return
	}
	u.win.Show()
	u.win.Restore()
	u.win.Focus()
}

// trayFor enables the tray's Lock item while a Group is served.
func (u *UI) trayFor(st shell.State) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if st.Seq < u.traySeq {
		return // States can arrive out of order
	}
	u.traySeq = st.Seq
	if u.lockItem != nil {
		u.lockItem.SetEnabled(st.Served != "")
	}
}
