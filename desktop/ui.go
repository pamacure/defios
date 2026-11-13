//go:build windows || (darwin && (cgo || purego)) || (linux && (purego || desktop))

package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/node"
)

// stateEvent carries each new shell.State to the frontend, talkEvent
// each new shell.TalkState, and noticeEvent a clicked notification's
// shell.Notice, to show the page it is about.
const (
	stateEvent  = "shell:state"
	talkEvent   = "talk:state"
	noticeEvent = "app:notice"
)

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
func (u *UI) SetMessageNotifications(on bool) shell.State {
	return u.shell.SetMessageNotifications(on)
}

// Settings: the edit-target Group's part.

func (u *UI) GroupSettings() shell.GroupSettingsView { return u.shell.GroupSettings() }
func (u *UI) SaveGroupSettings(d shell.SettingsDraft) shell.GroupSettingsView {
	return u.shell.SaveGroupSettings(d)
}

// Agents: this Node's Agent Profiles and the edit-target Group's
// Providers.

func (u *UI) Agents(open string) shell.AgentsView { return u.shell.Agents(open) }
func (u *UI) SaveAgentProfile(d shell.ProfileDraft) shell.AgentsView {
	return u.shell.SaveAgentProfile(d)
}
func (u *UI) DeleteAgentProfile(handle string) shell.AgentsView {
	return u.shell.DeleteAgentProfile(handle)
}
func (u *UI) GrantAgent(handle, item string, secrets []string) shell.AgentsView {
	return u.shell.GrantAgent(handle, item, secrets)
}
func (u *UI) RevokeAgent(handle, item string) shell.AgentsView {
	return u.shell.RevokeAgent(handle, item)
}
func (u *UI) SaveProvider(d shell.ProviderDraft) shell.AgentsView {
	return u.shell.SaveProvider(d)
}
func (u *UI) RemoveProvider(alias string) shell.AgentsView { return u.shell.RemoveProvider(alias) }

// The shared Secret field: the edit-target Group's Secrets.

func (u *UI) Secrets() shell.SecretsView                     { return u.shell.Secrets() }
func (u *UI) NewSecret(name, value string) shell.SecretsView { return u.shell.NewSecret(name, value) }

// Vault: this device's Backup and passphrase, and the edit-target
// Group's Secrets.

func (u *UI) Vault() shell.VaultView               { return u.shell.Vault() }
func (u *UI) BackupTo(file string) shell.VaultView { return u.shell.BackupTo(file) }
func (u *UI) RemoveVaultSecret(name string) shell.VaultView {
	return u.shell.RemoveVaultSecret(name)
}
func (u *UI) SaveVaultSecret(d shell.SecretDraft) shell.VaultView {
	return u.shell.SaveVaultSecret(d)
}
func (u *UI) ChangePassphrase(old, next, again string) shell.VaultView {
	return u.shell.ChangePassphrase(old, next, again)
}
func (u *UI) RevealSecret(name, passphrase string) shell.RevealView {
	return u.shell.RevealSecret(name, passphrase)
}

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

// Talk.

func (u *UI) Talk() shell.TalkState                         { return u.shell.Talk() }
func (u *UI) OpenConversation(conv string) shell.TalkState  { return u.shell.OpenConversation(conv) }
func (u *UI) SendMessage(text string) shell.TalkState       { return u.shell.SendMessage(text) }
func (u *UI) CancelChain(chain string) shell.TalkState      { return u.shell.CancelChain(chain) }
func (u *UI) DropQueued(msgID string) shell.TalkState       { return u.shell.DropQueued(msgID) }
func (u *UI) SetKeyAlias(alias string) shell.TalkState      { return u.shell.SetKeyAlias(alias) }
func (u *UI) AcceptKeygen(id, alias string) shell.TalkState { return u.shell.AcceptKeygen(id, alias) }
func (u *UI) AbortKeygen(id, reason string) shell.TalkState { return u.shell.AbortKeygen(id, reason) }
func (u *UI) NewKey(curve string, t int, alias string) shell.TalkState {
	return u.shell.NewKey(curve, t, alias)
}
func (u *UI) SetAgentIncluded(handle string, on bool) shell.TalkState {
	return u.shell.SetAgentIncluded(handle, on)
}

func (u *UI) Transcript(msgID string) shell.TranscriptView { return u.shell.Transcript(msgID) }

// Sign Requests.

func (u *UI) AcceptSign(msgID string) shell.TalkState { return u.shell.AcceptSign(msgID) }
func (u *UI) RefuseSign(msgID, reason string) shell.TalkState {
	return u.shell.RefuseSign(msgID, reason)
}
func (u *UI) BroadcastSign(msgID string) shell.TalkState    { return u.shell.BroadcastSign(msgID) }
func (u *UI) SignForm(conv string) shell.SignFormView       { return u.shell.SignForm(conv) }
func (u *UI) FillSign(d shell.SignDraft) shell.SignFormView { return u.shell.FillSign(d) }
func (u *UI) FocusSign(group, conv, msgID string) shell.TalkState {
	return u.shell.FocusSign(group, conv, msgID)
}
func (u *UI) ProposeSign(conv, reason string, txs []node.TxSpec) shell.SignFormView {
	return u.shell.ProposeSign(conv, reason, txs)
}
func (u *UI) RunTranscript(conv, chain, handle string) shell.TranscriptView {
	return u.shell.RunTranscript(conv, chain, handle)
}

// opened shows the window on what clicked notice n is about: its
// Conversation and card, while its Group is the one served.
func (u *UI) opened(n shell.Notice) {
	u.show()
	if n.Group != "" {
		u.shell.FocusSign(n.Group, n.Conversation, n.SignRequestID)
	}
	u.app.Event.Emit(noticeEvent, n)
}

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

// ChooseBackupDestination asks where to write a new Backup with the OS's
// save dialog; "" when none was chosen.
func (u *UI) ChooseBackupDestination() (string, error) {
	name := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '-'
		}
		return r
	}, u.shell.State().Node.Name)
	if name == "" {
		name = "device"
	}
	d := u.app.Dialog.SaveFile().
		SetMessage("Save a Backup of this device").
		SetFilename(fmt.Sprintf("defios-%s-%s.backup", name, time.Now().Format("2006-01-02"))).
		CanCreateDirectories(true)
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
