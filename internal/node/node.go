// Package node is the application layer front ends host a Node through
// (ADR 0034): a typed API over a Node directory, and one subscription to
// what happens on it. It holds hosting (Unlock serves a Group in-process,
// as `defios serve` does, and Lock stops it); onboarding: Create or
// Restore a Node, scaffold Groups, and form them with other Nodes; and
// talk and Keys: Conversations and their messages, chains of agents,
// the Outbox, keygen Sessions and minted Keys; Sign Requests, with the
// Hooks of this Node's that fired on them; and agents: the Node's Agent
// Profiles, where each is included, and each Group's Providers; and the
// Vault: each Group's Secrets, Backups and the passphrase change; and
// Trigger config: each Group's Jobs, Hooks and Schedules; and the Node's
// Plugins and each Group's Networks; and each Group's settings, and the
// Node's disk and process usage.
package node

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"defios.com/internal/backup"
	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/engine/tssl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/nodedir"
	"defios.com/internal/secret"
	"defios.com/internal/serve"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
	"defios.com/internal/talk"
	"defios.com/internal/trigger"
	"defios.com/internal/vault"
)

// Node is a Node as a front end drives it. Nothing in it assumes a unix
// socket, a terminal or a desktop OS, so a later client of a Node hosted
// elsewhere can implement it over a connection. Host is the in-process
// implementation.
type Node interface {
	// NodeID is this Node's ID, hex.
	NodeID() string
	// DisplayName is the Node's default display name, the one `init
	// --name` set; "" when it has none. It needs no Unlock.
	DisplayName() (string, error)
	// Check reports what would stop Unlock(_, group) that needs no
	// passphrase to find, so a front end can refuse before asking for one.
	Check(group string) error
	// Unlock opens the Vault with passphrase and serves group. The Node
	// keeps the passphrase while it serves, to read Secrets on each use
	// and to Switch.
	Unlock(passphrase, group string) error
	// Lock stops serving and forgets the passphrase. It returns why
	// serving failed, if it did while stopping; locking a locked Node is
	// a no-op.
	Lock() error
	// Switch serves group in place of the Group served now, with the
	// passphrase Unlock was given. A Switch that fails leaves the Node
	// locked.
	Switch(group string) error
	// Status is the Node's status now.
	Status() Status
	// Subscribe delivers events, in order, starting with the status now,
	// until ctx is done, when the channel closes.
	Subscribe(ctx context.Context) <-chan Event

	// Groups, each named by the alias it has on this Node.
	NewGroup(spec GroupSpec) (GroupConfig, error)
	Groups(ctx context.Context) ([]GroupSummary, error)
	GroupByID(groupID string) (string, error)
	InspectGroup(ctx context.Context, group string) (GroupDetail, error)

	// Forming a Group, from a served Group that has none yet.
	Discover(ctx context.Context, group string) ([]DiscoveredNode, error)
	Propose(ctx context.Context, group string, nodeIDs []string) (Formation, error)
	Accept(ctx context.Context, group, groupID string) (Formation, error)
	Decline(ctx context.Context, group, groupID, reason string) (Formation, error)

	// Talk. A Conversation is named by its hex ID, GroupTalk for the
	// Group-level talk; ResolveConversation finds a Key's from a query.
	// Reads need no serve; writes and Follow need the Group served.
	Conversations(ctx context.Context, group string) (Conversations, error)
	ResolveConversation(ctx context.Context, group, keyQuery string) (string, error)
	Messages(ctx context.Context, group, conv string) ([]Message, error)
	Lean(ctx context.Context, group, conv string) (Lean, error)
	Chat(ctx context.Context, group, conv string, limit int, transcripts bool) ([]ChatMessage, error)
	Handles(ctx context.Context, group, conv string) ([]string, error)
	Send(ctx context.Context, group, conv string, m Outgoing) (Sent, error)
	Cancel(ctx context.Context, group, conv, chain string) (Cancelled, error)
	Follow(ctx context.Context, group, conv string) (*ChainStream, error)
	Transcript(ctx context.Context, group, conv, chain string) (Transcript, error)
	Outbox(ctx context.Context, group string) ([]Queued, error)
	DropQueued(ctx context.Context, group, msgID string) (string, error)

	// Keys and the keygen Sessions that mint them.
	Keys(ctx context.Context, group string, passphrase Passphrase) ([]Key, error)
	InspectKey(ctx context.Context, group, query string, passphrase Passphrase) (Key, error)
	SetKeyAlias(ctx context.Context, group, query, alias string) (Key, error)
	StartKeygen(ctx context.Context, group string, k Keygen) (Session, error)
	AcceptSession(ctx context.Context, group, id, alias string) error
	AbortSession(ctx context.Context, group, id, reason string) error
	Sessions(ctx context.Context, group string) ([]Session, error)
	InspectSession(ctx context.Context, group, id string) (SessionDetail, error)

	// Sign Requests, named by a msg_id or a unique hex prefix of one. All
	// need the Group served.
	SignRequests(ctx context.Context, group string) ([]SignRequest, error)
	InspectSignRequest(ctx context.Context, group, msgID string) (SignRequestDetail, error)
	FillTransactions(ctx context.Context, group, keyQuery string, txs []TxSpec) (Filled, error)
	ProposeSignRequest(ctx context.Context, group, keyQuery string, spec SignSpec) (Sent, error)
	AcceptSignRequest(ctx context.Context, group, msgID string) (Answered, error)
	RefuseSignRequest(ctx context.Context, group, msgID, reason string) (Answered, error)
	DeclineSignRequest(ctx context.Context, group, msgID string) (DeclineResult, error)
	BroadcastSignRequest(ctx context.Context, group, msgID string) (Broadcast, error)

	// Agent Profiles, the whole Node's, named by Handle. None needs serve.
	AgentProfiles(ctx context.Context) ([]AgentProfile, error)
	AgentProfile(ctx context.Context, handle string) (AgentProfile, error)
	AddAgentProfile(ctx context.Context, handle string, e ProfileEdit) (AgentProfile, error)
	EditAgentProfile(ctx context.Context, handle string, e ProfileEdit) (AgentProfile, error)
	DeleteAgentProfile(ctx context.Context, handle string) error
	AgentSystem(ctx context.Context, handle string) (string, error)
	SetAgentSystem(ctx context.Context, handle, prompt string) error
	Grant(ctx context.Context, handle, item string, secrets []string) (GrantKind, AgentProfile, error)
	Revoke(ctx context.Context, handle, item string) (GrantKind, AgentProfile, error)

	// Which agents answer where: a Group's Group-level talk (GroupTalk) or
	// one of its Conversations. The Group need not be served.
	IncludedAgents(ctx context.Context, group, conv string) ([]string, error)
	IncludeAgent(ctx context.Context, group, conv, handle string) (Inclusion, error)
	ExcludeAgent(ctx context.Context, group, conv, handle string) (Inclusion, error)

	// Providers, a Group's, which a Conversation overrides key by key. The
	// Group need not be served.
	Providers(ctx context.Context, group, conv string) ([]Provider, error)
	Provider(ctx context.Context, group, conv, alias string) (Provider, error)
	AddProvider(ctx context.Context, group, conv, alias string, e ProviderEdit) (Provider, error)
	EditProvider(ctx context.Context, group, conv, alias string, e ProviderEdit) (Provider, error)
	RemoveProvider(ctx context.Context, group, conv, alias string) error

	// A Group's Secrets, by name. The Vault opens with the passphrase this
	// host holds while it is unlocked, else with the one passphrase gives;
	// RevealSecret alone reads a value back, and only with the passphrase
	// entered. None needs serve.
	Secrets(ctx context.Context, group string, passphrase Passphrase) ([]Secret, error)
	Secret(ctx context.Context, group, name string, passphrase Passphrase) (Secret, error)
	RevealSecret(ctx context.Context, group, name string, passphrase Passphrase) (value, source string, err error)
	SetSecret(ctx context.Context, group, name, value string, passphrase Passphrase) error
	RemoveSecret(ctx context.Context, group, name string, passphrase Passphrase) error

	// The whole Node's Vault: a Backup, and a passphrase change, which is
	// refused while the Node is hosted.
	Backup(ctx context.Context, dst string, passphrase Passphrase) (backup.Manifest, error)
	CheckPassphrase(passphrase string) error
	CheckPassphraseChange() error
	ChangePassphrase(ctx context.Context, old, next string) (vault.Change, error)

	// Trigger config: a Group's Jobs, Hooks and Schedules (ADR 0032), by
	// name, in any Group, served or not. Writes are validated as the config
	// loader does and change only that table of config.toml, comments
	// elsewhere included; config.toml is also read and written whole as
	// text. RunJob runs a Job of the served Group now.
	Jobs(ctx context.Context, group string) ([]Job, error)
	AddJob(ctx context.Context, group, name string, j config.Job) error
	EditJob(ctx context.Context, group, name string, j config.Job) error
	DeleteJob(ctx context.Context, group, name string) error
	Hooks(ctx context.Context, group string) ([]Hook, error)
	AddHook(ctx context.Context, group, name string, hk config.Hook) error
	EditHook(ctx context.Context, group, name string, hk config.Hook) error
	DeleteHook(ctx context.Context, group, name string) error
	Schedules(ctx context.Context, group string) ([]Schedule, error)
	AddSchedule(ctx context.Context, group, name string, s config.Schedule) error
	EditSchedule(ctx context.Context, group, name string, s config.Schedule) error
	DeleteSchedule(ctx context.Context, group, name string) error
	GroupConfigTOML(ctx context.Context, group string) (string, error)
	SetGroupConfigTOML(ctx context.Context, group, text string) error
	RunJob(ctx context.Context, group, name string, ev *trigger.Event) (JobResult, error)

	// Plugins, the whole Node's. An install or update is staged first, so
	// a front end can show what it brings before committing it. None
	// needs serve.
	Plugins(ctx context.Context) ([]Plugin, error)
	InstalledPlugins(ctx context.Context) ([]string, error)
	StagePluginInstall(ctx context.Context, source string) (*PluginStage, error)
	StagePluginUpdate(ctx context.Context, name string) (*PluginStage, error)
	RemovePlugin(ctx context.Context, name string) error

	// Networks, a Group's, whose tables a Conversation overrides whole.
	// The Group need not be served.
	Networks(ctx context.Context, group, conv string) ([]Network, error)
	Network(ctx context.Context, group, conv, alias string) (Network, error)
	AddNetwork(ctx context.Context, group, conv, alias string, e NetworkEdit) (Network, error)
	EditNetwork(ctx context.Context, group, conv, alias string, e NetworkEdit) (Network, error)
	RemoveNetwork(ctx context.Context, group, conv, alias string) error

	// A Group's settings: this Node's presence in it and its prune
	// policy. The Group need not be served; a served one takes a change
	// the next time it is served.
	GroupSettings(ctx context.Context, group string) (GroupSettings, error)
	EditGroupSettings(ctx context.Context, group string, e SettingsEdit) (GroupSettings, error)

	// Usage is the Node's disk, every Group's or one's, and the serving
	// process's own use. It needs no Group served.
	Usage(ctx context.Context, group string) (Usage, error)

	// TokenUsage is a Group's Token Usage: per Handle, the input and
	// output tokens of the Chains this Node ran. It is apart from Usage,
	// which is Node Usage only, and needs no Group served.
	TokenUsage(ctx context.Context, group string) (TokenUsage, error)
}

// Errors a front end tells apart. Vault failures come through as the
// vault package's own: vault.ErrBadPassphrase, vault.ErrBusy (a passphrase
// change is under way) and *vault.LeftoverError.
var (
	// ErrNoNode: the directory holds no Node.
	ErrNoNode = errors.New("node: no Node in this directory")
	// ErrUnknownGroup: the Node has no Group by that alias.
	ErrUnknownGroup = errors.New("node: no such Group on this Node")
	// ErrServedElsewhere: another process is serving this Node, so this
	// one does not serve it too.
	ErrServedElsewhere = errors.New("node: another process is serving this Node")
	// ErrUnlocked: this host already serves a Group; Lock or Switch.
	ErrUnlocked = errors.New("node: already unlocked")
	// ErrLocked: the operation needs the Node unlocked.
	ErrLocked = errors.New("node: locked")
)

// NoHeadscaleURLError: the Group's config names no control server, so the
// Node cannot join its tailnet.
type NoHeadscaleURLError struct{ ConfigFile string }

func (e *NoHeadscaleURLError) Error() string {
	return fmt.Sprintf("headscale_url is not set in %s", e.ConfigFile)
}

// IdentityError: the Vault opened but its keys do not match this Node's
// identity file.
type IdentityError struct{ Err error }

func (e *IdentityError) Error() string { return e.Err.Error() }
func (e *IdentityError) Unwrap() error { return e.Err }

// Status is the Node status a front end shows.
type Status struct {
	Group     string // the Group served; "" when locked
	Serving   bool
	TailnetUp bool
	Leader    string // Node ID of the Raft leader; "" when none is known
	// Roster is the served Group's Roster with each Node's reachability
	// from here; empty before the Group has one.
	Roster []RosterNode
	// MajorityReachable: more than half the Roster is reachable, so
	// writes can commit.
	MajorityReachable bool
	// Keys are the served Group's minted Keys, each with whether t of its
	// Nodes are reachable, so it can sign.
	Keys []KeyReach
	// Err is why serving stopped on its own; nil after Lock.
	Err error
}

// RosterNode is one Roster Node as Status reports it.
type RosterNode struct {
	NodeID      string
	DisplayName string
	Reachable   bool
}

// KeyReach is one Key as Status reports it.
type KeyReach struct {
	Key       string // curve:hex
	T         int
	Reachable int  // how many of its Nodes are reachable
	CanSign   bool // at least T of them are
}

// Event is one thing that happened on the Node. Exactly one field is set.
type Event struct {
	Status   *Status   // the Node's status changed to this
	Proposal *Proposal // another Node proposed a Group; Accept or Decline it
	Accepted *Accepted // a Node accepted the served Group's proposal
	// Message is a message the served Group's replica committed, any
	// Node's, this one's queued ones once they commit included.
	Message *MessageEvent
	// Outbox is this Node's Outbox after what it holds queued changed: a
	// message was queued, committed or dropped.
	Outbox *OutboxEvent
	// Chain is an event of agents answering in a Conversation on this
	// Node: a chain's start and end, each agent's output as it generates
	// it (thinking, text, tool calls), and what it said. A Trigger of this
	// Node's firing (trigger.fire) and how each of its Jobs ended
	// (job.end) come this way too, in the Conversation of what it fired on.
	Chain *ChainEventIn
	// Session is a Session whose status moved: started, completed or
	// aborted.
	Session *SessionEvent
	// SignRequest is a Sign Request whose status, stances, Session or
	// Broadcast Receipts moved, one proposed included, or whose deadline
	// passed: expired, which no message marks.
	SignRequest *SignRequestEvent
	// HookRun is a Hook of this Node's firing on a Sign Request, and each
	// of its Jobs starting and ending.
	HookRun *HookRunEvent
}

// MessageEvent is a committed message and where.
type MessageEvent struct {
	Group        string
	Conversation string // hex Conversation ID; GroupTalk for Group-level
	Message      Message
}

// OutboxEvent is what a Group's Outbox holds queued now, oldest first.
type OutboxEvent struct {
	Group  string
	Queued []Queued
}

// ChainEventIn is a chain event and where.
type ChainEventIn struct {
	Group        string
	Conversation string // hex Conversation ID; GroupTalk for Group-level
	Event        ChainEvent
}

// SessionEvent is a Session's new status, in Session.Status.
type SessionEvent struct {
	Group   string
	Session Session
}

// Proposal is a Group proposal this Node holds pending.
type Proposal struct {
	Group     string // the alias of the served Group that holds it
	GroupID   string
	Initiator string // Node ID of the proposing Node
	Roster    []ProposedNode
}

// ProposedNode is one Node a proposal's Roster names.
type ProposedNode struct {
	NodeID      string
	DisplayName string
	Hostname    string
}

// Accepted is one Node's acceptance, this Node's own included, as this
// Node holds it.
type Accepted struct {
	Group   string // the alias of the served Group
	GroupID string
	NodeID  string // who accepted
	Status  string // the Group's status after it: pending | valid
}

// Options are what a host chooses. The zero value is production.
type Options struct {
	// Getenv resolves config "$NAME" references; nil is os.Getenv.
	Getenv func(string) string
	// Logf receives the serving Node's log lines; nil discards them.
	Logf func(format string, args ...any)
	// NewTailnet joins the tailnet; nil is serve.NewTsnet.
	NewTailnet func(serve.TsnetOptions) serve.Tailnet
	// Sessions sets how this Node runs Sessions, once the Vault is open;
	// nil is tss-lib's engines and the default wait.
	Sessions func(self identity.ID) (Sessions, error)
	// StatusInterval is how often a serving Node's status is refreshed;
	// zero is 5 seconds.
	StatusInterval time.Duration
}

// Sessions is how a Node runs Sessions.
type Sessions struct {
	Keygen func() session.Engine // nil: the Node runs no Sessions
	Sign   func() session.Engine // nil: the Node does not sign
	// WaitingFor overrides a Session's wait when non-zero.
	WaitingFor time.Duration
}

// Host hosts a Node in this process. Its methods are safe to call from
// any goroutine.
type Host struct {
	p   nodedir.Paths
	o   Options
	pub identity.Public

	// op serializes Unlock, Lock and Switch, so one never sees another
	// half done; mu guards the fields below and is never held while the
	// Vault opens.
	op         sync.Mutex
	mu         sync.Mutex
	cur        *serving // nil when locked
	passphrase string   // held while cur is set
	status     Status
	subs       map[*subscriber]struct{}
}

var _ Node = (*Host)(nil)

// serving is one Unlock's serve and what it holds.
type serving struct {
	group  string
	run    *serve.Running
	cancel context.CancelFunc
	done   chan struct{} // closed once the serve stopped and released all
}

// Open reads the Node in p. It serves nothing until Unlock.
func Open(p nodedir.Paths, o Options) (*Host, error) {
	pub, err := identity.ReadFile(p.IdentityFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoNode
	} else if err != nil {
		return nil, err
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.NewTailnet == nil {
		o.NewTailnet = serve.NewTsnet
	}
	if o.Sessions == nil {
		o.Sessions = tssSessions
	}
	if o.StatusInterval <= 0 {
		o.StatusInterval = 5 * time.Second
	}
	return &Host{p: p, o: o, pub: pub, subs: map[*subscriber]struct{}{}}, nil
}

func tssSessions(self identity.ID) (Sessions, error) {
	return Sessions{
		Keygen: func() session.Engine { return tssl.New([32]byte(self)) },
		Sign:   func() session.Engine { return tssl.NewSigner([32]byte(self)) },
	}, nil
}

// NodeID implements Node.
func (h *Host) NodeID() string { return h.pub.NodeID.String() }

// DisplayName implements Node.
func (h *Host) DisplayName() (string, error) {
	raw, err := os.ReadFile(h.p.DisplayNameFile())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(raw)), err
}

// Check implements Node.
func (h *Host) Check(group string) error {
	_, err := h.check(group)
	return err
}

// check loads group's config and refuses what Unlock would, short of
// opening the Vault.
func (h *Host) check(group string) (config.Config, error) {
	cfg, err := h.checkConfig(group)
	if err != nil {
		return config.Config{}, err
	}
	if serve.IsUp(h.p.Socket) {
		return config.Config{}, ErrServedElsewhere
	}
	unlockVault, err := useVault(h.p)
	if err != nil {
		return config.Config{}, err
	}
	unlockVault()
	return cfg, nil
}

// checkConfig loads group's config and refuses a Group that config alone
// shows cannot be served.
func (h *Host) checkConfig(group string) (config.Config, error) {
	if group == "" {
		return config.Config{}, ErrUnknownGroup
	}
	if _, err := os.Stat(h.p.GroupConfigFile(group)); errors.Is(err, os.ErrNotExist) {
		return config.Config{}, fmt.Errorf("%w: %q", ErrUnknownGroup, group)
	} else if err != nil {
		return config.Config{}, err
	}
	e, err := config.Load(h.p, group, nil, nil)
	if err != nil {
		return config.Config{}, err
	}
	if e.Group.HeadscaleURL == "" {
		return config.Config{}, &NoHeadscaleURLError{ConfigFile: h.p.GroupConfigFile(group)}
	}
	return e.Group, nil
}

// useVault takes the Vault's lock shared, so no passphrase change runs
// while it is held, and refuses a Vault an interrupted change left.
func useVault(p nodedir.Paths) (unlock func(), err error) {
	unlock, err = vault.Lock(p.VaultDir(), false)
	if err != nil {
		return nil, err
	}
	if err := vault.CheckLeftover(p.VaultDir()); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// Unlock implements Node.
func (h *Host) Unlock(passphrase, group string) error {
	h.op.Lock()
	defer h.op.Unlock()
	return h.unlock(passphrase, group)
}

// unlock is Unlock with h.op held.
func (h *Host) unlock(passphrase, group string) error {
	h.mu.Lock()
	unlocked := h.cur != nil
	h.mu.Unlock()
	if unlocked {
		return ErrUnlocked
	}
	sv, err := h.start(passphrase, group)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur, h.passphrase = sv, passphrase
	h.setStatus(Status{Group: group, Serving: true})
	return nil
}

// start opens the Vault and serves group, as `defios serve` always has.
func (h *Host) start(passphrase, alias string) (*serving, error) {
	cfg, err := h.check(alias)
	if err != nil {
		return nil, err
	}
	// Held for as long as the Node serves: it reseals a Group's shares on
	// keygen, so no passphrase change may run under it.
	unlockVault, err := useVault(h.p)
	if err != nil {
		return nil, err
	}
	release := unlockVault
	defer func() {
		if release != nil {
			release()
		}
	}()
	// Unlock the Node's shared secrets plus only this Group's shares;
	// another Group's shares file is never opened (ADR 0007).
	shared, err := vault.Open(h.p.VaultSharedFile(), passphrase)
	if err != nil {
		return nil, err
	}
	shares, err := vault.OpenShares(h.p.VaultSharesFile(alias), passphrase)
	if errors.Is(err, os.ErrNotExist) {
		shares = nil // no Key minted for this Group yet
	} else if err != nil {
		return nil, err
	}
	self, err := identity.Unlock(h.pub, shared.AuthSeed, shared.EncryptionPrivateKey)
	if err != nil {
		return nil, &IdentityError{Err: err}
	}
	sessions, err := h.o.Sessions(self.NodeID)
	if err != nil {
		return nil, err
	}
	logf := h.o.Logf
	// The Group's Secrets are read at each use, so a `secret set` while
	// the Node serves takes effect without a restart.
	secrets := func() (map[string]string, error) {
		s, err := vault.OpenSecrets(h.p.VaultSecretsFile(alias), passphrase)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // no Secret set for this Group yet
		}
		return s, err
	}
	// The Vault is open before the tailnet join, so a Vault-held preauth
	// key works on first boot. A Node already joined needs none, so a
	// reference that resolves nowhere is reported and the join goes ahead
	// on the saved tailnet state.
	var authKey string
	if cfg.TailnetAuthKey != "" {
		r := &secret.Resolver{Group: alias, Getenv: h.o.Getenv, Vault: secrets}
		authKey, err = r.Resolve(cfg.TailnetAuthKey)
		if errors.Is(err, secret.ErrUnresolved) {
			logf("serve: tailnet_auth_key: %v; joining on this Node's saved tailnet state", err)
		} else if err != nil {
			return nil, err
		}
	}
	tailnet := h.o.NewTailnet(serve.TsnetOptions{
		Dir:        h.p.TsnetDir(alias),
		Hostname:   cfg.Hostname,
		ControlURL: cfg.HeadscaleURL,
		AuthKey:    authKey,
		Logf:       logf,
	})
	ctx, cancel := context.WithCancel(context.Background())
	run, err := serve.Start(ctx, serve.Options{
		Paths:             h.p,
		Alias:             alias,
		Node:              self,
		Config:            cfg,
		Tailnet:           tailnet,
		Logf:              logf,
		Preparams:         shared.EcdsaPreparams,
		Vault:             vault.NewStore(h.p.VaultSharesFile(alias), passphrase, shares),
		SessionWaitingFor: sessions.WaitingFor,
		NewEngine:         sessions.Keygen,
		NewSigner:         sessions.Sign,
		Getenv:            h.o.Getenv,
		Secrets:           secrets,
		OnProposal: func(p group.Proposal) {
			ev := &Proposal{Group: alias, GroupID: p.GroupID.String(), Initiator: p.Initiator.String()}
			for _, n := range p.Roster.Nodes() {
				ev.Roster = append(ev.Roster, ProposedNode{NodeID: n.NodeID.String(), DisplayName: n.DisplayName, Hostname: n.TsnetHostname})
			}
			h.publish(Event{Proposal: ev})
		},
		OnAccept: func(gid group.ID, who identity.ID, st group.Status) {
			h.publish(Event{Accepted: &Accepted{Group: alias, GroupID: gid.String(), NodeID: who.String(), Status: st.String()}})
		},
		OnChain: func(conv []byte, ev serve.ChainEvent) {
			h.publish(Event{Chain: &ChainEventIn{Group: alias, Conversation: hex.EncodeToString(conv), Event: ev}})
		},
		OnTalk: func(conv []byte, m talk.Message) {
			h.publish(Event{Message: &MessageEvent{Group: alias, Conversation: hex.EncodeToString(conv), Message: h.message(m)}})
		},
		OnSession: func(sum session.Summary) {
			h.publish(Event{Session: &SessionEvent{Group: alias, Session: sum}})
		},
		OnSignRequest: func(sum signreq.Summary) {
			h.publish(Event{SignRequest: &SignRequestEvent{Group: alias, Request: sum}})
		},
		OnHookRun: func(run serve.HookRun) {
			h.publish(Event{HookRun: &HookRunEvent{Group: alias, Run: run}})
		},
		OnOutbox: func(queued []cosl.Signed) {
			q, err := queuedOf(queued)
			if err != nil {
				logf("node: Outbox: %v", err)
				return
			}
			h.publish(Event{Outbox: &OutboxEvent{Group: alias, Queued: q}})
		},
	})
	if errors.Is(err, serve.ErrAlreadyRunning) {
		cancel()
		return nil, ErrServedElsewhere
	} else if err != nil {
		cancel()
		return nil, err
	}
	sv := &serving{group: alias, run: run, cancel: cancel, done: make(chan struct{})}
	release = nil // the serving goroutine releases the Vault now
	go h.watch(sv, ctx, unlockVault)
	return sv, nil
}

// watch refreshes sv's status until its serve stops, then releases the
// Vault and, when the serve stopped on its own, locks the Node.
func (h *Host) watch(sv *serving, ctx context.Context, unlockVault func()) {
	defer close(sv.done)
	tick := time.NewTicker(h.o.StatusInterval)
	defer tick.Stop()
	for stopped := false; !stopped; {
		select {
		case <-sv.run.Done():
			stopped = true
			continue
		default:
		}
		h.refresh(sv, ctx)
		select {
		case <-tick.C:
		case <-sv.run.Done():
			stopped = true
		}
	}
	sv.cancel()
	unlockVault()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur == sv { // not Lock's doing
		h.cur, h.passphrase = nil, ""
		h.setStatus(Status{Err: sv.run.Wait()})
	}
}

// refresh publishes sv's status, if sv is still what this host serves.
func (h *Host) refresh(sv *serving, ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, h.o.StatusInterval+10*time.Second)
	defer cancel()
	ns := sv.run.NodeStatus(ctx)
	st := Status{Group: sv.group, Serving: true, TailnetUp: ns.TailnetUp}
	if ns.HasLeader {
		st.Leader = ns.Leader.String()
	}
	reachable, reach := 0, map[identity.ID]bool{}
	for _, r := range ns.Roster {
		st.Roster = append(st.Roster, RosterNode{NodeID: r.NodeID.String(), DisplayName: r.DisplayName, Reachable: r.Reachable})
		if r.Reachable {
			reachable++
			reach[r.NodeID] = true
		}
	}
	st.MajorityReachable = len(ns.Roster) > 0 && reachable > len(ns.Roster)/2
	for _, k := range ns.Keys {
		kr := KeyReach{Key: k.Key, T: k.T}
		for _, n := range k.Nodes {
			if reach[n] {
				kr.Reachable++
			}
		}
		kr.CanSign = kr.Reachable >= k.T
		st.Keys = append(st.Keys, kr)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur == sv {
		h.setStatus(st)
	}
}

// Lock implements Node.
func (h *Host) Lock() error {
	h.op.Lock()
	defer h.op.Unlock()
	return h.lock()
}

// lock is Lock with h.op held.
func (h *Host) lock() error {
	h.mu.Lock()
	sv := h.cur
	h.cur, h.passphrase = nil, ""
	h.mu.Unlock()
	if sv == nil {
		return nil
	}
	sv.cancel()
	<-sv.done
	h.mu.Lock()
	if h.cur == nil {
		h.setStatus(Status{})
	}
	h.mu.Unlock()
	return sv.run.Wait()
}

// Switch implements Node. A group that cannot be served, by its config
// alone, is refused before the Group served now stops.
func (h *Host) Switch(group string) error {
	h.op.Lock()
	defer h.op.Unlock()
	h.mu.Lock()
	if h.cur == nil {
		h.mu.Unlock()
		return ErrLocked
	}
	passphrase, from := h.passphrase, h.cur.group
	h.mu.Unlock()
	if _, err := h.checkConfig(group); err != nil {
		return err
	}
	if err := h.lock(); err != nil {
		h.o.Logf("node: stopping Group %s: %v", from, err)
	}
	return h.unlock(passphrase, group)
}

// Status implements Node.
func (h *Host) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status.clone()
}

// publish tells every subscriber ev.
func (h *Host) publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		s.push(ev)
	}
}

// setStatus records st and tells subscribers when it changed. h.mu is held.
func (h *Host) setStatus(st Status) {
	if st.equal(h.status) {
		return
	}
	h.status = st
	for s := range h.subs {
		c := st.clone()
		s.push(Event{Status: &c})
	}
}

func (s Status) clone() Status {
	s.Roster = append([]RosterNode(nil), s.Roster...)
	s.Keys = append([]KeyReach(nil), s.Keys...)
	return s
}

func (s Status) equal(o Status) bool {
	if s.Group != o.Group || s.Serving != o.Serving || s.TailnetUp != o.TailnetUp || s.Leader != o.Leader ||
		s.MajorityReachable != o.MajorityReachable || len(s.Roster) != len(o.Roster) || (s.Err == nil) != (o.Err == nil) ||
		!slices.Equal(s.Keys, o.Keys) {
		return false
	}
	if s.Err != nil && s.Err.Error() != o.Err.Error() {
		return false
	}
	for i := range s.Roster {
		if s.Roster[i] != o.Roster[i] {
			return false
		}
	}
	return true
}
