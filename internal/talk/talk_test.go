package talk_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/nodedir"
	"defios.com/internal/outbox"
	"defios.com/internal/recap"
	"defios.com/internal/talk"
	coslv1 "defios.com/proto/cosl/v1"
)

// fixture is a replica in a temp SQLite store, written by alice and bob,
// and the messages of alice's this Node's replica may not hold: mine.
type fixture struct {
	t          *testing.T
	paths      nodedir.Paths
	store      *cosl.Store
	fsm        *cosl.FSM
	roster     group.Roster
	alice, bob cosl.Author
	seq        uint64
	mine       []outbox.Entry
	// mineRead is called when Mine is, to see what the store held then.
	mineRead func()
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	a, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	b, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	roster := group.NewRoster(
		group.RosterNode{Public: a.Public, DisplayName: "alice", TsnetHostname: "defios-alice"},
		group.RosterNode{Public: b.Public, DisplayName: "bob", TsnetHostname: "defios-bob"},
	)
	dir := t.TempDir()
	store, err := cosl.Open(filepath.Join(dir, "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return &fixture{
		t:      t,
		paths:  nodedir.Paths{Config: dir, Data: dir},
		store:  store,
		fsm:    cosl.NewFSM(store, roster),
		roster: roster,
		alice:  cosl.Author{Node: a, GroupID: roster.GroupID()},
		bob:    cosl.Author{Node: b, GroupID: roster.GroupID()},
	}
}

func (f *fixture) reader() talk.Reader {
	return talk.Reader{
		Paths: f.paths,
		Alias: "home",
		Store: f.store,
		Mine: func() ([]outbox.Entry, error) {
			if f.mineRead != nil {
				f.mineRead()
			}
			return f.mine, nil
		},
		Roster: &f.roster,
	}
}

func (f *fixture) read(conv []byte) talk.View {
	f.t.Helper()
	v, err := f.reader().Read(conv)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

func (f *fixture) sign(by cosl.Author, conv []byte, payload proto.Message) cosl.Signed {
	f.t.Helper()
	s, err := by.Sign(cosl.TypeAppFreeform, conv, payload)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func said(text string) *coslv1.AppFreeform {
	return &coslv1.AppFreeform{Text: text, ContentType: "text/plain"}
}

// commit applies s at the next seq.
func (f *fixture) commit(s cosl.Signed) cosl.Signed {
	f.t.Helper()
	f.seq++
	if res, err := f.fsm.Apply(f.seq, 1, s.Bytes); err != nil || !res.Inserted {
		f.t.Fatalf("apply %d: %v %+v", f.seq, err, res)
	}
	return s
}

// say commits by's text in Group-level talk.
func (f *fixture) say(by cosl.Author, text string) cosl.Signed {
	return f.commit(f.sign(by, nil, said(text)))
}

// texts is each message's text, or [pruned] or [unreadable], then "#<seq>"
// or "#queued".
func texts(ms talk.Messages) string {
	var out []string
	for _, m := range ms {
		text := m.Text
		switch {
		case m.Pruned:
			text = "[pruned]"
		case m.Unreadable:
			text = "[unreadable]"
		}
		at := fmt.Sprint("#", m.Row.Seq)
		if m.Row.Queued() {
			at = "#queued"
		}
		out = append(out, text+at)
	}
	return strings.Join(out, " ")
}

// This Node's view is its committed talk, then what it wrote that its
// replica does not hold: committed past what is applied, by seq, then the
// queued, in queue order. A message both in the replica and from the
// Outbox shows once, as committed.
func TestAMessageInBothShowsOnceAsCommitted(t *testing.T) {
	f := newFixture(t)
	one := f.say(f.alice, "one")
	f.say(f.bob, "two")
	three := f.sign(f.alice, nil, said("three"))
	waiting := f.sign(f.alice, nil, said("waiting"))
	f.mine = []outbox.Entry{{Signed: one, Seq: 1}, {Signed: three, Seq: 3}, {Signed: waiting}}
	if got := texts(f.read(nil).Messages); got != "one#1 two#2 three#3 waiting#queued" {
		t.Fatalf("view = %s", got)
	}
}

// The Outbox is read before the replica: a message the serve commits
// between the two reads is in both, and shows once.
func TestTheOutboxIsReadBeforeTheReplica(t *testing.T) {
	f := newFixture(t)
	s := f.sign(f.alice, nil, said("draining"))
	f.mine = []outbox.Entry{{Signed: s}}
	f.mineRead = func() { f.commit(s) }
	if got := texts(f.read(nil).Messages); got != "draining#1" {
		t.Fatalf("view = %s", got)
	}
}

// The view of a Conversation holds only its own talk: Group-level talk and
// another Conversation's, and what is not talk, are not in it.
func TestTheViewIsOneConversations(t *testing.T) {
	f := newFixture(t)
	conv := make([]byte, 32)
	conv[0] = 1
	f.say(f.alice, "group-level")
	f.commit(f.sign(f.bob, conv, said("committed here")))
	abort, err := f.alice.Sign(cosl.TypeSessionAbort, conv, &coslv1.SessionAbort{Reason: "noise"})
	if err != nil {
		t.Fatal(err)
	}
	f.mine = []outbox.Entry{{Signed: f.sign(f.alice, nil, said("queued group-level"))}, {Signed: f.sign(f.alice, conv, said("here"))}, {Signed: abort}}
	if got := texts(f.read(conv).Messages); got != "committed here#2 here#queued" {
		t.Fatalf("view = %s", got)
	}
}

// A payload that does not parse is a message marked unreadable, not an
// error: the FSM does not parse payloads, and one member's malformed
// message must not stop the talk being read.
func TestAnUnreadablePayloadIsMarkedNotAnError(t *testing.T) {
	f := newFixture(t)
	f.say(f.alice, "before")
	f.commit(f.sign(f.bob, nil, &wrapperspb.BytesValue{Value: []byte{0xff}}))
	f.say(f.alice, "after")
	v := f.read(nil)
	if got := texts(v.Messages); got != "before#1 [unreadable]#2 after#3" {
		t.Fatalf("view = %s", got)
	}
	if got := texts(lean(t, v).Messages); got != "before#1 after#3" {
		t.Errorf("lean = %s", got)
	}
	if got := texts(v.Chat()); got != "before#1 after#3" {
		t.Errorf("chat = %s", got)
	}
}

// lean is v's lean view, which must read.
func lean(t *testing.T, v talk.View) talk.Lean {
	t.Helper()
	l, err := v.Lean()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// writeRecap is this Node's RECAP.md of Group-level talk up to base.
func (f *fixture) writeRecap(base uint64, text string) *recap.Recap {
	f.t.Helper()
	r := recap.Recap{BaseSeq: base, Text: text}
	if err := recap.Write(f.paths.RecapFile("home", nodedir.ConversationName(nil)), r); err != nil {
		f.t.Fatal(err)
	}
	return &r
}

// The lean view is this Node's Recap, then the readable talk after its base
// seq; talk it accounts for is not read, pruned or not. Queued talk, which
// has no seq, is after it.
func TestTheRecapCutsTheLeanView(t *testing.T) {
	f := newFixture(t)
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		f.say(f.alice, s)
	}
	f.mine = []outbox.Entry{{Signed: f.sign(f.alice, nil, said("queued"))}}
	if _, err := f.store.Prune(nil, 1); err != nil {
		t.Fatal(err)
	}
	want := f.writeRecap(3, "a to c")
	l := lean(t, f.read(nil))
	if l.Recap == nil || *l.Recap != *want || l.Missing || texts(l.Messages) != "d#4 e#5 queued#queued" {
		t.Fatalf("lean = %+v: %s", l, texts(l.Messages))
	}
}

// Pruned talk the Recap does not account for, or any pruned talk with no
// Recap, is missing: the lean view says so, and has nothing to show for it.
func TestPrunedTalkNoRecapAccountsForIsMissing(t *testing.T) {
	f := newFixture(t)
	for _, s := range []string{"a", "b", "c", "d"} {
		f.say(f.alice, s)
	}
	if _, err := f.store.Prune(nil, 3); err != nil {
		t.Fatal(err)
	}
	l := lean(t, f.read(nil))
	if !l.Missing || l.Recap != nil || texts(l.Messages) != "d#4" {
		t.Errorf("no Recap: lean = %+v: %s", l, texts(l.Messages))
	}
	f.writeRecap(1, "a")
	if l := lean(t, f.read(nil)); !l.Missing || texts(l.Messages) != "d#4" {
		t.Errorf("a Recap before it: lean = %+v: %s", l, texts(l.Messages))
	}
	f.writeRecap(3, "a to c")
	if l := lean(t, f.read(nil)); l.Missing || texts(l.Messages) != "d#4" {
		t.Errorf("a Recap over it: lean = %+v: %s", l, texts(l.Messages))
	}
}

// The chat view is every readable message this replica holds from the
// first, whatever the Recap; pruned talk is absent. The full view still
// holds the pruned rows.
func TestChatStartsAtTheFirstMessage(t *testing.T) {
	f := newFixture(t)
	for _, s := range []string{"a", "b", "c", "d"} {
		f.say(f.alice, s)
	}
	f.mine = []outbox.Entry{{Signed: f.sign(f.alice, nil, said("queued"))}}
	f.writeRecap(3, "a to c")
	if _, err := f.store.Prune(nil, 1); err != nil {
		t.Fatal(err)
	}
	v := f.read(nil)
	if got := texts(v.Chat()); got != "b#2 c#3 d#4 queued#queued" {
		t.Errorf("chat = %s", got)
	}
	if got := texts(v.Messages); got != "[pruned]#1 b#2 c#3 d#4 queued#queued" {
		t.Errorf("messages = %s", got)
	}
	if got := texts(v.Messages.Last(2)); got != "d#4 queued#queued" {
		t.Errorf("last 2 = %s", got)
	}
}

// Each message names its sender as the Roster does, and an agent's Handle
// and depth; with no Roster the name is left to the caller.
func TestNamesComeFromTheRoster(t *testing.T) {
	f := newFixture(t)
	f.say(f.alice, "hi")
	f.commit(f.sign(f.bob, nil, &coslv1.AppFreeform{Text: "ok", ContentType: "text/markdown", Handle: "auditor", Depth: 1}))
	v := f.read(nil)
	a, b := v.Messages[0], v.Messages[1]
	if a.Sender != "alice" || a.Handle != "" || a.Depth != 0 || a.ContentType != "text/plain" {
		t.Errorf("alice's = %+v", a)
	}
	if b.Sender != "bob" || b.Handle != "auditor" || b.Depth != 1 || b.ContentType != "text/markdown" || b.Row.Sender != f.bob.Node.NodeID {
		t.Errorf("bob's agent's = %+v", b)
	}
	r := f.reader()
	r.Roster = nil
	v, err := r.Read(nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Messages[0].Sender != "" {
		t.Errorf("no Roster: sender %q", v.Messages[0].Sender)
	}
}

// A lean line names a person by display name and an agent by display name
// and Handle, and keeps to one line.
func TestLeanLine(t *testing.T) {
	if got := talk.LeanLine("alice", "", "hi\nthere"); got != `alice: hi\nthere` {
		t.Errorf("person: %q", got)
	}
	if got := talk.LeanLine("alice", "auditor", "ok"); got != "alice/@auditor: ok" {
		t.Errorf("agent: %q", got)
	}
}

// A RECAP.md that does not parse fails only what reads the Recap, the lean
// view, naming the file; the talk and the chat read as ever.
func TestAMalformedRecapFailsOnlyTheLeanView(t *testing.T) {
	f := newFixture(t)
	f.say(f.alice, "a")
	path := f.paths.RecapFile("home", nodedir.ConversationName(nil))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a recap"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := f.read(nil)
	if got := texts(v.Chat()); got != "a#1" {
		t.Errorf("chat = %s", got)
	}
	if _, err := v.Lean(); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("lean: %v, want an error naming %s", err, path)
	}
}
