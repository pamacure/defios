package session_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/nacl/box"
	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/session"
	coslv1 "defios.com/proto/cosl/v1"
)

// double is a keygen engine that records what the Session layer asks of it.
type double struct {
	mu      sync.Mutex
	starts  []session.Params
	recvs   []recv
	out     chan session.Outgoing
	done    chan session.Result
	script  func(session.Params, chan session.Outgoing)
	waiting [][32]byte
	finish  *session.Result
	onRecv  func([]byte)
}

type recv struct {
	from      [32]byte
	broadcast bool
	data      []byte
}

func newDouble() *double {
	return &double{out: make(chan session.Outgoing, 16), done: make(chan session.Result, 1)}
}

func (d *double) Start(p session.Params) error {
	d.mu.Lock()
	d.starts = append(d.starts, p)
	script := d.script
	d.mu.Unlock()
	if script != nil {
		script(p, d.out)
	}
	if d.finish != nil {
		d.done <- *d.finish
	}
	return nil
}

func (d *double) Receive(from [32]byte, isBroadcast bool, data []byte) error {
	if d.onRecv != nil {
		d.onRecv(append([]byte(nil), data...))
	}
	d.mu.Lock()
	d.recvs = append(d.recvs, recv{from: from, broadcast: isBroadcast, data: append([]byte(nil), data...)})
	d.mu.Unlock()
	return nil
}

func (d *double) Out() <-chan session.Outgoing { return d.out }
func (d *double) Done() <-chan session.Result  { return d.done }
func (d *double) WaitingFor() [][32]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][32]byte(nil), d.waiting...)
}
func (d *double) Abort() {}

func (d *double) started() []session.Params {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]session.Params(nil), d.starts...)
}

func (d *double) received() []recv {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]recv, len(d.recvs))
	copy(out, d.recvs)
	return out
}

// node is one participant. Appends land on every replica, the way Raft does,
// so a routing tag cannot be used as a delivery address.
type node struct {
	t           *testing.T
	id          *identity.Node
	name        string
	store       *cosl.Store
	svc         *session.Service
	roster      group.Roster
	waitingFor  time.Duration
	acceptPath  string
	pendingPath string
	shares      session.ShareStore
	signInfo    func([32]byte) (session.SignInfo, error)
	finish      *session.Result
	clock       func() time.Time
	append      func(context.Context, cosl.Signed) error
	appendFn    func(context.Context, cosl.Signed) error
	waiting     [][32]byte
	mu          sync.Mutex
	doubles     []*double
	script      func(session.Params, chan session.Outgoing)
	onRecv      func([]byte)
	logs        []string
	cancel      context.CancelFunc
	done        chan struct{}
}

// pair is two Nodes sharing one Roster. Appends fan out to both replicas.
type pair struct {
	t      *testing.T
	roster group.Roster
	fan    *fanout
	alice  *node
	bob    *node
	carol  *node // nil unless newTrio
	nowMu  sync.Mutex
	now    time.Time
}

func newPair(t *testing.T, waitingFor time.Duration) *pair { return newCluster(t, waitingFor, false) }

// newTrio is a Roster of three, for Sessions that sign with a subset.
func newTrio(t *testing.T, waitingFor time.Duration) *pair { return newCluster(t, waitingFor, true) }

func newCluster(t *testing.T, waitingFor time.Duration, withCarol bool) *pair {
	t.Helper()
	ids := make([]*identity.Node, 3)
	for i := range ids {
		n, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = n
	}
	nodes := []group.RosterNode{
		{Public: ids[0].Public, DisplayName: "alice", TsnetHostname: "defios-alice"},
		{Public: ids[1].Public, DisplayName: "bob", TsnetHostname: "defios-bob"},
	}
	if withCarol {
		nodes = append(nodes, group.RosterNode{Public: ids[2].Public, DisplayName: "carol", TsnetHostname: "defios-carol"})
	}
	roster := group.NewRoster(nodes...)
	c := &pair{t: t, roster: roster, fan: &fanout{}, now: time.Now()}
	c.alice = c.open(ids[0], "alice", waitingFor)
	c.bob = c.open(ids[1], "bob", waitingFor)
	all := []*node{c.alice, c.bob}
	if withCarol {
		c.carol = c.open(ids[2], "carol", waitingFor)
		all = append(all, c.carol)
	}
	t.Cleanup(func() {
		for _, n := range all {
			n.stop()
		}
		for _, n := range all {
			n.store.Close()
		}
	})
	return c
}

func (c *pair) open(id *identity.Node, name string, waitingFor time.Duration) *node {
	c.t.Helper()
	dir := c.t.TempDir()
	store, err := cosl.Open(dir + "/cosl.sqlite")
	if err != nil {
		c.t.Fatal(err)
	}
	c.fan.add(cosl.NewFSM(store, c.roster))
	n := &node{
		t: c.t, id: id, name: name, store: store, roster: c.roster,
		waitingFor: waitingFor, acceptPath: dir + "/local-accept.json",
		clock: c.nowFn,
	}
	n.appendFn = c.fan.Append
	n.append = n.dispatchAppend
	n.start()
	return n
}

func (c *pair) nowFn() time.Time {
	c.nowMu.Lock()
	defer c.nowMu.Unlock()
	return c.now
}

func (c *pair) advance(d time.Duration) {
	c.nowMu.Lock()
	c.now = c.now.Add(d)
	c.nowMu.Unlock()
}

func (n *node) start() {
	n.t.Helper()
	svc, err := session.New(session.Config{
		Node: n.id, Roster: n.roster, Store: n.store, Append: n.append,
		NewEngine: n.engine, NewSigner: n.engine, SignInfo: n.lookupSign, Preparams: []byte("ecdsa-preparams"),
		WaitingFor: n.waitingFor, Now: n.clock, Logf: n.logf, AcceptPath: n.acceptPath,
		Shares: n.shares, PendingPath: n.pendingPath,
		Tick: 5 * time.Millisecond,
	})
	if err != nil {
		n.t.Fatal(err)
	}
	n.svc = svc
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	n.done = make(chan struct{})
	go func() { svc.Run(ctx); close(n.done) }()
}

func (n *node) lookupSign(id [32]byte) (session.SignInfo, error) {
	if n.signInfo == nil {
		return session.SignInfo{}, errors.New("no sign request")
	}
	return n.signInfo(id)
}

func (n *node) stop() {
	if n.cancel == nil {
		return
	}
	n.cancel()
	<-n.done
	n.cancel = nil
}

// restart drops the in-memory party the way a serve restart does. acceptPath
// empty means the local-accept file is gone; the initiator is still detected
// from the start row.
func (n *node) restart(acceptPath string) {
	n.t.Helper()
	n.stop()
	n.mu.Lock()
	n.doubles = nil
	n.mu.Unlock()
	n.acceptPath = acceptPath
	n.start()
}

func (n *node) dispatchAppend(ctx context.Context, signed cosl.Signed) error {
	n.mu.Lock()
	fn := n.appendFn
	n.mu.Unlock()
	return fn(ctx, signed)
}

func (n *node) setAppend(fn func(context.Context, cosl.Signed) error) {
	n.mu.Lock()
	n.appendFn = fn
	n.mu.Unlock()
}

func (n *node) setWaiting(ids [][32]byte) {
	n.mu.Lock()
	n.waiting = append([][32]byte(nil), ids...)
	for _, d := range n.doubles {
		d.mu.Lock()
		d.waiting = append([][32]byte(nil), ids...)
		d.mu.Unlock()
	}
	n.mu.Unlock()
}

func (n *node) engine() session.Engine {
	d := newDouble()
	n.mu.Lock()
	d.script = n.script
	d.onRecv = n.onRecv
	d.finish = n.finish
	d.waiting = append([][32]byte(nil), n.waiting...)
	n.doubles = append(n.doubles, d)
	n.mu.Unlock()
	return d
}

func (n *node) logf(format string, args ...any) {
	n.mu.Lock()
	n.logs = append(n.logs, fmt.Sprintf(format, args...))
	n.mu.Unlock()
}

func (n *node) setScript(fn func(session.Params, chan session.Outgoing)) {
	n.mu.Lock()
	n.script = fn
	n.mu.Unlock()
}

func (n *node) setOnRecv(fn func([]byte)) {
	n.mu.Lock()
	n.onRecv = fn
	n.mu.Unlock()
}

func (n *node) engines() []*double {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]*double, len(n.doubles))
	copy(out, n.doubles)
	return out
}

func (n *node) logLines() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, len(n.logs))
	copy(out, n.logs)
	return out
}

type fanout struct {
	mu   sync.Mutex
	next uint64
	fsms []*cosl.FSM
}

func (f *fanout) add(fsm *cosl.FSM) {
	f.mu.Lock()
	f.fsms = append(f.fsms, fsm)
	f.mu.Unlock()
}

func (f *fanout) Append(_ context.Context, signed cosl.Signed) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	for _, fsm := range f.fsms {
		res, err := fsm.Apply(f.next, 1, signed.Bytes)
		if err != nil {
			return err
		}
		if !res.Inserted {
			return fmt.Errorf("not inserted: %s", res.Reason)
		}
	}
	return nil
}

func wait(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting")
}

func (n *node) rows(t *testing.T) []cosl.Row {
	t.Helper()
	rows, err := n.store.Log(cosl.Query{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func (n *node) ofType(t *testing.T, typ uint32) []cosl.Row {
	t.Helper()
	var out []cosl.Row
	for _, r := range n.rows(t) {
		if r.MsgType == typ {
			out = append(out, r)
		}
	}
	return out
}

func decode(t *testing.T, row cosl.Row) proto.Message {
	t.Helper()
	env, err := row.Decode()
	if err != nil {
		t.Fatal(err)
	}
	msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func mustID(t *testing.T, hexID string) []byte {
	t.Helper()
	id, err := session.ParseID(hexID)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), id[:]...)
}

func (c *pair) sign(n *node, typ uint32, payload proto.Message) cosl.Signed {
	c.t.Helper()
	signed, err := (cosl.Author{Node: n.id, GroupID: c.roster.GroupID()}).Sign(typ, nil, payload)
	if err != nil {
		c.t.Fatal(err)
	}
	return signed
}

// signIn is sign with a conversation id, as a sign.accept needs.
func (c *pair) signIn(n *node, typ uint32, conv []byte, payload proto.Message) cosl.Signed {
	c.t.Helper()
	signed, err := (cosl.Author{Node: n.id, GroupID: c.roster.GroupID()}).Sign(typ, conv, payload)
	if err != nil {
		c.t.Fatal(err)
	}
	return signed
}

func listed(t *testing.T, n *node) []session.Summary {
	t.Helper()
	out, err := session.List(n.rows(t))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestListAndInspectReportStatusParticipantsAndCompletes(t *testing.T) {
	alice, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	bob, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	roster := group.NewRoster(
		group.RosterNode{Public: alice.Public, DisplayName: "alice", TsnetHostname: "defios-alice"},
		group.RosterNode{Public: bob.Public, DisplayName: "bob", TsnetHostname: "defios-bob"},
	)
	store, err := cosl.Open(t.TempDir() + "/cosl.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fsm := cosl.NewFSM(store, roster)
	gid := roster.GroupID()
	var seq uint64
	apply := func(n *identity.Node, typ uint32, payload proto.Message) {
		t.Helper()
		signed, err := (cosl.Author{Node: n, GroupID: gid}).Sign(typ, nil, payload)
		if err != nil {
			t.Fatal(err)
		}
		seq++
		res, err := fsm.Apply(seq, 1, signed.Bytes)
		if err != nil || !res.Inserted {
			t.Fatalf("apply: %v %+v", err, res)
		}
	}
	var aID, bID session.ID
	if _, err := rand.Read(aID[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(bID[:]); err != nil {
		t.Fatal(err)
	}
	ids := [][]byte{append([]byte(nil), roster.NodeIDs()[0][:]...), append([]byte(nil), roster.NodeIDs()[1][:]...)}
	apply(alice, cosl.TypeSessionStart, &coslv1.SessionStart{
		SessionId: aID[:], Curve: "ecdsa", T: 2, N: 2, Participants: ids,
	})
	apply(bob, cosl.TypeSessionAbort, &coslv1.SessionAbort{SessionId: aID[:], Reason: "operator stop"})
	apply(alice, cosl.TypeSessionStart, &coslv1.SessionStart{
		SessionId: bID[:], Curve: "eddsa", T: 2, N: 2, Participants: ids,
	})
	pk := []byte{0x02, 0xaa}
	apply(alice, cosl.TypeSessionComplete, &coslv1.SessionComplete{SessionId: bID[:], Curve: "eddsa", PublicKey: pk})

	rows, err := store.Log(cosl.Query{})
	if err != nil {
		t.Fatal(err)
	}
	list, err := session.List(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].SessionID != aID.String() || list[0].Status != session.Aborted || list[0].Curve != "ecdsa" || list[0].T != 2 || list[0].N != 2 {
		t.Fatalf("list[0] = %+v", list[0])
	}
	if list[1].SessionID != bID.String() || list[1].Status != session.Open || list[1].Curve != "eddsa" {
		t.Fatalf("list[1] = %+v", list[1])
	}
	detail, err := session.Inspect(rows, bID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Participants) != 2 || detail.Participants[0] != roster.NodeIDs()[0].String() || len(detail.Completes) != 1 {
		t.Fatalf("detail = %+v", detail)
	}
	if detail.Completes[0].Sender != alice.NodeID.String() || detail.Completes[0].Curve != "eddsa" || detail.Completes[0].PublicKey != "02aa" {
		t.Fatalf("complete = %+v", detail.Completes[0])
	}
	aborted, err := session.Inspect(rows, aID)
	if err != nil || aborted.Status != session.Aborted || aborted.Reason != "operator stop" {
		t.Fatalf("aborted = %+v %v", aborted, err)
	}

	apply(bob, cosl.TypeSessionComplete, &coslv1.SessionComplete{SessionId: bID[:], Curve: "eddsa", PublicKey: pk})
	rows, err = store.Log(cosl.Query{})
	if err != nil {
		t.Fatal(err)
	}
	detail, err = session.Inspect(rows, bID)
	if err != nil || detail.Status != session.Complete || len(detail.Completes) != 2 {
		t.Fatalf("after both completes: %+v %v", detail, err)
	}
	apply(alice, cosl.TypeSessionAbort, &coslv1.SessionAbort{SessionId: bID[:], Reason: "late"})
	rows, err = store.Log(cosl.Query{})
	if err != nil {
		t.Fatal(err)
	}
	detail, err = session.Inspect(rows, bID)
	if err != nil || detail.Status != session.Complete {
		t.Fatalf("agreeing completes stay a Key after abort: %+v %v", detail, err)
	}
	minted, err := session.CompletedSessions(rows)
	if err != nil || len(minted) != 1 || minted[0].SessionID != bID {
		t.Fatalf("minted = %+v %v", minted, err)
	}
	if _, err := session.Inspect(rows, session.ID{}); !errors.Is(err, session.ErrUnknownSession) {
		t.Fatalf("missing session: %v", err)
	}
}

func TestStartSealsTheNonceToEachParticipant(t *testing.T) {
	c := newPair(t, time.Hour)
	sum, err := c.alice.svc.Start(context.Background(), "ecdsa", 2)
	if err != nil {
		t.Fatal(err)
	}
	if sum.N != 2 || sum.T != 2 || sum.Curve != "ecdsa" || sum.Status != session.Open {
		t.Fatalf("summary = %+v", sum)
	}
	var aliceEng *double
	wait(t, func() bool {
		es := c.alice.engines()
		if len(es) == 0 || len(es[0].started()) == 0 {
			return false
		}
		aliceEng = es[0]
		return true
	})
	if len(c.bob.engines()) != 0 {
		t.Fatal("bob started an engine before accept")
	}
	nonce := aliceEng.started()[0].Nonce
	if nonce == ([32]byte{}) {
		t.Fatal("nonce is zero")
	}
	if !bytes.Equal(aliceEng.started()[0].Preparams, []byte("ecdsa-preparams")) {
		t.Fatalf("preparams = %q", aliceEng.started()[0].Preparams)
	}
	starts := c.alice.ofType(t, cosl.TypeSessionStart)
	if len(starts) != 1 {
		t.Fatalf("starts = %d", len(starts))
	}
	msg := decode(t, starts[0]).(*coslv1.SessionStart)
	if msg.N != 2 || msg.T != 2 || len(msg.Participants) != 2 || len(msg.NonceBoxes) != 2 {
		t.Fatalf("start = %+v", msg)
	}
	for i, id := range c.roster.NodeIDs() {
		if !bytes.Equal(msg.Participants[i], id[:]) {
			t.Fatalf("participant %d = %x, roster %x", i, msg.Participants[i], id[:])
		}
	}
	keys := map[identity.ID][]byte{
		c.alice.id.NodeID: c.alice.id.EncryptionPrivateKey,
		c.bob.id.NodeID:   c.bob.id.EncryptionPrivateKey,
	}
	opened := map[identity.ID]bool{}
	for _, b := range msg.NonceBoxes {
		var to identity.ID
		copy(to[:], b.To)
		plain, ok := openBox(b, keys[to])
		if !ok || !bytes.Equal(plain, nonce[:]) {
			t.Fatalf("box for %s did not open to the engine nonce", to)
		}
		opened[to] = true
		// The other Node's key must not open this box.
		other := c.bob.id.EncryptionPrivateKey
		if to == c.bob.id.NodeID {
			other = c.alice.id.EncryptionPrivateKey
		}
		if _, ok := openBox(b, other); ok {
			t.Fatalf("box for %s opened with the wrong key", to)
		}
	}
	if len(opened) != 2 {
		t.Fatalf("opened %d boxes", len(opened))
	}
	// The start row carries boxes, not the preimage.
	env, err := starts[0].Decode()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(env.Payload, nonce[:]) {
		t.Fatal("plaintext nonce is a COSL field")
	}

	_, err = c.alice.svc.Start(context.Background(), "rsa", 2)
	if !errors.Is(err, session.ErrBadCurve) {
		t.Fatalf("bad curve: %v", err)
	}
}

func openBox(b *coslv1.SealedBox, priv []byte) ([]byte, bool) {
	if len(b.Nonce) != 24 || len(b.EphemeralPublicKey) != 32 || len(priv) != 32 {
		return nil, false
	}
	var nonce [24]byte
	var eph, rp [32]byte
	copy(nonce[:], b.Nonce)
	copy(eph[:], b.EphemeralPublicKey)
	copy(rp[:], priv)
	return box.Open(nil, b.Ciphertext, &nonce, &eph, &rp)
}

func TestOtherNodeDoesNotStartUntilLocalAccept(t *testing.T) {
	c := newPair(t, time.Hour)
	sum, err := c.alice.svc.Start(context.Background(), "ecdsa", 2)
	if err != nil {
		t.Fatal(err)
	}
	want := "session " + sum.SessionID + " waiting for accept"
	wait(t, func() bool {
		for _, line := range c.bob.logLines() {
			if line == want {
				return true
			}
		}
		return false
	})
	if len(c.bob.engines()) != 0 {
		t.Fatal("bob started an engine while waiting for accept")
	}
	if len(c.bob.ofType(t, cosl.TypeTssSession)) != 0 || len(c.alice.ofType(t, cosl.TypeTssSession)) != 0 {
		t.Fatal("tss.session emitted before the other Node accepted")
	}
	id, err := session.ParseID(sum.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.bob.svc.Accept(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		es := c.bob.engines()
		return len(es) == 1 && len(es[0].started()) == 1
	})
	alice := c.alice.engines()[0].started()[0].Nonce
	bob := c.bob.engines()[0].started()[0].Nonce
	if alice != bob {
		t.Fatal("nodes unsealed different nonces")
	}
}

func TestOutOfRangeTIsAbortedWithoutStartingTheEngine(t *testing.T) {
	c := newPair(t, time.Hour)
	sum, err := c.alice.svc.Start(context.Background(), "ecdsa", 1)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		return abortedReason(t, c.alice, sum.SessionID, "t=1 is not valid for n=2")
	})
	if len(c.alice.engines()) != 0 || len(c.bob.engines()) != 0 {
		t.Fatal("engine started for an invalid t")
	}
	if len(c.alice.ofType(t, cosl.TypeSessionStart)) != 1 {
		t.Fatal("invalid start row was not kept")
	}
}

func TestLaterStartWhileOneIsOpenIsAborted(t *testing.T) {
	c := newPair(t, time.Hour)
	first, err := c.alice.svc.Start(context.Background(), "ecdsa", 2)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		es := c.alice.engines()
		return len(es) == 1 && len(es[0].started()) == 1
	})
	second, err := c.bob.svc.Start(context.Background(), "eddsa", 2)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		return abortedReason(t, c.alice, second.SessionID, "another session is open")
	})
	if len(c.bob.engines()) != 0 {
		t.Fatal("engine started for the later session")
	}
	list := listed(t, c.alice)
	status := map[string]string{}
	for _, s := range list {
		status[s.SessionID] = s.Status
	}
	if status[first.SessionID] != session.Open || status[second.SessionID] != session.Aborted {
		t.Fatalf("status = %v", status)
	}
}

func TestPointToPointIsSealedAndBroadcastIsPlaintextOnEveryReplica(t *testing.T) {
	c := newPair(t, time.Hour)
	to := [32]byte(c.bob.id.NodeID)
	c.alice.setScript(func(_ session.Params, out chan session.Outgoing) {
		out <- session.Outgoing{Data: []byte("hello-all")}
		dst := to
		out <- session.Outgoing{To: &dst, Data: []byte("secret-for-bob")}
	})
	if _, err := c.alice.svc.Start(context.Background(), "ecdsa", 2); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		return len(c.alice.ofType(t, cosl.TypeTssSession)) == 2 && len(c.bob.ofType(t, cosl.TypeTssSession)) == 2
	})
	for _, n := range []*node{c.alice, c.bob} {
		rows := n.ofType(t, cosl.TypeTssSession)
		broadcast := decode(t, rows[0]).(*coslv1.TssSession)
		p2p := decode(t, rows[1]).(*coslv1.TssSession)
		if len(broadcast.RoutingTag) != 0 || len(broadcast.EphemeralPublicKey) != 0 || len(broadcast.Nonce) != 0 || !bytes.Equal(broadcast.Data, []byte("hello-all")) {
			t.Fatalf("%s broadcast = %+v", n.name, broadcast)
		}
		if !bytes.Equal(p2p.RoutingTag, c.bob.id.NodeID[:]) || len(p2p.EphemeralPublicKey) != 32 || len(p2p.Nonce) != 24 || bytes.Equal(p2p.Data, []byte("secret-for-bob")) {
			t.Fatalf("%s p2p = %+v", n.name, p2p)
		}
		plain, ok := openBox(&coslv1.SealedBox{
			EphemeralPublicKey: p2p.EphemeralPublicKey, Nonce: p2p.Nonce, Ciphertext: p2p.Data,
		}, c.bob.id.EncryptionPrivateKey)
		if !ok || !bytes.Equal(plain, []byte("secret-for-bob")) {
			t.Fatalf("%s: bob could not unseal p2p (%v)", n.name, ok)
		}
		if _, ok := openBox(&coslv1.SealedBox{
			EphemeralPublicKey: p2p.EphemeralPublicKey, Nonce: p2p.Nonce, Ciphertext: p2p.Data,
		}, c.alice.id.EncryptionPrivateKey); ok {
			t.Fatalf("%s: alice unsealed a box addressed to bob", n.name)
		}
	}
	id, _ := session.ParseID(listed(t, c.alice)[0].SessionID)
	if err := c.bob.svc.Accept(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		es := c.bob.engines()
		return len(es) == 1 && len(es[0].received()) == 2
	})
	got := c.bob.engines()[0].received()
	if !got[0].broadcast || !bytes.Equal(got[0].data, []byte("hello-all")) || got[1].broadcast || !bytes.Equal(got[1].data, []byte("secret-for-bob")) {
		t.Fatalf("bob received %+v", got)
	}
	if len(c.alice.engines()[0].received()) != 0 {
		t.Fatalf("alice was delivered her own rounds: %+v", c.alice.engines()[0].received())
	}
}

func TestReplayDeliversCommittedRoundsBeforeLiveOutgoing(t *testing.T) {
	c := newPair(t, time.Hour)
	sum, err := c.alice.svc.Start(context.Background(), "ecdsa", 2)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		es := c.alice.engines()
		return len(es) == 1 && len(es[0].started()) == 1
	})
	sid := mustID(t, sum.SessionID)
	for _, data := range []string{"one", "two"} {
		if err := c.fan.Append(context.Background(), c.sign(c.alice, cosl.TypeTssSession, &coslv1.TssSession{
			SessionId: sid, Data: []byte(data),
		})); err != nil {
			t.Fatal(err)
		}
	}
	c.bob.setScript(func(_ session.Params, out chan session.Outgoing) {
		out <- session.Outgoing{Data: []byte("live-from-bob")}
	})
	c.bob.setOnRecv(func(data []byte) {
		if string(data) != "one" {
			return
		}
		for _, row := range c.bob.ofType(t, cosl.TypeTssSession) {
			msg := decode(t, row).(*coslv1.TssSession)
			if bytes.Equal(msg.Data, []byte("live-from-bob")) {
				t.Errorf("live outgoing was on the replica before historical round %q was delivered", data)
			}
		}
	})
	id, _ := session.ParseID(sum.SessionID)
	if err := c.bob.svc.Accept(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		es := c.bob.engines()
		return len(es) == 1 && len(es[0].received()) == 2
	})
	got := c.bob.engines()[0].received()
	if string(got[0].data) != "one" || string(got[1].data) != "two" {
		t.Fatalf("replay order = %q, %q", got[0].data, got[1].data)
	}
	wait(t, func() bool {
		for _, n := range []*node{c.alice, c.bob} {
			found := false
			for _, row := range n.ofType(t, cosl.TypeTssSession) {
				msg := decode(t, row).(*coslv1.TssSession)
				if bytes.Equal(msg.Data, []byte("live-from-bob")) {
					found = true
				}
			}
			if !found {
				return false
			}
		}
		return true
	})
}

func TestSilenceAbortsWhenTheWaitExpires(t *testing.T) {
	c := newPair(t, time.Hour)
	sum, err := c.alice.svc.Start(context.Background(), "ecdsa", 2)
	if err != nil {
		t.Fatal(err)
	}
	want := "session " + sum.SessionID + " waiting for accept"
	wait(t, func() bool {
		for _, line := range c.bob.logLines() {
			if line == want {
				return true
			}
		}
		return false
	})
	if len(c.bob.engines()) != 0 {
		t.Fatal("bob started an engine")
	}
	c.advance(time.Hour + time.Second)
	wait(t, func() bool { return abortedReason(t, c.bob, sum.SessionID, "waiting for accept") })
	if len(c.bob.engines()) != 0 {
		t.Fatal("timeout started bob's engine")
	}
	for _, row := range c.bob.ofType(t, cosl.TypeSessionAbort) {
		msg := decode(t, row).(*coslv1.SessionAbort)
		if row.Sender != c.bob.id.NodeID || msg.Reason != "waiting for accept" {
			continue
		}
		return
	}
	t.Fatal("bob did not append the waiting-for-accept abort")
}

func TestRestartAbortsASessionWhosePartyIsGone(t *testing.T) {
	c := newPair(t, time.Hour)
	sum, err := c.alice.svc.Start(context.Background(), "ecdsa", 2)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		es := c.alice.engines()
		return len(es) == 1 && len(es[0].started()) == 1
	})
	if len(c.alice.ofType(t, cosl.TypeSessionAbort)) != 0 {
		t.Fatal("abort landed before restart")
	}
	// The accept file is intentionally dropped. The start row is this Node's accept.
	c.alice.restart("")
	wait(t, func() bool { return abortedReason(t, c.alice, sum.SessionID, "party state lost") })
	if len(c.alice.engines()) != 0 {
		t.Fatal("restarted initiator started a fresh engine")
	}

	c2 := newPair(t, time.Hour)
	sum, err = c2.alice.svc.Start(context.Background(), "ecdsa", 2)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := session.ParseID(sum.SessionID)
	if err := c2.bob.svc.Accept(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		es := c2.bob.engines()
		return len(es) == 1 && len(es[0].started()) == 1
	})
	path := c2.bob.acceptPath
	c2.bob.restart(path)
	wait(t, func() bool { return abortedReason(t, c2.bob, sum.SessionID, "party state lost") })
	if len(c2.bob.engines()) != 0 {
		t.Fatal("restarted acceptor started a fresh engine")
	}
}

func TestOperatorAbortAppendsTheReasonOnce(t *testing.T) {
	c := newPair(t, time.Hour)
	sum, err := c.alice.svc.Start(context.Background(), "ecdsa", 2)
	if err != nil {
		t.Fatal(err)
	}
	id, err := session.ParseID(sum.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.bob.svc.Abort(context.Background(), id, "nope"); err != nil {
		t.Fatal(err)
	}
	if err := c.bob.svc.Abort(context.Background(), id, "again"); err != nil {
		t.Fatal(err)
	}
	if !abortedReason(t, c.alice, sum.SessionID, "nope") {
		t.Fatal("abort reason not on the replica")
	}
	if n := len(c.alice.ofType(t, cosl.TypeSessionAbort)); n != 1 {
		t.Fatalf("abort rows = %d", n)
	}
	if err := c.bob.svc.Accept(context.Background(), id); !errors.Is(err, session.ErrClosedSession) {
		t.Fatalf("accept after abort: %v", err)
	}
	var missing session.ID
	missing[0] = 1
	if err := c.bob.svc.Abort(context.Background(), missing, ""); !errors.Is(err, session.ErrUnknownSession) {
		t.Fatalf("abort missing: %v", err)
	}
}

func TestLiveWaitAbortIsRetriedWhenAppendFails(t *testing.T) {
	c := newPair(t, time.Hour)
	var attempts atomic.Int32
	c.alice.setAppend(func(ctx context.Context, signed cosl.Signed) error {
		var env coslv1.Envelope
		if err := proto.Unmarshal(signed.Bytes, &env); err != nil {
			t.Errorf("envelope: %v", err)
			return err
		}
		if env.MsgType == cosl.TypeSessionAbort && attempts.Add(1) == 1 {
			return errors.New("no leader")
		}
		return c.fan.Append(ctx, signed)
	})
	c.alice.setWaiting([][32]byte{[32]byte(c.bob.id.NodeID)})

	sum, err := c.alice.svc.Start(context.Background(), "ecdsa", 2)
	if err != nil {
		t.Fatal(err)
	}
	id, err := session.ParseID(sum.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.bob.svc.Accept(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		return len(c.alice.engines()) == 1 && len(c.alice.engines()[0].started()) == 1
	})
	time.Sleep(30 * time.Millisecond)
	c.advance(time.Hour + time.Second)
	wait(t, func() bool {
		return abortedReason(t, c.alice, sum.SessionID, "timed out waiting for parties")
	})
	if attempts.Load() < 2 {
		t.Fatalf("abort attempts = %d, want a retry after the first failure", attempts.Load())
	}
	if n := len(c.alice.ofType(t, cosl.TypeSessionAbort)); n != 1 {
		t.Fatalf("abort rows = %d", n)
	}
	if got := len(c.alice.engines()[0].started()); got != 1 {
		t.Fatalf("engine starts = %d, want 1", got)
	}
}

type memShares struct {
	mu     sync.Mutex
	shares map[string][]byte
	onPut  func()
	puts   int
}

func (m *memShares) key(curve string, publicKey []byte) string {
	return curve + ":" + hexEncode(publicKey)
}

func (m *memShares) Put(curve string, publicKey, saveData []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shares == nil {
		m.shares = map[string][]byte{}
	}
	m.shares[m.key(curve, publicKey)] = append([]byte(nil), saveData...)
	m.puts++
	if m.onPut != nil {
		m.onPut()
	}
	return nil
}

func (m *memShares) Delete(curve string, publicKey []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.shares, m.key(curve, publicKey))
	return nil
}

func (m *memShares) Get(curve string, publicKey []byte) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.shares[m.key(curve, publicKey)]
	return v, ok
}

func (m *memShares) has(curve string, publicKey []byte) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.shares[m.key(curve, publicKey)]
	return ok
}

func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = digits[c>>4]
		out[i*2+1] = digits[c&0x0f]
	}
	return string(out)
}

func TestShareIsWrittenBeforeCompleteAndRemovedWhenCompletesDisagree(t *testing.T) {
	c := newPair(t, time.Hour)
	aliceShares := &memShares{}
	bobShares := &memShares{}
	c.alice.shares = aliceShares
	c.bob.shares = bobShares
	c.alice.restart(c.alice.acceptPath)
	c.bob.restart(c.bob.acceptPath)

	pk := bytes.Repeat([]byte{0x02}, 33)
	other := bytes.Repeat([]byte{0x03}, 33)
	var completesAtPut int
	aliceShares.onPut = func() {
		completesAtPut = len(c.alice.ofType(t, cosl.TypeSessionComplete))
	}
	c.alice.finish = &session.Result{PublicKey: pk, SaveData: []byte("alice-share")}
	c.bob.finish = &session.Result{PublicKey: other, SaveData: []byte("bob-share")}

	sum, err := c.alice.svc.Start(context.Background(), "ecdsa", 2)
	if err != nil {
		t.Fatal(err)
	}
	id, err := session.ParseID(sum.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.bob.svc.Accept(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		return abortedReason(t, c.alice, sum.SessionID, "completes disagree") && !aliceShares.has("ecdsa", pk) && !bobShares.has("ecdsa", other)
	})
	if aliceShares.puts < 1 || bobShares.puts < 1 || completesAtPut != 0 {
		t.Fatalf("puts alice=%d bob=%d, completes visible at alice Put = %d", aliceShares.puts, bobShares.puts, completesAtPut)
	}
}

func abortedReason(t *testing.T, n *node, sessionID, reason string) bool {
	t.Helper()
	id, err := session.ParseID(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := session.Inspect(n.rows(t), id)
	if err != nil {
		return false
	}
	return detail.Status == session.Aborted && detail.Reason == reason
}
