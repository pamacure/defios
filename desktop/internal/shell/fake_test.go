package shell_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"defios.com/desktop/internal/prefs"
	"defios.com/desktop/internal/shell"
	"defios.com/internal/group"
	"defios.com/internal/node"
	"defios.com/internal/vault"
)

const pw = "correct horse"

// fakeNode is a Node whose Vault opens with pw and that serves in memory.
type fakeNode struct {
	mu        sync.Mutex
	name      string
	groups    []string
	elsewhere bool // another process serves it
	served    string
	pass      string
	unlocks   int
	status    node.Status // the last one published
	subs      []chan node.Event

	// Onboarding. A Group not in forms is valid.
	forms       map[string]*fakeForm
	secrets     map[string]map[string]string
	discovered  []node.DiscoveredNode
	unreached   map[string]bool // Node IDs a proposal does not reach
	undelivered []string        // what Accept reports undelivered
	proposed    [][]string      // each Propose's Node IDs

	talk *fakeTalk

	// Settings: each Group's, as GroupSettings reads them, and the
	// Token Usage its Transcripts sum to.
	settings map[string]node.GroupSettings
	tokens   map[string][]node.HandleTokens
	// secretErr, when set, is what SetSecret fails with.
	secretErr error

	// written are the files each Backup wrote.
	written []string
}

// fakeForm is a Group's formation: unformed while proposal is nil.
type fakeForm struct {
	groupID   string
	initiator string
	roster    []node.GroupNode
}

func (f *fakeForm) status() string {
	if f.initiator == "" {
		return node.StatusUnformed
	}
	for _, m := range f.roster {
		if !m.Accepted {
			return "pending"
		}
	}
	return "valid"
}

func (n *fakeNode) form(group string) *fakeForm {
	if n.forms == nil {
		n.forms = map[string]*fakeForm{}
	}
	if n.forms[group] == nil {
		n.forms[group] = &fakeForm{}
	}
	return n.forms[group]
}

func (n *fakeNode) groupStatus(group string) string {
	if f, ok := n.forms[group]; ok {
		return f.status()
	}
	return "valid"
}

func (n *fakeNode) CheckPassphrase(passphrase string) error {
	if passphrase != pw {
		return vault.ErrBadPassphrase
	}
	return nil
}

func (n *fakeNode) NewGroup(spec node.GroupSpec) (node.GroupConfig, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if spec.Alias == "" || strings.ContainsAny(spec.Alias, " ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		return node.GroupConfig{}, &node.InvalidError{Err: errors.New("not a valid alias")}
	}
	if !strings.HasPrefix(spec.HeadscaleURL, "https://") {
		return node.GroupConfig{}, &node.InvalidError{Err: errors.New("not a headscale URL")}
	}
	for _, g := range n.groups {
		if g == spec.Alias {
			return node.GroupConfig{}, node.ErrGroupExists
		}
	}
	n.groups = append(n.groups, spec.Alias)
	n.form(spec.Alias)
	return node.GroupConfig{Alias: spec.Alias, HeadscaleURL: spec.HeadscaleURL}, nil
}

func (n *fakeNode) SetSecret(_ context.Context, group, name, value string, passphrase node.Passphrase) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	phrase := n.pass
	if phrase == "" {
		phrase, _, _ = passphrase()
	}
	if phrase != pw {
		return vault.ErrBadPassphrase
	}
	if n.secretErr != nil {
		return n.secretErr
	}
	if n.secrets == nil {
		n.secrets = map[string]map[string]string{}
	}
	if n.secrets[group] == nil {
		n.secrets[group] = map[string]string{}
	}
	n.secrets[group][name] = value
	return nil
}

func (n *fakeNode) secret(group, name string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.secrets[group][name]
}

func (n *fakeNode) servedOrErr(group string) error {
	if n.served != group {
		return node.ErrNotServed
	}
	return nil
}

func (n *fakeNode) Discover(_ context.Context, group string) ([]node.DiscoveredNode, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.servedOrErr(group); err != nil {
		return nil, err
	}
	return n.discovered, nil
}

func (n *fakeNode) Propose(_ context.Context, group string, ids []string) (node.Formation, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.servedOrErr(group); err != nil {
		return node.Formation{}, err
	}
	n.proposed = append(n.proposed, ids)
	var missed []string
	for _, id := range ids {
		if n.unreached[id] {
			missed = append(missed, id)
		}
	}
	if missed != nil {
		return node.Formation{}, &node.NotReachedError{NodeIDs: missed, Err: errors.New("not reached")}
	}
	f := n.form(group)
	f.groupID, f.initiator, f.roster = "9r0up", n.NodeID(), nil
	for _, id := range ids {
		f.roster = append(f.roster, node.GroupNode{NodeID: id, DisplayName: "name-" + id, Accepted: id == n.NodeID()})
	}
	return node.Formation{GroupID: f.groupID, Status: f.status()}, nil
}

// receive is another Node, from, proposing group's Roster of ids.
func (n *fakeNode) receive(group, from string, ids ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	f := n.form(group)
	f.groupID, f.initiator, f.roster = "9r0up", from, nil
	for _, id := range ids {
		f.roster = append(f.roster, node.GroupNode{NodeID: id, DisplayName: "name-" + id, Accepted: id == from})
	}
	for _, ch := range n.subs {
		ch <- node.Event{Proposal: &node.Proposal{Group: group, GroupID: f.groupID, Initiator: from}}
	}
}

// acceptBy is Node id accepting group's proposal.
func (n *fakeNode) acceptBy(group, id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	f := n.form(group)
	for i := range f.roster {
		if f.roster[i].NodeID == id {
			f.roster[i].Accepted = true
		}
	}
	for _, ch := range n.subs {
		ch <- node.Event{Accepted: &node.Accepted{Group: group, GroupID: f.groupID, NodeID: id, Status: f.status()}}
	}
}

func (n *fakeNode) InspectGroup(_ context.Context, alias string) (node.GroupDetail, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	f := n.forms[alias]
	if f == nil || f.initiator == "" {
		return node.GroupDetail{}, group.ErrNoGroup
	}
	return node.GroupDetail{Alias: alias, GroupID: f.groupID, Initiator: f.initiator, Roster: append([]node.GroupNode(nil), f.roster...)}, nil
}

func (n *fakeNode) Accept(_ context.Context, group, groupID string) (node.Formation, error) {
	n.mu.Lock()
	f := n.forms[group]
	n.mu.Unlock()
	if f == nil || f.groupID != groupID {
		return node.Formation{}, errors.New("group: no such proposal")
	}
	n.acceptBy(group, n.NodeID())
	n.mu.Lock()
	defer n.mu.Unlock()
	return node.Formation{GroupID: groupID, Status: f.status(), Undelivered: n.undelivered}, nil
}

func (n *fakeNode) Decline(_ context.Context, group, groupID, reason string) (node.Formation, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	f := n.forms[group]
	if f == nil || f.groupID != groupID {
		return node.Formation{}, errors.New("group: no such proposal")
	}
	*f = fakeForm{}
	return node.Formation{GroupID: groupID, Status: "declined"}, nil
}

func (n *fakeNode) NodeID() string { return "n0de" }
func (n *fakeNode) DisplayName() (string, error) {
	return n.name, nil
}

func (n *fakeNode) Check(group string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.check(group)
}

func (n *fakeNode) check(group string) error {
	for _, g := range n.groups {
		if g == group {
			if n.elsewhere {
				return node.ErrServedElsewhere
			}
			return nil
		}
	}
	return node.ErrUnknownGroup
}

func (n *fakeNode) Unlock(passphrase, group string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.served != "" {
		return node.ErrUnlocked
	}
	if err := n.check(group); err != nil {
		return err
	}
	if passphrase != pw {
		return vault.ErrBadPassphrase
	}
	n.unlocks++
	n.served, n.pass = group, passphrase
	n.publish(node.Status{Group: group, Serving: true})
	return nil
}

func (n *fakeNode) Lock() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.served, n.pass = "", ""
	n.publish(node.Status{})
	return nil
}

func (n *fakeNode) Switch(group string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.served == "" {
		return node.ErrLocked
	}
	if err := n.check(group); err != nil {
		n.served, n.pass = "", ""
		n.publish(node.Status{})
		return err
	}
	n.served = group
	n.publish(node.Status{Group: group, Serving: true})
	return nil
}

// stop is serving stopping on its own, with err.
func (n *fakeNode) stop(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.served, n.pass = "", ""
	n.publish(node.Status{Err: err})
}

func (n *fakeNode) setElsewhere(v bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.elsewhere = v
}

func (n *fakeNode) Status() node.Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.status
}

func (n *fakeNode) Subscribe(ctx context.Context) <-chan node.Event {
	n.mu.Lock()
	defer n.mu.Unlock()
	ch := make(chan node.Event, 64)
	st := n.status
	ch <- node.Event{Status: &st}
	n.subs = append(n.subs, ch)
	return ch
}

func (n *fakeNode) publish(st node.Status) {
	n.status = st
	for _, ch := range n.subs {
		ch <- node.Event{Status: &st}
	}
}

func (n *fakeNode) Groups(context.Context) ([]node.GroupSummary, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []node.GroupSummary
	for _, g := range n.groups {
		out = append(out, node.GroupSummary{Alias: g, Status: n.groupStatus(g), Served: n.served == g || n.elsewhere})
	}
	return out, nil
}

type memPrefs struct {
	mu sync.Mutex
	p  prefs.Prefs
}

func (m *memPrefs) Load() (prefs.Prefs, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.p, nil
}

func (m *memPrefs) Save(p prefs.Prefs) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.p = p
	return nil
}

func (m *memPrefs) get() prefs.Prefs {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.p
}

type memKeychain struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *memKeychain) Get(account string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[account]
	if !ok {
		return "", shell.ErrNoSecret
	}
	return v, nil
}

func (k *memKeychain) Set(account, secret string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = map[string]string{}
	}
	k.m[account] = secret
	return nil
}

func (k *memKeychain) Delete(account string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.m[account]; !ok {
		return shell.ErrNoSecret
	}
	delete(k.m, account)
	return nil
}

func (k *memKeychain) has(account string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, ok := k.m[account]
	return ok
}

type fakeAutostart struct {
	mu sync.Mutex
	on bool
}

func (a *fakeAutostart) Enable() error  { a.mu.Lock(); a.on = true; a.mu.Unlock(); return nil }
func (a *fakeAutostart) Disable() error { a.mu.Lock(); a.on = false; a.mu.Unlock(); return nil }
func (a *fakeAutostart) enabled() bool  { a.mu.Lock(); defer a.mu.Unlock(); return a.on }

// rig is a Shell over fakes.
type rig struct {
	t         *testing.T
	node      *fakeNode
	noNode    bool
	prefs     *memPrefs
	keychain  *memKeychain
	autostart *fakeAutostart
	mu        sync.Mutex
	notes     []shell.Notice
	shell     *shell.Shell
	// backupGroups are the Groups the Backup at backupFile holds.
	backupGroups []string
	progress     []string // the State's Progress as Create reported each line
}

const backupFile = "/backups/laptop.backup"

func newRig(t *testing.T, groups ...string) *rig {
	r := &rig{
		t:         t,
		node:      &fakeNode{name: "laptop", groups: groups},
		prefs:     &memPrefs{},
		keychain:  &memKeychain{},
		autostart: &fakeAutostart{},
	}
	return r
}

func (r *rig) start(atLogin bool) *shell.Shell {
	r.shell = shell.New(shell.Deps{
		Open: func() (shell.Node, error) {
			if r.noNode {
				return nil, node.ErrNoNode
			}
			return r.node, nil
		},
		Create: func(c node.Creation) error {
			if !r.noNode {
				return node.ErrNodeExists
			}
			for _, line := range []string{"generating ECDSA preparams", "done"} {
				c.Progress(line)
				r.mu.Lock()
				r.progress = append(r.progress, strings.Join(r.shell.State().Progress, "|"))
				r.mu.Unlock()
			}
			r.noNode, r.node.name, r.node.groups = false, c.DisplayName, nil
			return nil
		},
		CheckRestore: func(file string) error {
			if file != backupFile {
				return &node.NotAFileError{Path: file}
			}
			return nil
		},
		Restore: func(_ context.Context, file, passphrase string) (node.Restored, error) {
			if passphrase != pw {
				return node.Restored{}, vault.ErrBadPassphrase
			}
			r.noNode, r.node.groups = false, r.backupGroups
			return node.Restored{NodeID: "n0de"}, nil
		},
		Prefs:     r.prefs,
		Keychain:  r.keychain,
		Autostart: r.autostart,
		Notify: func(n shell.Notice) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.notes = append(r.notes, n)
		},
		HookGrace:  20 * time.Millisecond,
		RetryEvery: 5 * time.Millisecond,
	})
	r.shell.Start(atLogin)
	r.t.Cleanup(r.shell.Close)
	return r.shell
}

func (r *rig) notifications() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.notes)
}

// noticesNow are the notifications posted so far.
func (r *rig) noticesNow() []shell.Notice {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]shell.Notice(nil), r.notes...)
}

// eventually waits for the Shell's state to satisfy ok.
func eventually(t *testing.T, s *shell.Shell, ok func(shell.State) bool) shell.State {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		st := s.State()
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("state never matched; last: %+v", st)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func view(v shell.View) func(shell.State) bool {
	return func(st shell.State) bool { return st.View == v }
}

func problem(st shell.State) string {
	if st.Problem == nil {
		return ""
	}
	return st.Problem.Code
}

var errBoom = errors.New("tailnet went away")
