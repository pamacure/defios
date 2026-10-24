package cosl_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
)

// fixture is a 2-Node Roster with the private keys to author envelopes.
type fixture struct {
	t      *testing.T
	nodes  []*identity.Node
	roster group.Roster
	store  *cosl.Store
	fsm    *cosl.FSM
	path   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	var members []group.RosterNode
	var nodes []*identity.Node
	for _, name := range []string{"alice", "bob"} {
		n, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, n)
		members = append(members, group.RosterNode{Public: n.Public, DisplayName: name, TsnetHostname: "defios-" + name})
	}
	roster := group.NewRoster(members...)
	path := filepath.Join(t.TempDir(), "cosl.sqlite")
	store, err := cosl.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return &fixture{t: t, nodes: nodes, roster: roster, store: store, fsm: cosl.NewFSM(store, roster), path: path}
}

func (f *fixture) priv(i int) ed25519.PrivateKey { return ed25519.NewKeyFromSeed(f.nodes[i].AuthSeed) }

// envelope builds an unsigned, well-formed envelope from node i.
func (f *fixture) envelope(i int, msgType uint32, payload proto.Message) *coslv1.Envelope {
	f.t.Helper()
	raw, err := proto.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	gid := f.roster.GroupID()
	msgID := make([]byte, 32)
	rand.Read(msgID)
	return &coslv1.Envelope{
		Version:   1,
		GroupId:   gid[:],
		MsgType:   msgType,
		Sender:    f.nodes[i].NodeID[:],
		MsgId:     msgID,
		CreatedAt: 1700000000_000000000,
		Payload:   raw,
	}
}

func (f *fixture) signed(i int, env *coslv1.Envelope) []byte {
	f.t.Helper()
	b, err := cosl.Sign(env, f.priv(i))
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func freeform(text string) *coslv1.AppFreeform { return &coslv1.AppFreeform{Text: text} }

func (f *fixture) apply(index, term uint64, data []byte) cosl.Result {
	f.t.Helper()
	res, err := f.fsm.Apply(index, term, data)
	if err != nil {
		f.t.Fatalf("Apply(%d,%d): %v", index, term, err)
	}
	return res
}

func (f *fixture) rows() []cosl.Row {
	f.t.Helper()
	rows, err := f.store.Log(cosl.Query{})
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

func TestApplyInsertsVerifiedEnvelopeAndChainsPrevHash(t *testing.T) {
	f := newFixture(t)
	e1 := f.signed(0, f.envelope(0, cosl.TypeAppFreeform, freeform("hi")))
	e2 := f.signed(1, f.envelope(1, cosl.TypeAppFreeform, freeform("hello")))

	if res := f.apply(5, 1, e1); !res.Inserted {
		t.Fatalf("first apply not inserted: %+v", res)
	}
	if res := f.apply(7, 2, e2); !res.Inserted { // raft index gap is fine
		t.Fatalf("second apply not inserted: %+v", res)
	}

	rows := f.rows()
	if len(rows) != 2 || rows[0].Seq != 5 || rows[1].Seq != 7 || rows[0].Term != 1 || rows[1].Term != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if !bytes.Equal(rows[0].Envelope, e1) || !bytes.Equal(rows[1].Envelope, e2) {
		t.Fatal("stored envelope bytes differ from what was applied")
	}

	// Independent chain: genesis 32 zero bytes, then
	// SHA-256(prev || seq_be64 || term_be64 || envelope).
	prev := make([]byte, 32)
	for _, r := range rows {
		h := sha256.New()
		h.Write(prev)
		h.Write(binary.BigEndian.AppendUint64(nil, r.Seq))
		h.Write(binary.BigEndian.AppendUint64(nil, r.Term))
		h.Write(r.Envelope)
		prev = h.Sum(nil)
		if !bytes.Equal(r.PrevHash[:], prev) {
			t.Fatalf("seq %d prev_hash = %x, want %x", r.Seq, r.PrevHash, prev)
		}
	}

	meta, err := f.store.Meta()
	if err != nil {
		t.Fatal(err)
	}
	if meta.LastAppliedSeq != 7 || meta.LastAppliedTerm != 2 || !bytes.Equal(meta.ChainHead[:], prev) {
		t.Fatalf("meta = %+v, want seq 7 term 2 head %x", meta, prev)
	}
}

func TestRowDenormalizesEnvelopeFields(t *testing.T) {
	f := newFixture(t)
	env := f.envelope(1, cosl.TypeAppFreeform, freeform("scoped"))
	conv := bytes.Repeat([]byte{0xcc}, 32)
	env.ConversationId = conv
	f.apply(1, 1, f.signed(1, env))

	r := f.rows()[0]
	if r.Sender != f.nodes[1].NodeID || r.MsgType != cosl.TypeAppFreeform || !bytes.Equal(r.MsgID[:], env.MsgId) || r.CreatedAt != env.CreatedAt {
		t.Fatalf("row = %+v", r)
	}
	if !bytes.Equal(r.ConversationID, conv) {
		t.Fatalf("conversation_id = %x, want %x", r.ConversationID, conv)
	}
	plain := f.envelope(0, cosl.TypeAppFreeform, freeform("group-level"))
	f.apply(2, 1, f.signed(0, plain))
	if r := f.rows()[1]; r.ConversationID != nil {
		t.Fatalf("Group-level conversation_id = %x, want nil", r.ConversationID)
	}
}

// TestAuthVerificationNegatives is docs/spec/mvp.md §11.3.
func TestAuthVerificationNegatives(t *testing.T) {
	f := newFixture(t)
	stranger, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		data   func() []byte
		reason string
	}{
		{"bad sig", func() []byte {
			env := f.envelope(0, cosl.TypeAppFreeform, freeform("x"))
			f.signed(0, env)
			env.Sig[0] ^= 0xff
			b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(env)
			return b
		}, "signature"},
		{"sender not on Roster", func() []byte {
			env := f.envelope(0, cosl.TypeAppFreeform, freeform("x"))
			env.Sender = stranger.NodeID[:]
			b, _ := cosl.Sign(env, ed25519.NewKeyFromSeed(stranger.AuthSeed))
			return b
		}, "roster"},
		{"group_id mismatch", func() []byte {
			env := f.envelope(0, cosl.TypeAppFreeform, freeform("x"))
			env.GroupId = bytes.Repeat([]byte{0xee}, 32)
			return f.signed(0, env)
		}, "group"},
		{"unknown version", func() []byte {
			env := f.envelope(0, cosl.TypeAppFreeform, freeform("x"))
			env.Version = 2
			return f.signed(0, env)
		}, "version"},
		{"truncated envelope", func() []byte {
			b := f.signed(0, f.envelope(0, cosl.TypeAppFreeform, freeform("x")))
			return b[:len(b)/2]
		}, "unmarshal"},
	}
	var index uint64
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index++
			before := len(f.rows())
			res := f.apply(index, 1, tc.data())
			if res.Inserted {
				t.Fatalf("%s: inserted", tc.name)
			}
			if !strings.Contains(strings.ToLower(res.Reason), tc.reason) {
				t.Fatalf("%s: reason %q does not mention %q", tc.name, res.Reason, tc.reason)
			}
			if after := len(f.rows()); after != before {
				t.Fatalf("%s: row count %d -> %d", tc.name, before, after)
			}
		})
	}

	t.Run("unknown msg_type with valid signature is inserted", func(t *testing.T) {
		index++
		env := f.envelope(1, 42, freeform("future kind"))
		if res := f.apply(index, 1, f.signed(1, env)); !res.Inserted {
			t.Fatalf("not inserted: %+v", res)
		}
	})

	// The registry rule: a reader that predates a type stores its rows and
	// skips what it cannot decode. sign.accept (11) arrived after the others.
	t.Run("an older reader skips sign.accept", func(t *testing.T) {
		cosl.WithoutType(t, cosl.TypeSignAccept)
		index++
		payload := &coslv1.SignAccept{SignRequestId: make([]byte, 32)}
		env := f.envelope(1, cosl.TypeSignAccept, payload)
		if res := f.apply(index, 1, f.signed(1, env)); !res.Inserted {
			t.Fatalf("not inserted: %+v", res)
		}
		raw, _ := proto.Marshal(payload)
		if _, err := cosl.DecodePayload(cosl.TypeSignAccept, raw); !errors.Is(err, cosl.ErrUnknownType) {
			t.Fatalf("decode = %v, want ErrUnknownType", err)
		}
		if got := cosl.Summary(cosl.TypeSignAccept, raw); got != "34 bytes" {
			t.Fatalf("summary %q", got)
		}
		if got := cosl.TypeName(cosl.TypeSignAccept); got != "unknown(11)" {
			t.Fatalf("name %q", got)
		}
	})

	t.Run("duplicate msg_id yields no second row", func(t *testing.T) {
		data := f.signed(0, f.envelope(0, cosl.TypeAppFreeform, freeform("once")))
		index++
		if res := f.apply(index, 1, data); !res.Inserted {
			t.Fatalf("first copy not inserted: %+v", res)
		}
		before := len(f.rows())
		index++
		res := f.apply(index, 1, data)
		if res.Inserted {
			t.Fatal("duplicate inserted")
		}
		if !strings.Contains(res.Reason, "duplicate") {
			t.Fatalf("reason %q", res.Reason)
		}
		if after := len(f.rows()); after != before {
			t.Fatalf("row count %d -> %d", before, after)
		}
		meta, _ := f.store.Meta()
		if meta.LastAppliedSeq != index {
			t.Fatalf("last_applied_seq = %d, want %d (skips still advance)", meta.LastAppliedSeq, index)
		}
	})
}

func TestSchemaMatchesSpecAndIsAppendOnly(t *testing.T) {
	f := newFixture(t)
	f.apply(1, 1, f.signed(0, f.envelope(0, cosl.TypeAppFreeform, freeform("row"))))

	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}

	objects := map[string]string{}
	rows, err := db.Query("SELECT type, name FROM sqlite_master")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var typ, name string
		rows.Scan(&typ, &name)
		objects[name] = typ
	}
	rows.Close()
	for name, typ := range map[string]string{
		"cosl": "table", "cosl_meta": "table",
		"idx_cosl_msg_id": "index", "idx_cosl_conversation": "index", "idx_cosl_type": "index",
		"cosl_no_update": "trigger", "cosl_no_delete": "trigger",
	} {
		if objects[name] != typ {
			t.Errorf("%s: got %q, want %s", name, objects[name], typ)
		}
	}

	if _, err := db.Exec("UPDATE cosl SET term = 99"); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("UPDATE: err = %v, want append-only abort", err)
	}
	if _, err := db.Exec("DELETE FROM cosl"); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("DELETE: err = %v, want append-only abort", err)
	}
	var n int
	db.QueryRow("SELECT count(*) FROM cosl").Scan(&n)
	if n != 1 {
		t.Fatalf("row count after refused UPDATE/DELETE = %d", n)
	}
}

func TestLogFiltersByTypeAndLimitsToLatest(t *testing.T) {
	f := newFixture(t)
	f.apply(1, 1, f.signed(0, f.envelope(0, cosl.TypeAppFreeform, freeform("a"))))
	f.apply(2, 1, f.signed(0, f.envelope(0, cosl.TypeMembershipJoin, &coslv1.MembershipJoin{DisplayName: "alice"})))
	f.apply(3, 1, f.signed(1, f.envelope(1, cosl.TypeAppFreeform, freeform("b"))))
	f.apply(4, 1, f.signed(1, f.envelope(1, cosl.TypeAppFreeform, freeform("c"))))

	typ := cosl.TypeAppFreeform
	rows, err := f.store.Log(cosl.Query{MsgType: &typ})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Seq != 1 || rows[2].Seq != 4 {
		t.Fatalf("type filter rows = %+v", rows)
	}
	rows, err = f.store.Log(cosl.Query{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Seq != 3 || rows[1].Seq != 4 {
		t.Fatalf("limit rows should be the latest 2 in seq order, got %+v", rows)
	}
}

func TestByMsgIDPrefix(t *testing.T) {
	f := newFixture(t)
	env := f.envelope(0, cosl.TypeAppFreeform, freeform("find me"))
	f.apply(1, 1, f.signed(0, env))
	f.apply(2, 1, f.signed(0, f.envelope(0, cosl.TypeAppFreeform, freeform("other"))))

	r, err := f.store.ByMsgIDPrefix(env.MsgId[:4])
	if err != nil || !bytes.Equal(r.MsgID[:], env.MsgId) {
		t.Fatalf("prefix lookup: %+v, %v", r, err)
	}
	if _, err := f.store.ByMsgIDPrefix([]byte{0xff, 0xff, 0xff, 0xff, 0xff}); err != cosl.ErrNotFound {
		t.Fatalf("missing: err = %v, want ErrNotFound", err)
	}
	if _, err := f.store.ByMsgIDPrefix(nil); err != cosl.ErrAmbiguous {
		t.Fatalf("empty prefix matching all rows: err = %v, want ErrAmbiguous", err)
	}
}
