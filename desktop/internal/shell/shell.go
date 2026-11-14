// Package shell is the desktop app's state, apart from any window: which
// screen it shows, the Node it hosts through internal/node (ADR 0034), and
// the app's own preferences, keychain and start at login. The Wails layer
// binds a Shell to the frontend and renders each State it reports.
package shell

import (
	"context"
	"errors"
	"sync"
	"time"

	"defios.com/desktop/internal/prefs"
	"defios.com/internal/node"
)

// Node is what the shell needs of a Node; *node.Host is one.
type Node interface {
	NodeID() string
	DisplayName() (string, error)
	Check(group string) error
	Unlock(passphrase, group string) error
	Lock() error
	Switch(group string) error
	Status() node.Status
	Subscribe(ctx context.Context) <-chan node.Event
	Groups(ctx context.Context) ([]node.GroupSummary, error)

	// Onboarding.
	CheckPassphrase(passphrase string) error
	NewGroup(spec node.GroupSpec) (node.GroupConfig, error)
	SetSecret(ctx context.Context, group, name, value string, passphrase node.Passphrase) error
	InspectGroup(ctx context.Context, group string) (node.GroupDetail, error)
	Discover(ctx context.Context, group string) ([]node.DiscoveredNode, error)
	Propose(ctx context.Context, group string, nodeIDs []string) (node.Formation, error)
	Accept(ctx context.Context, group, groupID string) (node.Formation, error)
	Decline(ctx context.Context, group, groupID, reason string) (node.Formation, error)

	TalkNode
	SettingsNode
	AgentsNode
	VaultNode
	TriggersNode
}

// PrefStore loads and saves the app's preferences.
type PrefStore interface {
	Load() (prefs.Prefs, error)
	Save(prefs.Prefs) error
}

// ErrNoSecret: the keychain holds no passphrase for this Node.
var ErrNoSecret = errors.New("shell: no passphrase in the keychain")

// Keychain is the OS keychain, one secret per account. Get and Delete
// return ErrNoSecret when it holds none.
type Keychain interface {
	Get(account string) (string, error)
	Set(account, secret string) error
	Delete(account string) error
}

// Autostart registers the app to start at login.
type Autostart interface {
	Enable() error
	Disable() error
}

// Deps are what a Shell runs on.
type Deps struct {
	// Open opens the Node directory; node.ErrNoNode when it holds none.
	Open func() (Node, error)
	// Create mints a Node in the directory Open opens, as node.Create.
	Create func(node.Creation) error
	// CheckRestore and Restore bring back the Node a Backup file holds
	// into that directory, as node.CheckRestore and node.Restore.
	CheckRestore func(file string) error
	Restore      func(ctx context.Context, file, passphrase string) (node.Restored, error)
	Prefs        PrefStore
	Keychain     Keychain
	Autostart    Autostart
	// Notify posts an OS notification.
	Notify func(Notice)
	// HookGrace is how long a new Sign Request's notification waits for a
	// Hook of this device's to fire on it; zero is 2 seconds.
	HookGrace time.Duration
	// OnChange receives every new State, from any goroutine.
	OnChange func(State)
	// OnTalk receives every new TalkState, from any goroutine.
	OnTalk func(TalkState)
	// RetryEvery is how often a Node another process serves is checked
	// again; zero is 2 seconds.
	RetryEvery time.Duration
	// ConnectWindow is how long a Group just served may stay Connecting,
	// waiting for every Roster device to be reachable; zero is 15
	// seconds.
	ConnectWindow time.Duration
}

// View is the screen the app shows.
type View string

const (
	ViewStarting        View = "starting"
	ViewNoNode          View = "no-node"  // onboarding step 1
	ViewCreating        View = "creating" // step 1, Create running
	ViewRestoring       View = "restoring"
	ViewNoGroup         View = "no-group" // onboarding step 2
	ViewLocked          View = "locked"
	ViewUnlocking       View = "unlocking"
	ViewServedElsewhere View = "served-elsewhere"
	ViewServed          View = "served"
	// ViewForming: the served Group has no valid Record yet; onboarding
	// step 3 forms it.
	ViewForming View = "forming"
)

// State is everything the frontend renders.
type State struct {
	// Seq orders States: a frontend ignores one older than what it shows.
	Seq  uint64
	View View
	// Node is the Node the app hosts.
	Node   NodeInfo
	Groups []Group
	// Selected is the Group the lock screen preselects.
	Selected string
	// Served is the Group served now; "" when locked.
	Served string
	// EditGroup is the Group the section pages edit, and
	// EditGroupServed whether it is the one served.
	EditGroup       string
	EditGroupServed bool
	// The served Group's reachability.
	TailnetUp         bool
	Online            int // Roster Nodes reachable, this one included
	RosterSize        int
	MajorityReachable bool
	// Connecting: the Group was just served and not every Roster device
	// is reachable yet, so the app waits on them before it shows the
	// Group, until Deps.ConnectWindow passes or ContinueNow.
	Connecting bool
	// Problem is what went wrong last, for the screen to show.
	Problem *Problem
	Prefs   Prefs

	// Progress is the latest lines Create reported, while it runs.
	Progress []string
	// PassphraseHeld: the app holds the passphrase, so adding a Group
	// needs none typed. It does while a Group serves, and from Create or
	// Restore until the first Group serves.
	PassphraseHeld bool
	// Forming is the served Group's formation, in ViewForming.
	Forming *Forming
	// Restored is set once a Node comes back from a Backup, until a
	// Group serves: the steps that are the person's own.
	Restored *Restored
	// BackupDue: a Key was minted since the last Backup taken from the
	// app, so the app shows "Back up now".
	BackupDue bool
	// KeysMinted counts the Keys minted since the app started, so a
	// dismissed "Back up now" shows again for the next one.
	KeysMinted int
}

// Restored is what follows a Restore.
type Restored struct {
	// PluginsProblem is why the Plugins were not fetched again, if not.
	PluginsProblem string
}

// NodeInfo names the Node.
type NodeInfo struct {
	Name string
	ID   string
}

// Group is one of the Node's Groups.
type Group struct {
	Alias  string
	Status string // unformed | pending | valid | error
	Served bool   // served now, by this app or another process
}

// Prefs are the preferences the Settings page toggles.
type Prefs struct {
	StartAtLogin  bool
	Keychain      bool
	Notifications bool
	// MessageNotifications also notifies of each new message.
	MessageNotifications bool
	// Theme is "system", "light" or "dark".
	Theme string
}

// Shell is the app's state. Its methods are safe from any goroutine; each
// returns the State after it.
type Shell struct {
	d Deps

	// op serializes the operations a person triggers, so an Unlock never
	// overlaps a Lock; mu guards the fields below and is never held while
	// the Node works.
	op sync.Mutex
	mu sync.Mutex
	// saveMu serializes writes of the preferences file, each of the
	// latest preferences, so an older copy never lands after a newer one.
	saveMu sync.Mutex
	n      Node
	st     State
	p      prefs.Prefs
	// pass is the passphrase the served Group was unlocked with, held
	// only while it serves, to put in the keychain when that is turned on,
	// and to add a Group with. Create and Restore hold theirs too, until
	// the first Group serves.
	pass string
	// edit is the edit-target Group a person chose; "" follows the served.
	edit string
	// back is the Group served before the one served now, to go back to
	// rather than form that one now.
	back string
	// connects counts the times a Group was served, so the end of one's
	// ConnectWindow never ends a later one's Connecting.
	connects int
	// loginUnlock: unlock from the keychain once another process stops
	// serving this Node.
	loginUnlock bool
	retrying    bool

	ctx      context.Context
	cancel   context.CancelFunc
	stopNode context.CancelFunc // ends the current Node's subscription

	talk   *talk
	notify *notifier
}

// New is a Shell that shows nothing until Start.
func New(d Deps) *Shell {
	if d.RetryEvery <= 0 {
		d.RetryEvery = 2 * time.Second
	}
	if d.ConnectWindow <= 0 {
		d.ConnectWindow = 15 * time.Second
	}
	if d.Notify == nil {
		d.Notify = func(Notice) {}
	}
	if d.HookGrace <= 0 {
		d.HookGrace = 2 * time.Second
	}
	if d.OnChange == nil {
		d.OnChange = func(State) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Shell{d: d, ctx: ctx, cancel: cancel, st: State{View: ViewStarting}, talk: newTalk(ctx, d.OnTalk)}
	s.notify = newNotifier(s)
	return s
}

// Start opens the Node and routes to the screen it calls for. atLogin
// means the OS started the app at login: with start at login on, it then
// unlocks from the keychain if that is on, and otherwise notifies that
// the Node is locked.
func (s *Shell) Start(atLogin bool) {
	s.op.Lock()
	defer s.op.Unlock()
	p, err := s.d.Prefs.Load()
	s.mu.Lock()
	s.p = p
	s.mu.Unlock()
	if err != nil {
		s.fail(appProblem("prefs", err))
	}
	s.open()
	if !atLogin || !p.StartAtLogin {
		return
	}
	switch s.State().View {
	case ViewLocked:
		s.unlockAtLogin()
	case ViewServedElsewhere:
		s.mu.Lock()
		s.loginUnlock = p.Keychain
		s.mu.Unlock()
	}
}

// Refresh opens the Node again, as after onboarding in the CLI. It does
// nothing while a Group is served.
func (s *Shell) Refresh() State {
	s.op.Lock()
	defer s.op.Unlock()
	if s.State().Served == "" {
		s.open()
	}
	return s.State()
}

// open (re)opens the Node and routes. s.op is held.
func (s *Shell) open() {
	s.mu.Lock()
	if s.stopNode != nil {
		s.stopNode()
		s.stopNode = nil
	}
	s.mu.Unlock()
	n, err := s.d.Open()
	if errors.Is(err, node.ErrNoNode) {
		s.set(func(st *State) { st.View, st.Problem = ViewNoNode, nil })
		return
	} else if err != nil {
		s.set(func(st *State) { st.View, st.Problem = ViewNoNode, problemOf(err) })
		return
	}
	name, err := n.DisplayName()
	if err != nil {
		name = ""
	}
	ctx, stop := context.WithCancel(s.ctx)
	s.mu.Lock()
	s.n, s.stopNode = n, stop
	s.st.Node = NodeInfo{Name: name, ID: n.NodeID()}
	s.mu.Unlock()
	// Subscribed here, not in follow, so no event after open is missed.
	go s.follow(ctx, n, n.Subscribe(ctx))
	s.route(nil)
}

// route shows the screen for a Node that serves nothing in this app, with
// problem.
func (s *Shell) route(problem *Problem) {
	n := s.node()
	groups, err := s.groups(n)
	if err != nil {
		s.set(func(st *State) { st.View, st.Problem = ViewLocked, problemOf(err) })
		return
	}
	if len(groups) == 0 {
		s.set(func(st *State) { st.View, st.Groups, st.Problem = ViewNoGroup, nil, problem })
		return
	}
	selected := s.selected(groups)
	if errors.Is(n.Check(selected), node.ErrServedElsewhere) {
		s.servedElsewhere(groups, selected)
		return
	}
	s.set(func(st *State) {
		st.View, st.Groups, st.Selected, st.Served, st.Problem = ViewLocked, groups, selected, "", problem
	})
}

func (s *Shell) node() Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *Shell) groups(n Node) ([]Group, error) {
	sums, err := n.Groups(s.ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Group, 0, len(sums))
	for _, g := range sums {
		out = append(out, Group{Alias: g.Alias, Status: g.Status, Served: g.Served})
	}
	return out, nil
}

// selected is the Group the lock screen preselects: the last served, if
// the Node still has it, else the first.
func (s *Shell) selected(groups []Group) string {
	s.mu.Lock()
	last := s.p.LastGroup
	s.mu.Unlock()
	for _, g := range groups {
		if g.Alias == last {
			return last
		}
	}
	return groups[0].Alias
}

// servedElsewhere shows that another process serves the Node, and checks
// again until it stops.
func (s *Shell) servedElsewhere(groups []Group, selected string) {
	s.mu.Lock()
	start := !s.retrying
	s.retrying = true
	s.mu.Unlock()
	s.set(func(st *State) {
		st.View, st.Groups, st.Selected, st.Served, st.Problem = ViewServedElsewhere, groups, selected, "", nil
	})
	if start {
		go s.retry()
	}
}

func (s *Shell) retry() {
	defer func() {
		s.mu.Lock()
		s.retrying, s.loginUnlock = false, false
		s.mu.Unlock()
	}()
	tick := time.NewTicker(s.d.RetryEvery)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
		}
		if errors.Is(s.node().Check(s.State().Selected), node.ErrServedElsewhere) {
			continue
		}
		s.op.Lock()
		if s.State().View != ViewServedElsewhere {
			// Something else moved the app on meanwhile.
			s.op.Unlock()
			return
		}
		s.mu.Lock()
		login := s.loginUnlock
		s.loginUnlock, s.retrying = false, false
		s.mu.Unlock()
		s.route(nil)
		if login && s.State().View == ViewLocked {
			s.unlockAtLogin()
		}
		s.op.Unlock()
		return
	}
}

// unlockAtLogin unlocks the last served Group from the keychain, if that
// is on, else notifies that the Node is locked. s.op is held.
func (s *Shell) unlockAtLogin() {
	s.mu.Lock()
	p, id := s.p, s.st.Node.ID
	s.mu.Unlock()
	if p.Keychain && p.LastGroup != "" {
		if pass, err := s.d.Keychain.Get(id); err == nil {
			if s.unlock(p.LastGroup, pass).View == ViewServed {
				return
			}
		}
	}
	if p.Notifications() {
		s.d.Notify(Notice{Title: "DeFiOS is locked", Body: "Open DeFiOS and enter your passphrase to bring this device online."})
	}
}

// follow applies the Node's status as it changes, until ctx is done.
func (s *Shell) follow(ctx context.Context, n Node, events <-chan node.Event) {
	for ev := range events {
		s.talk.event(n, ev)
		s.notify.event(s.State().Served, n.NodeID(), ev)
		s.keyMinted(s.State().Served, ev.Session)
		// A proposal or an acceptance moves step 3 on, as the status does.
		if ev.Status == nil && ev.Proposal == nil && ev.Accepted == nil {
			continue
		}
		// Applied under s.op, so never in the middle of an Unlock, Lock
		// or Switch, and off this goroutine, so the Node's events never
		// wait on one. The status is read then, not taken from the event:
		// a stale event must not undo a Lock that came after it.
		go func() {
			s.op.Lock()
			defer s.op.Unlock()
			if ctx.Err() == nil && s.node() == n {
				s.apply(n.Status())
			}
		}()
	}
}

// apply shows st, the Node's status. s.op is held.
func (s *Shell) apply(st node.Status) {
	if st.Serving {
		// The served Group changed only on Unlock or Switch, not on each
		// status refresh: remember it and list the Groups again then, and
		// on each refresh while it forms.
		s.mu.Lock()
		changed := s.st.View != ViewServed || s.st.Served != st.Group
		groups := s.st.Groups
		remember := s.p.LastGroup != st.Group
		s.p.LastGroup = st.Group
		s.mu.Unlock()
		if remember {
			s.persist()
		}
		if changed {
			if listed, err := s.groups(s.node()); err == nil {
				groups = listed
			}
		}
		forming := s.formingOf(st.Group, groups)
		connect := changed && forming == nil
		s.set(func(v *State) {
			if forming != nil && v.Forming != nil && v.Forming.Group == forming.Group {
				// Discover writes these without s.op: keep what it found
				// since formingOf copied them.
				forming.Discovered, forming.DiscoverProblem = v.Forming.Discovered, v.Forming.DiscoverProblem
			}
			v.View, v.Served, v.Groups, v.Forming, v.Restored = ViewServed, st.Group, groups, forming, nil
			if forming != nil {
				v.View = ViewForming
			}
			v.TailnetUp, v.MajorityReachable = st.TailnetUp, st.MajorityReachable
			v.RosterSize, v.Online = len(st.Roster), 0
			for _, r := range st.Roster {
				if r.Reachable {
					v.Online++
				}
			}
			if connect {
				v.Connecting = true
			}
			if v.RosterSize > 0 && v.Online == v.RosterSize {
				v.Connecting = false // every device is in
			}
		})
		if connect {
			s.mu.Lock()
			s.connects++
			n := s.connects
			s.mu.Unlock()
			time.AfterFunc(s.d.ConnectWindow, func() { s.connected(n) })
		}
		if forming == nil {
			s.talk.serve(s.node(), s.State().Node.ID, st.Group, st)
		} else {
			s.talk.stop()
		}
		return
	}
	if v := s.State().View; v != ViewServed && v != ViewForming {
		return // never served, or Unlock is under way
	}
	// Serving stopped, on its own when st.Err is set.
	s.forget()
	s.route(problemOrNil(st.Err))
}

// connected ends the Connecting of the n-th Group served, if still on.
func (s *Shell) connected(n int) {
	s.mu.Lock()
	current := s.connects == n && s.st.Connecting
	s.mu.Unlock()
	if current {
		s.set(func(v *State) {
			if s.connects == n { // set holds s.mu
				v.Connecting = false
			}
		})
	}
}

// ContinueNow shows the served Group without waiting longer for its
// devices.
func (s *Shell) ContinueNow() State {
	s.set(func(v *State) { v.Connecting = false })
	return s.State()
}

// Unlock serves group with passphrase.
func (s *Shell) Unlock(group, passphrase string) State {
	s.op.Lock()
	defer s.op.Unlock()
	return s.unlock(group, passphrase)
}

// unlock is Unlock with s.op held.
func (s *Shell) unlock(group, passphrase string) State {
	n := s.node()
	if n == nil {
		return s.State()
	}
	s.mu.Lock()
	s.back = ""
	s.mu.Unlock()
	s.set(func(st *State) { st.View, st.Selected, st.Problem = ViewUnlocking, group, nil })
	if err := n.Unlock(passphrase, group); err != nil {
		if errors.Is(err, node.ErrServedElsewhere) {
			groups, _ := s.groups(n)
			s.servedElsewhere(groups, group)
			return s.State()
		}
		s.route(problemOf(err))
		s.set(func(st *State) { st.Selected = group })
		return s.State()
	}
	s.mu.Lock()
	s.pass = passphrase
	s.p.LastGroup = group
	p, id := s.p, s.st.Node.ID
	s.mu.Unlock()
	s.persist()
	if p.Keychain {
		// Keep the keychain's copy current, as after a passphrase change.
		if err := s.d.Keychain.Set(id, passphrase); err != nil {
			s.fail(appProblem("keychain", err))
		}
	}
	s.apply(n.Status())
	return s.State()
}

// Lock stops serving and forgets the passphrase.
func (s *Shell) Lock() State {
	s.op.Lock()
	defer s.op.Unlock()
	n := s.node()
	if n == nil {
		return s.State()
	}
	err := n.Lock()
	s.forget()
	s.route(problemOrNil(err))
	return s.State()
}

// Switch serves group in place of the one served, with no passphrase.
func (s *Shell) Switch(group string) State {
	s.op.Lock()
	defer s.op.Unlock()
	n := s.node()
	from := s.State().Served
	if n == nil || from == group {
		return s.State()
	}
	s.mu.Lock()
	s.back = from
	s.mu.Unlock()
	s.set(func(st *State) { st.View, st.Problem = ViewUnlocking, nil })
	if err := n.Switch(group); err != nil {
		// A failed Switch leaves the Node locked.
		s.forget()
		s.route(problemOf(err))
		return s.State()
	}
	s.apply(n.Status())
	return s.State()
}

// SetEditGroup chooses the Group the section pages edit.
func (s *Shell) SetEditGroup(group string) State {
	s.mu.Lock()
	s.edit = group
	s.mu.Unlock()
	s.set(func(*State) {})
	return s.State()
}

// DismissProblem drops the Problem shown, as the user leaves the screen
// or form it was about, without changing the View.
func (s *Shell) DismissProblem() State {
	s.set(func(st *State) { st.Problem = nil })
	return s.State()
}

// SetStartAtLogin registers or unregisters the app to start at login.
func (s *Shell) SetStartAtLogin(on bool) State {
	s.op.Lock()
	defer s.op.Unlock()
	var err error
	if on {
		err = s.d.Autostart.Enable()
	} else {
		err = s.d.Autostart.Disable()
	}
	if err != nil {
		s.fail(appProblem("autostart", err))
		return s.State()
	}
	s.savePrefs(func(p *prefs.Prefs) { p.StartAtLogin = on })
	return s.State()
}

// SetKeychain keeps the passphrase in the OS keychain, or deletes it from
// there. Turning it on needs a Group served, whose passphrase it stores.
func (s *Shell) SetKeychain(on bool) State {
	s.op.Lock()
	defer s.op.Unlock()
	s.mu.Lock()
	pass, id := s.pass, s.st.Node.ID
	s.mu.Unlock()
	if on {
		if pass == "" {
			s.fail(problemOf(node.ErrLocked))
			return s.State()
		}
		if err := s.d.Keychain.Set(id, pass); err != nil {
			s.fail(appProblem("keychain", err))
			return s.State()
		}
	} else if err := s.d.Keychain.Delete(id); err != nil && !errors.Is(err, ErrNoSecret) {
		s.fail(appProblem("keychain", err))
		return s.State()
	}
	s.savePrefs(func(p *prefs.Prefs) { p.Keychain = on })
	return s.State()
}

// SetNotifications turns the app's notifications on or off.
func (s *Shell) SetNotifications(on bool) State {
	s.op.Lock()
	defer s.op.Unlock()
	s.savePrefs(func(p *prefs.Prefs) { p.MuteNotifications = !on })
	return s.State()
}

// SetMessageNotifications turns notifications of each new message on or
// off; they are off by default.
func (s *Shell) SetMessageNotifications(on bool) State {
	s.op.Lock()
	defer s.op.Unlock()
	s.savePrefs(func(p *prefs.Prefs) { p.MessageNotifications = on })
	return s.State()
}

// SetTheme picks the app's colours: "system" follows the OS, "light" or
// "dark" overrides it.
func (s *Shell) SetTheme(name string) State {
	s.op.Lock()
	defer s.op.Unlock()
	t, ok := themes[name]
	if !ok {
		s.set(func(st *State) { st.Problem = &Problem{"bad-theme", "Choose System, Light or Dark."} })
		return s.State()
	}
	s.savePrefs(func(p *prefs.Prefs) { p.Theme = t })
	return s.State()
}

var themes = map[string]prefs.Theme{"system": prefs.ThemeSystem, "light": prefs.ThemeLight, "dark": prefs.ThemeDark}

func themeName(t prefs.Theme) string {
	if t == prefs.ThemeSystem {
		return "system"
	}
	return string(t)
}

// SetWindowSize keeps the window's size for the next start.
func (s *Shell) SetWindowSize(width, height int) {
	s.mu.Lock()
	s.p.Width, s.p.Height = width, height
	s.mu.Unlock()
	s.persist()
}

// Prefs are the preferences as last loaded or set.
func (s *Shell) Prefs() prefs.Prefs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.p
}

func (s *Shell) savePrefs(change func(*prefs.Prefs)) {
	s.mu.Lock()
	change(&s.p)
	s.mu.Unlock()
	if err := s.persist(); err != nil {
		s.fail(appProblem("prefs", err))
		return
	}
	s.set(func(st *State) { st.Problem = nil })
}

// persist writes the preferences as they are now.
func (s *Shell) persist() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	return s.d.Prefs.Save(s.Prefs())
}

// forget drops the passphrase the Shell holds, as serving stops.
func (s *Shell) forget() {
	s.mu.Lock()
	s.pass = ""
	s.mu.Unlock()
	s.talk.stop()
}

// Close locks the Node and stops the Shell, as the app quits.
func (s *Shell) Close() {
	s.op.Lock()
	defer s.op.Unlock()
	// Stop following first, so the Lock below routes nothing.
	s.cancel()
	if n := s.node(); n != nil {
		if err := n.Lock(); err != nil {
			s.fail(problemOf(err))
		}
	}
	s.forget()
}

// State is the Shell's state now.
func (s *Shell) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

// fail shows p without leaving the screen.
func (s *Shell) fail(p *Problem) { s.set(func(st *State) { st.Problem = p }) }

// set changes the State and reports it.
func (s *Shell) set(change func(*State)) {
	s.mu.Lock()
	change(&s.st)
	st := &s.st
	// The edit target is the Group chosen, while the Node has it, else
	// the one served.
	st.EditGroup = st.Served
	if s.edit != "" && hasGroup(st.Groups, s.edit) {
		st.EditGroup = s.edit
	}
	st.EditGroupServed = st.EditGroup != "" && st.EditGroup == st.Served
	st.PassphraseHeld = s.pass != ""
	if st.View != ViewForming {
		st.Forming = nil
	}
	if st.View != ViewServed {
		st.Connecting = false
	}
	st.BackupDue = s.p.BackupDue
	st.Prefs = Prefs{StartAtLogin: s.p.StartAtLogin, Keychain: s.p.Keychain, Notifications: s.p.Notifications(), MessageNotifications: s.p.MessageNotifications, Theme: themeName(s.p.Theme)}
	st.Seq++
	out := *st
	s.mu.Unlock()
	s.d.OnChange(out)
}

func hasGroup(groups []Group, alias string) bool {
	for _, g := range groups {
		if g.Alias == alias {
			return true
		}
	}
	return false
}
