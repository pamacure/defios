package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/outbox"
	"defios.com/internal/raftx"
	coslv1 "defios.com/proto/cosl/v1"
)

// raft is a fake replica: with no leader every append fails as Raft's does,
// otherwise it commits in call order, or rejects what reject names.
type raft struct {
	mu        sync.Mutex
	leader    bool
	committed [][32]byte
	reject    map[[32]byte]bool
}

func (r *raft) append(_ context.Context, s cosl.Signed) (raftx.Position, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.leader {
		return raftx.Position{}, raftx.ErrNoLeader
	}
	if r.reject[s.MsgID] {
		return raftx.Position{}, raftx.ErrNotInserted
	}
	r.committed = append(r.committed, s.MsgID)
	return raftx.Position{Index: uint64(len(r.committed)), Term: 1}, nil
}

func (r *raft) setLeader(up bool) {
	r.mu.Lock()
	r.leader = up
	r.mu.Unlock()
}

func (r *raft) log() [][32]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][32]byte(nil), r.committed...)
}

type fixture struct {
	t      *testing.T
	raft   *raft
	path   string
	author cosl.Author
	logs   []string
	now    time.Time // zero is the wall clock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	n, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	roster := group.NewRoster(group.RosterNode{Public: n.Public, DisplayName: "alice", TsnetHostname: "defios-alice"})
	return &fixture{
		t:      t,
		raft:   &raft{reject: map[[32]byte]bool{}},
		path:   filepath.Join(t.TempDir(), "outbox.json"),
		author: cosl.Author{Node: n, GroupID: roster.GroupID()},
	}
}

func (f *fixture) open() *outbox.Outbox {
	f.t.Helper()
	o, err := outbox.Open(outbox.Config{
		Path:   f.path,
		Append: f.raft.append,
		Now: func() time.Time {
			if f.now.IsZero() {
				return time.Now()
			}
			return f.now
		},
		Logf: func(format string, args ...any) { f.logs = append(f.logs, fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return o
}

// logged reports whether any log line contains word.
func (f *fixture) logged(word string) bool {
	return slices.ContainsFunc(f.logs, func(l string) bool { return strings.Contains(l, word) })
}

func (f *fixture) sign(typ uint32, payload proto.Message) cosl.Signed {
	f.t.Helper()
	s, err := f.author.Sign(typ, nil, payload)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fixture) talk(text string) cosl.Signed {
	return f.sign(cosl.TypeAppFreeform, &coslv1.AppFreeform{Text: text, ContentType: "text/plain"})
}

func (f *fixture) write(o *outbox.Outbox, s cosl.Signed) outbox.Result {
	f.t.Helper()
	res, err := o.Write(context.Background(), s)
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func ids(ss ...cosl.Signed) [][32]byte {
	out := make([][32]byte, len(ss))
	for i, s := range ss {
		out[i] = s.MsgID
	}
	return out
}

// A write that finds no leader is queued, not committed, and the drain
// commits it once a leader is known.
func TestNoLeaderQueuesAndDrainCommits(t *testing.T) {
	f := newFixture(t)
	o := f.open()
	m := f.talk("hello")
	if res := f.write(o, m); !res.Queued {
		t.Fatalf("Write = %+v, want queued", res)
	}
	if got := f.raft.log(); len(got) != 0 {
		t.Fatalf("committed %d with no leader", len(got))
	}
	f.raft.setLeader(true)
	o.Drain(context.Background())
	if got := f.raft.log(); !slices.Equal(got, ids(m)) {
		t.Fatalf("committed %x, want the queued message", got)
	}
	if n := len(o.Entries()); n != 0 {
		t.Fatalf("%d entries left after drain", n)
	}
}

// While the Outbox holds anything, a write joins its back even with a leader
// known, so this Node's messages commit in the order it wrote them.
func TestNonEmptyOutboxKeepsWriteOrder(t *testing.T) {
	f := newFixture(t)
	o := f.open()
	first := f.talk("first")
	f.write(o, first)
	f.raft.setLeader(true)
	second := f.talk("second")
	if res := f.write(o, second); !res.Queued {
		t.Fatalf("Write with a non-empty Outbox = %+v, want queued", res)
	}
	o.Drain(context.Background())
	third := f.talk("third")
	if res := f.write(o, third); res.Queued || res.Position.Index != 3 {
		t.Fatalf("Write after the drain = %+v, want committed at 3", res)
	}
	if got := f.raft.log(); !slices.Equal(got, ids(first, second, third)) {
		t.Fatalf("committed out of write order: %x", got)
	}
}

// What is queued survives the Outbox being reopened, as a serve restart
// does, and still commits in order.
func TestQueuedSurvivesReopen(t *testing.T) {
	f := newFixture(t)
	a, b := f.talk("a"), f.talk("b")
	o := f.open()
	f.write(o, a)
	f.write(o, b)

	o = f.open()
	f.raft.setLeader(true)
	o.Drain(context.Background())
	if got := f.raft.log(); !slices.Equal(got, ids(a, b)) {
		t.Fatalf("committed %x after reopen, want a then b", got)
	}
	if n := len(f.open().Entries()); n != 0 {
		t.Fatalf("%d entries on disk after the drain", n)
	}
}

// Session traffic and a Sign Request with a Solana transaction, whose
// recent_blockhash would expire in the queue, never queue: they fail with
// no-leader as before, and do not hold up what comes after them.
func TestUnqueueableFailsFast(t *testing.T) {
	f := newFixture(t)
	o := f.open()
	solana := &coslv1.SignRequest{Reason: "pay", DeadlineSeconds: 60, Transactions: []*coslv1.SignTransaction{
		{Intent: &coslv1.SignTransaction_Ethereum{Ethereum: &coslv1.EthereumIntent{}}},
		{Intent: &coslv1.SignTransaction_Solana{Solana: &coslv1.SolanaIntent{}}},
	}}
	for name, s := range map[string]cosl.Signed{
		"session.abort":       f.sign(cosl.TypeSessionAbort, &coslv1.SessionAbort{Reason: "x"}),
		"tss.session":         f.sign(cosl.TypeTssSession, &coslv1.TssSession{}),
		"solana sign.request": f.sign(cosl.TypeSignRequest, solana),
	} {
		if _, err := o.Write(context.Background(), s); !errors.Is(err, raftx.ErrNoLeader) {
			t.Errorf("%s: Write err = %v, want ErrNoLeader", name, err)
		}
	}
	f.write(o, f.talk("queued"))
	f.raft.setLeader(true)
	abort := f.sign(cosl.TypeSessionAbort, &coslv1.SessionAbort{Reason: "y"})
	if res, err := o.Write(context.Background(), abort); err != nil || res.Queued {
		t.Fatalf("session.abort with a non-empty Outbox = %+v, %v; want committed at once", res, err)
	}
	if n := len(o.Entries()); n != 1 {
		t.Fatalf("Outbox holds %d, want only the talk", n)
	}
}

// A queued message the FSM rejects is logged and skipped; those after it
// still commit.
func TestDrainSkipsWhatTheFSMRejects(t *testing.T) {
	f := newFixture(t)
	o := f.open()
	a, bad, c := f.talk("a"), f.talk("bad"), f.talk("c")
	for _, s := range []cosl.Signed{a, bad, c} {
		f.write(o, s)
	}
	f.raft.reject[bad.MsgID] = true
	f.raft.setLeader(true)
	o.Drain(context.Background())
	if got := f.raft.log(); !slices.Equal(got, ids(a, c)) {
		t.Fatalf("committed %x, want a and c", got)
	}
	if n := len(o.Entries()); n != 0 {
		t.Fatalf("%d entries left; the rejected one should be gone", n)
	}
	if !f.logged("rejected") {
		t.Fatalf("no log line says the message was rejected: %q", f.logs)
	}
}

// A queued sign.request whose deadline, counted from its created_at, has
// passed never commits: the drain drops it, logs it as expired unsent, and
// goes on.
func TestDrainDropsExpiredSignRequest(t *testing.T) {
	f := newFixture(t)
	f.now = time.Now()
	o := f.open()
	req := f.sign(cosl.TypeSignRequest, &coslv1.SignRequest{Reason: "pay", DeadlineSeconds: 300, Transactions: []*coslv1.SignTransaction{
		{Intent: &coslv1.SignTransaction_Ethereum{Ethereum: &coslv1.EthereumIntent{}}},
	}})
	after := f.talk("after")
	f.write(o, req)
	f.write(o, after)

	f.now = f.now.Add(301 * time.Second)
	f.raft.setLeader(true)
	o.Drain(context.Background())
	if got := f.raft.log(); !slices.Equal(got, ids(after)) {
		t.Fatalf("committed %x, want only the talk after the expired request", got)
	}
	if !f.logged("expired unsent") {
		t.Fatalf("no log line says expired unsent: %q", f.logs)
	}
}
