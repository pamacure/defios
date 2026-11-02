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
	// applied is the seq the fake replica has applied up to.
	applied uint64
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
		Path:    f.path,
		Append:  f.raft.append,
		Applied: func() uint64 { return f.applied },
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

// Session traffic never queues: it fails with no-leader as before, and does
// not hold up what comes after it.
func TestUnqueueableFailsFast(t *testing.T) {
	f := newFixture(t)
	o := f.open()
	for name, s := range map[string]cosl.Signed{
		"session.abort": f.sign(cosl.TypeSessionAbort, &coslv1.SessionAbort{Reason: "x"}),
		"tss.session":   f.sign(cosl.TypeTssSession, &coslv1.TssSession{}),
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

// A Sign Request with a Solana transaction queues like any other: if a
// majority returns before its deadline, the drain commits it.
func TestSolanaSignRequestQueues(t *testing.T) {
	f := newFixture(t)
	o := f.open()
	req := f.sign(cosl.TypeSignRequest, &coslv1.SignRequest{Reason: "pay", DeadlineSeconds: 60, Transactions: []*coslv1.SignTransaction{
		{Intent: &coslv1.SignTransaction_Solana{Solana: &coslv1.SolanaIntent{}}},
	}})
	if res := f.write(o, req); !res.Queued {
		t.Fatalf("Write = %+v, want queued", res)
	}
	f.raft.setLeader(true)
	o.Drain(context.Background())
	if got := f.raft.log(); !slices.Equal(got, ids(req)) {
		t.Fatalf("committed %x, want the queued request", got)
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

// rows are the fake replica's committed rows of ss, at seq 1 on.
func (f *fixture) rows(ss ...cosl.Signed) []cosl.Row {
	f.t.Helper()
	out := make([]cosl.Row, len(ss))
	for i, s := range ss {
		r, err := cosl.RowOf(s, uint64(i+1))
		if err != nil {
			f.t.Fatal(err)
		}
		out[i] = r
	}
	return out
}

// texts is each row's app.freeform text, "#<seq>" after it, or "#queued".
func texts(t *testing.T, rows []cosl.Row) string {
	t.Helper()
	var out []string
	for _, r := range rows {
		env, err := r.Decode()
		if err != nil {
			t.Fatal(err)
		}
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			t.Fatal(err)
		}
		at := fmt.Sprint("#", r.Seq)
		if r.Queued() {
			at = "#queued"
		}
		out = append(out, msg.(*coslv1.AppFreeform).Text+at)
	}
	return strings.Join(out, " ")
}

// This Node's view of the talk is its committed rows, then what it wrote
// that its replica has not applied yet: committed messages by seq, then the
// queued ones in queue order. A message both applied and remembered shows
// once, as committed; once applied it is no longer remembered.
func TestTheViewIsCommittedTalkThenWhatThisNodeHasNotApplied(t *testing.T) {
	f := newFixture(t)
	o := f.open()
	f.raft.setLeader(true)
	one, two := f.talk("one"), f.talk("two")
	f.write(o, one)
	f.write(o, two)
	f.raft.setLeader(false)
	three := f.talk("three")
	f.write(o, three)
	f.write(o, f.sign(cosl.TypeSignAccept, &coslv1.SignAccept{}))

	f.applied = 1
	view, err := outbox.View(f.rows(one), o.Unapplied(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(t, view); got != "one#1 two#2 three#queued" {
		t.Fatalf("view = %s", got)
	}
	f.applied = 2
	if got := len(o.Unapplied()); got != 2 {
		t.Fatalf("Unapplied holds %d once seq 2 is applied, want the 2 queued", got)
	}
}

// The view of a Conversation holds only its own talk: Group-level talk and
// another Conversation's are not in it.
func TestTheViewIsOneConversations(t *testing.T) {
	f := newFixture(t)
	o := f.open()
	conv := make([]byte, 32)
	conv[0] = 1
	here, err := f.author.Sign(cosl.TypeAppFreeform, conv, &coslv1.AppFreeform{Text: "here", ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	f.write(o, f.talk("group-level"))
	f.write(o, here)
	view, err := outbox.View(nil, o.Unapplied(), conv)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(t, view); got != "here#queued" {
		t.Fatalf("view = %s", got)
	}
}

// Read is the queued messages on disk, for a reader with no serve.
func TestReadIsWhatIsQueued(t *testing.T) {
	f := newFixture(t)
	o := f.open()
	a := f.talk("a")
	f.write(o, a)
	got, err := outbox.Read(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(got...), ids(a)) {
		t.Fatalf("Read = %x, want a", ids(got...))
	}
	if got, err := outbox.Read(filepath.Join(t.TempDir(), "none.json")); err != nil || len(got) != 0 {
		t.Fatalf("Read of no file = %v, %v; want empty", got, err)
	}
}

// Drop erases exactly the queued message a msg_id prefix names, nothing
// after it, and the drain then commits the rest in order.
func TestDropErasesOneQueuedMessage(t *testing.T) {
	f := newFixture(t)
	o := f.open()
	a, b, c := f.talk("a"), f.talk("b"), f.talk("c")
	for _, s := range []cosl.Signed{a, b, c} {
		f.write(o, s)
	}
	got, err := o.Drop(fmt.Sprintf("%X", b.MsgID[:5])[:9])
	if err != nil || got.MsgID != b.MsgID {
		t.Fatalf("Drop = %x, %v; want b", got.MsgID, err)
	}
	if on, _ := outbox.Read(f.path); !slices.Equal(ids(on...), ids(a, c)) {
		t.Fatalf("on disk after the drop: %x, want a and c", ids(on...))
	}
	f.raft.setLeader(true)
	o.Drain(context.Background())
	if got := f.raft.log(); !slices.Equal(got, ids(a, c)) {
		t.Fatalf("committed %x, want a then c", got)
	}
}

// A prefix that names no queued message, or more than one, or is not hex,
// drops nothing.
func TestDropRefusesWhatItCannotName(t *testing.T) {
	f := newFixture(t)
	o := f.open()
	for i := range 40 {
		f.write(o, f.talk(fmt.Sprint(i)))
	}
	for prefix, want := range map[string]error{
		"":                      outbox.ErrBadMsgID,
		"xyz":                   outbox.ErrBadMsgID,
		strings.Repeat("0", 65): outbox.ErrBadMsgID,
	} {
		if _, err := o.Drop(prefix); !errors.Is(err, want) {
			t.Errorf("Drop(%q) = %v, want %v", prefix, err, want)
		}
	}
	// 40 messages over 16 first hex digits: some digit starts two of them.
	byDigit := map[byte]int{}
	for _, s := range o.Entries() {
		byDigit[fmt.Sprintf("%x", s.MsgID[:1])[0]]++
	}
	for d, n := range byDigit {
		if n > 1 {
			if _, err := o.Drop(string(d)); !errors.Is(err, outbox.ErrAmbiguous) {
				t.Errorf("Drop(%q) = %v, want ErrAmbiguous", d, err)
			}
			break
		}
	}
	if _, err := o.Drop(strings.Repeat("0", 64)); !errors.Is(err, outbox.ErrNotQueued) {
		t.Errorf("Drop(zeros) = %v, want ErrNotQueued", err)
	}
	if n := len(o.Entries()); n != 40 {
		t.Fatalf("%d left, want all 40", n)
	}
}

// A message that has committed is on the log: Drop says so, with its seq,
// and erases nothing.
func TestDropOfACommittedMessageSaysItCommitted(t *testing.T) {
	f := newFixture(t)
	o := f.open()
	a := f.talk("a")
	f.write(o, a)
	f.raft.setLeader(true)
	o.Drain(context.Background())
	_, err := o.Drop(fmt.Sprintf("%x", a.MsgID))
	var committed *outbox.CommittedError
	if !errors.As(err, &committed) || committed.Seq != 1 || !errors.Is(err, outbox.ErrCommitted) {
		t.Fatalf("Drop = %v, want committed at seq 1", err)
	}
}

// blockingRaft holds each append until released, so a test can drop a
// message while the drain is committing it.
type blockingRaft struct {
	raft
	started chan struct{}
	release chan struct{}
}

func (r *blockingRaft) append(ctx context.Context, s cosl.Signed) (raftx.Position, error) {
	r.started <- struct{}{}
	<-r.release
	return r.raft.append(ctx, s)
}

// Dropping the message the drain is committing right now waits for that
// commit, then reports that it committed: it is never both dropped and
// committed.
func TestDropDuringItsCommitReportsTheCommit(t *testing.T) {
	f := newFixture(t)
	br := &blockingRaft{raft: raft{leader: true, reject: map[[32]byte]bool{}}, started: make(chan struct{}), release: make(chan struct{})}
	a := f.talk("a")
	f.write(f.open(), a) // no leader: queued on disk
	o, err := outbox.Open(outbox.Config{Path: f.path, Append: br.append})
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() { o.Drain(context.Background()); close(drained) }()
	<-br.started
	dropped := make(chan error)
	go func() { _, err := o.Drop(fmt.Sprintf("%x", a.MsgID[:4])); dropped <- err }()
	time.Sleep(50 * time.Millisecond)
	close(br.release)
	<-drained
	if err := <-dropped; !errors.Is(err, outbox.ErrCommitted) {
		t.Fatalf("Drop during the commit = %v, want ErrCommitted", err)
	}
	if got := br.log(); !slices.Equal(got, ids(a)) {
		t.Fatalf("committed %x, want a", got)
	}
}

// A message an append of which ended with no clear outcome may yet commit:
// Drop refuses it until a later attempt settles it, then reports how.
func TestDropRefusesAMessageThatMayYetCommit(t *testing.T) {
	f := newFixture(t)
	a := f.talk("a")
	f.write(f.open(), a) // no leader: queued on disk
	maybe := true
	o, err := outbox.Open(outbox.Config{Path: f.path, Append: func(ctx context.Context, s cosl.Signed) (raftx.Position, error) {
		if maybe {
			return raftx.Position{}, fmt.Errorf("%w: %w", raftx.ErrNoLeader, raftx.ErrMaybeCommitted)
		}
		return f.raft.append(ctx, s)
	}})
	if err != nil {
		t.Fatal(err)
	}
	o.Drain(context.Background())
	if _, err := o.Drop(fmt.Sprintf("%x", a.MsgID)); !errors.Is(err, outbox.ErrInFlight) {
		t.Fatalf("Drop = %v, want ErrInFlight", err)
	}
	maybe = false
	f.raft.setLeader(true)
	o.Drain(context.Background())
	if _, err := o.Drop(fmt.Sprintf("%x", a.MsgID)); !errors.Is(err, outbox.ErrCommitted) {
		t.Fatalf("Drop after the drain = %v, want ErrCommitted", err)
	}
}

// A plain no-leader append never reached a log: the message may be dropped.
func TestDropAfterAPlainNoLeader(t *testing.T) {
	f := newFixture(t)
	o := f.open()
	a := f.talk("a")
	f.write(o, a)
	o.Drain(context.Background())
	if _, err := o.Drop(fmt.Sprintf("%x", a.MsgID)); err != nil {
		t.Fatalf("Drop = %v", err)
	}
}

// Lock is exclusive: while one holder has it, a try fails with ErrBusy.
func TestLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.json")
	unlock, err := outbox.Lock(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Lock(path, false); !errors.Is(err, outbox.ErrBusy) {
		t.Fatalf("second Lock = %v, want ErrBusy", err)
	}
	unlock()
	again, err := outbox.Lock(path, false)
	if err != nil {
		t.Fatalf("Lock after unlock = %v", err)
	}
	again()
}
