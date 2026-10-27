// Package coslbench builds large, realistic COSL replicas for benchmarks of
// the code that reads them (docs/research/cosl-polling-benchmark.md). A
// replica holds a two-Node Group's history: Keys minted by keygen Sessions,
// Sign Requests signed by signing Sessions, and talk with Recaps. Round
// traffic is sized from a measured tss-lib run, since it dominates the
// bytes.
package coslbench

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
)

// Round traffic of one Session, measured from internal/engine/tssl with
// two parties: ECDSA keygen is 8 messages totalling 449,191 bytes, a
// one-Digest ECDSA signing 20 totalling 21,494, EdDSA keygen 6 totalling
// 1,088.
const (
	ecdsaKeygenRounds    = 8
	ecdsaKeygenRoundSize = 449191 / ecdsaKeygenRounds
	signRounds           = 20
	signRoundSize        = 21494 / signRounds
	eddsaKeygenRounds    = 6
	eddsaKeygenRoundSize = 1088 / eddsaKeygenRounds
)

// talkText is about 60 tokens, where ADR 0012 measured text starting to
// outweigh a row's envelope.
var talkText = strings.Repeat("the quick brown fox jumps over the lazy dog ", 6)

// recapEvery is how many messages of talk pass between Recaps.
const recapEvery = 1000

// Shape is how much history a replica holds.
type Shape struct {
	Name      string
	Talk      int // app.freeform rows, half Group-level, half in the signing Key's Conversation
	ECDSAKeys int // ECDSA keygen Sessions; the first Key signs
	EdDSAKeys int
	Signings  int // completed signing Sessions, one Digest each
}

// Shapes are the replica sizes the benchmarks run over.
var Shapes = []Shape{
	{Name: "small", Talk: 1_000, ECDSAKeys: 2, EdDSAKeys: 1, Signings: 10},
	{Name: "medium", Talk: 10_000, ECDSAKeys: 5, EdDSAKeys: 2, Signings: 100},
	{Name: "large", Talk: 50_000, ECDSAKeys: 10, EdDSAKeys: 5, Signings: 500},
}

// Replica is a built replica file and what a reader needs to make sense of
// it.
type Replica struct {
	Path   string
	Nodes  []*identity.Node // alice, bob: the Roster
	Roster group.Roster
	// SignKey is the ECDSA Key every signing Session used, and Conversation
	// its Conversation ID. Each completed signing Session carries Signature,
	// a valid r||s||v signature of Digest under it.
	SignKey      []byte
	Conversation []byte
	Digest       [32]byte
	Signature    []byte
}

// Build returns a copy of the replica for shape in a fresh temporary
// directory, so a benchmark may open and write it. Building runs every row
// through the FSM and takes a while, so the replica is cached under the
// system temp directory and reused while this package's layout version is
// unchanged; identities and keys are fixed so a cached replica stays valid.
func Build(tb testing.TB, shape Shape) Replica {
	tb.Helper()
	r := fixed(tb)
	cached := filepath.Join(os.TempDir(), "defios-coslbench", layout, shape.Name+".sqlite")
	if _, err := os.Stat(cached); err != nil {
		if err := build(r, shape, cached); err != nil {
			tb.Fatalf("coslbench: building %s: %v", shape.Name, err)
		}
	}
	r.Path = filepath.Join(tb.TempDir(), "cosl.sqlite")
	raw, err := os.ReadFile(cached)
	if err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(r.Path, raw, 0o600); err != nil {
		tb.Fatal(err)
	}
	return r
}

// layout names the cache; bump it whenever build writes different rows.
const layout = "v1"

// fixed is the Group, Key and signature every replica shares.
func fixed(tb testing.TB) Replica {
	tb.Helper()
	var nodes []*identity.Node
	var members []group.RosterNode
	for i, name := range []string{"alice", "bob"} {
		n, err := identity.FromSeed(seed("node", i))
		if err != nil {
			tb.Fatal(err)
		}
		nodes = append(nodes, n)
		members = append(members, group.RosterNode{Public: n.Public, DisplayName: name, TsnetHostname: "defios-" + name})
	}
	priv, _ := btcec.PrivKeyFromBytes(seed("sign-key", 0))
	pub := priv.PubKey().SerializeCompressed()
	digest := sha256.Sum256([]byte("coslbench digest"))
	compact, err := ecdsa.SignCompact(priv, digest[:], true)
	if err != nil {
		tb.Fatal(err)
	}
	sig := append(append([]byte(nil), compact[1:65]...), compact[0]-31) // v||r||s to r||s||v
	conv := conversationID("ecdsa", pub)
	return Replica{
		Nodes: nodes, Roster: group.NewRoster(members...),
		SignKey: pub, Conversation: conv[:], Digest: digest, Signature: sig,
	}
}

// conversationID is key.ConversationID, which this package cannot import:
// key imports session, whose benchmarks import this.
func conversationID(curve string, publicKey []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte("defios-conversation-id-v1"))
	h.Write([]byte(curve))
	h.Write(publicKey)
	return [32]byte(h.Sum(nil))
}

func seed(label string, i int) []byte {
	s := sha256.Sum256(fmt.Appendf(nil, "coslbench %s %d", label, i))
	return s[:]
}

// writer commits rows to a replica being built.
type writer struct {
	r       Replica
	store   *cosl.Store
	fsm     *cosl.FSM
	authors []cosl.Author
	seq     uint64
	err     error
}

func (w *writer) commit(from int, typ uint32, conv []byte, msg proto.Message) uint64 {
	if w.err != nil {
		return 0
	}
	s, err := w.authors[from].Sign(typ, conv, msg)
	if err != nil {
		w.err = err
		return 0
	}
	w.seq++
	res, err := w.fsm.Apply(w.seq, 1, s.Bytes)
	if err == nil && !res.Inserted {
		err = fmt.Errorf("seq %d not inserted: %+v", w.seq, res)
	}
	w.err = err
	return w.seq
}

func build(r Replica, shape Shape, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".building"
	os.Remove(tmp)
	store, err := cosl.Open(tmp)
	if err != nil {
		return err
	}
	w := &writer{r: r, store: store, fsm: cosl.NewFSM(store, r.Roster)}
	for _, n := range r.Nodes {
		w.authors = append(w.authors, cosl.Author{Node: n, GroupID: r.Roster.GroupID()})
	}
	for i, n := range r.Nodes {
		w.commit(i, cosl.TypeMembershipJoin, nil, cosl.MembershipJoin(n, []string{"alice", "bob"}[i], "defios-"+[]string{"alice", "bob"}[i]))
	}
	for i := range shape.ECDSAKeys {
		pub := r.SignKey
		if i > 0 {
			p, _ := btcec.PrivKeyFromBytes(seed("ecdsa-key", i))
			pub = p.PubKey().SerializeCompressed()
		}
		w.keygen(sessionID("ecdsa", i), "ecdsa", pub, ecdsaKeygenRounds, ecdsaKeygenRoundSize)
	}
	for i := range shape.EdDSAKeys {
		pub := seed("eddsa-key", i) // 32 bytes; nothing here checks it is a point
		w.keygen(sessionID("eddsa", i), "eddsa", pub, eddsaKeygenRounds, eddsaKeygenRoundSize)
	}
	// Talk with a signing every so often, so signings sit among the talk as
	// they would in a Conversation about them.
	signEvery := 0
	if shape.Signings > 0 {
		signEvery = max(1, shape.Talk/shape.Signings)
	}
	signed := 0
	var lastGroup uint64
	for i := range shape.Talk {
		conv := r.Conversation
		if i%2 == 0 {
			conv = nil
		}
		seq := w.commit(i%2, cosl.TypeAppFreeform, conv, &coslv1.AppFreeform{Text: fmt.Sprintf("%d %s", i, talkText)})
		if conv == nil {
			lastGroup = seq
		}
		if (i+1)%recapEvery == 0 {
			w.commit(0, cosl.TypeAppRecap, nil, &coslv1.AppRecap{Text: "recap " + talkText, BaseSeq: lastGroup})
		}
		if signEvery > 0 && (i+1)%signEvery == 0 && signed < shape.Signings {
			w.signing(signed)
			signed++
		}
	}
	for ; signed < shape.Signings; signed++ {
		w.signing(signed)
	}
	if w.err != nil {
		store.Close()
		return w.err
	}
	if err := store.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func sessionID(kind string, i int) []byte { return seed("session-"+kind, i) }

// participants is both Nodes' IDs in order, with a sealed nonce box each.
func (w *writer) participants() ([][]byte, []*coslv1.SealedBox) {
	var ids [][]byte
	var boxes []*coslv1.SealedBox
	for _, n := range w.r.Nodes {
		ids = append(ids, n.NodeID[:])
		boxes = append(boxes, &coslv1.SealedBox{To: n.NodeID[:], EphemeralPublicKey: make([]byte, 32), Nonce: make([]byte, 24), Ciphertext: make([]byte, 48)})
	}
	return ids, boxes
}

// rounds commits a Session's round traffic, alternating senders.
func (w *writer) rounds(sid []byte, n, size int) {
	for j := range n {
		w.commit(j%2, cosl.TypeTssSession, nil, &coslv1.TssSession{SessionId: sid, Data: fixedBytes(size, j)})
	}
}

func (w *writer) keygen(sid []byte, curve string, pub []byte, rounds, size int) {
	ids, boxes := w.participants()
	w.commit(0, cosl.TypeSessionStart, nil, &coslv1.SessionStart{SessionId: sid, Curve: curve, T: 2, N: 2, Participants: ids, NonceBoxes: boxes})
	w.rounds(sid, rounds, size)
	for i := range w.r.Nodes {
		w.commit(i, cosl.TypeSessionComplete, nil, &coslv1.SessionComplete{SessionId: sid, Curve: curve, PublicKey: pub})
	}
}

// signing commits one Sign Request through to both signers' completes.
func (w *writer) signing(i int) {
	conv := w.r.Conversation
	req := w.commit(0, cosl.TypeSignRequest, conv, &coslv1.SignRequest{
		Reason:          fmt.Sprintf("payment %d", i),
		DeadlineSeconds: 3600,
		Transactions:    []*coslv1.SignTransaction{{NetworkConfigId: seed("network", 0)}},
	})
	if w.err != nil {
		return
	}
	rows, err := w.store.Log(cosl.Query{Limit: 1})
	if err != nil || len(rows) != 1 || rows[0].Seq != req {
		w.err = fmt.Errorf("reading back sign.request %d: %v", req, err)
		return
	}
	reqID := rows[0].MsgID[:]
	for n := range w.r.Nodes {
		w.commit(n, cosl.TypeSignAccept, conv, &coslv1.SignAccept{SignRequestId: reqID})
	}
	sid := sessionID("sign", i)
	ids, boxes := w.participants()
	w.commit(0, cosl.TypeSessionStart, nil, &coslv1.SessionStart{SessionId: sid, Curve: "ecdsa", T: 2, N: 2, Participants: ids, NonceBoxes: boxes, SignRequestId: reqID})
	w.rounds(sid, signRounds, signRoundSize)
	for n := range w.r.Nodes {
		w.commit(n, cosl.TypeSessionComplete, nil, &coslv1.SessionComplete{SessionId: sid, Curve: "ecdsa", PublicKey: w.r.SignKey, Signatures: [][]byte{w.r.Signature}})
	}
}

// fixedBytes is n bytes that do not compress to nothing, as engine output
// would not.
func fixedBytes(n, salt int) []byte {
	out := make([]byte, 0, n+32)
	for i := 0; len(out) < n; i++ {
		s := sha256.Sum256(fmt.Appendf(nil, "%d/%d", salt, i))
		out = append(out, s[:]...)
	}
	return out[:n]
}
