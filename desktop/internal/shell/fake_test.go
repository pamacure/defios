package shell_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"defios.com/desktop/internal/prefs"
	"defios.com/desktop/internal/shell"
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
		out = append(out, node.GroupSummary{Alias: g, Status: "unformed", Served: n.served == g || n.elsewhere})
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
	notes     []string
	shell     *shell.Shell
}

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
		Prefs:     r.prefs,
		Keychain:  r.keychain,
		Autostart: r.autostart,
		Notify: func(title, body string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.notes = append(r.notes, title)
		},
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
