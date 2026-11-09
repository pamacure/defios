package cli_test

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/session"
	coslv1 "defios.com/proto/cosl/v1"
)

func TestSessionStartRequiresServeAndACurve(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	r := run(t, nil, "", "--dir", dir, "session", "start", "--curve", "ecdsa")
	if r.code != 2 || !strings.Contains(r.stderr, "serve") {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "session", "start")
	if r.code != 1 {
		t.Fatalf("missing curve: exit %d, stderr %q", r.code, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "session", "start", "--curve", "rsa")
	if r.code != 1 {
		t.Fatalf("bad curve: exit %d, stderr %q", r.code, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "session", "start", "--curve", "ecdsa", "--t", "0")
	if r.code != 1 {
		t.Fatalf("t=0: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestSessionListAndInspectReadTheReplicaWithoutServe(t *testing.T) {
	dir := t.TempDir()
	alice, bob, aborted, openID := seedSessions(t, dir)

	r := run(t, nil, "", "--dir", dir, "session", "list")
	if r.code != 0 {
		t.Fatalf("list exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	if lines[0] != aborted+"  aborted  ecdsa  t=2 n=2" {
		t.Errorf("line 0 = %q", lines[0])
	}
	if lines[1] != openID+"  open  eddsa  t=2 n=2" {
		t.Errorf("line 1 = %q", lines[1])
	}

	r = run(t, nil, "", "--dir", dir, "session", "inspect", openID)
	if r.code != 0 {
		t.Fatalf("inspect exit %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "status open") || !strings.Contains(r.stdout, "  "+alice.NodeID.String()) || !strings.Contains(r.stdout, "  "+bob.NodeID.String()+" eddsa:02aa") {
		t.Fatalf("inspect:\n%s", r.stdout)
	}

	r = run(t, nil, "", "--dir", dir, "session", "inspect", aborted, "--json")
	if r.code != 0 {
		t.Fatalf("json inspect exit %d: %s", r.code, r.stderr)
	}
	var detail session.Detail
	if err := json.Unmarshal([]byte(r.stdout), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Status != session.Aborted || detail.Reason != "operator stop" || detail.Curve != "ecdsa" || len(detail.Completes) != 0 {
		t.Fatalf("detail = %+v", detail)
	}

	r = run(t, nil, "", "--dir", dir, "session", "inspect", strings.Repeat("ab", 32))
	if r.code != 2 {
		t.Fatalf("unknown session: exit %d, stderr %q", r.code, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "session", "inspect", "nope")
	if r.code != 1 {
		t.Fatalf("bad id: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestSessionListWithoutReplicaIsOperational(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	r := run(t, nil, "", "--dir", dir, "session", "list")
	if r.code != 2 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "session", "accept", strings.Repeat("aa", 32))
	if r.code != 2 {
		t.Fatalf("accept without serve: exit %d, stderr %q", r.code, r.stderr)
	}
}

// seedSessions writes two Sessions into dir's replica: one aborted, one
// open with a single complete. Returns the Nodes and the Session IDs.
func seedSessions(t *testing.T, dir string) (alice, bob *identity.Node, aborted, open string) {
	t.Helper()
	var err error
	alice, err = identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	bob, err = identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	roster := group.NewRoster(
		group.RosterNode{Public: alice.Public, DisplayName: "alice", TsnetHostname: "defios-alice"},
		group.RosterNode{Public: bob.Public, DisplayName: "bob", TsnetHostname: "defios-bob"},
	)
	gid := roster.GroupID()
	path := filepath.Join(dir, "group", group.ImplicitAlias, "cosl.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := cosl.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fsm := cosl.NewFSM(store, roster)
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
	parts := [][]byte{roster.NodeIDs()[0][:], roster.NodeIDs()[1][:]}
	apply(alice, cosl.TypeSessionStart, &coslv1.SessionStart{SessionId: aID[:], Curve: "ecdsa", T: 2, N: 2, Participants: parts})
	apply(bob, cosl.TypeSessionAbort, &coslv1.SessionAbort{SessionId: aID[:], Reason: "operator stop"})
	apply(alice, cosl.TypeSessionStart, &coslv1.SessionStart{SessionId: bID[:], Curve: "eddsa", T: 2, N: 2, Participants: parts})
	apply(bob, cosl.TypeSessionComplete, &coslv1.SessionComplete{SessionId: bID[:], Curve: "eddsa", PublicKey: []byte{0x02, 0xaa}})
	return alice, bob, aID.String(), bID.String()
}

// seedProposals is a replica holding one open keygen Session per alias, in
// order, and returns their IDs.
func seedProposals(t *testing.T, dir string, aliases ...string) []string {
	t.Helper()
	alice, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	roster := group.NewRoster(group.RosterNode{Public: alice.Public, DisplayName: "alice", TsnetHostname: "defios-alice"})
	path := filepath.Join(dir, "group", group.ImplicitAlias, "cosl.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := cosl.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fsm := cosl.NewFSM(store, roster)
	ids := make([]string, len(aliases))
	for i, alias := range aliases {
		id := session.ID{byte(i + 1)}
		ids[i] = id.String()
		signed, err := (cosl.Author{Node: alice, GroupID: roster.GroupID()}).Sign(cosl.TypeSessionStart, nil, &coslv1.SessionStart{
			SessionId: id[:], Curve: "ecdsa", T: 1, N: 1, Participants: [][]byte{alice.NodeID[:]}, ProposedAlias: []byte(alias),
		})
		if err != nil {
			t.Fatal(err)
		}
		if res, err := fsm.Apply(uint64(i+1), 1, signed.Bytes); err != nil || !res.Inserted {
			t.Fatalf("apply: %v %+v", err, res)
		}
	}
	return ids
}

// session list and inspect show a valid proposal; one that is invalid, or
// none at all, leaves no trace in text or --json.
func TestSessionListAndInspectShowOnlyAValidProposal(t *testing.T) {
	dir := t.TempDir()
	ids := seedProposals(t, dir, "Cold", "\x1b[31mevil", "")

	r := run(t, nil, "", "--dir", dir, "session", "list")
	if r.code != 0 {
		t.Fatalf("list exit %d: %s", r.code, r.stderr)
	}
	want := []string{
		ids[0] + "  open  ecdsa  t=1 n=1  proposed_alias=Cold",
		ids[1] + "  open  ecdsa  t=1 n=1",
		ids[2] + "  open  ecdsa  t=1 n=1",
	}
	if got := strings.Split(strings.TrimSpace(r.stdout), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("list:\n%s", r.stdout)
	}

	r = run(t, nil, "", "--dir", dir, "session", "list", "--json")
	var raw []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &raw); err != nil || len(raw) != 3 {
		t.Fatalf("list --json %q: %v", r.stdout, err)
	}
	if raw[0]["proposed_alias"] != "Cold" {
		t.Errorf("proposal = %v", raw[0])
	}
	for _, row := range raw[1:] {
		if _, ok := row["proposed_alias"]; ok {
			t.Errorf("field present: %v", row)
		}
	}

	r = run(t, nil, "", "--dir", dir, "session", "inspect", ids[0])
	if r.code != 0 || !strings.Contains(r.stdout, "\nproposed_alias Cold\n") {
		t.Fatalf("inspect exit %d:\n%s", r.code, r.stdout)
	}
	r = run(t, nil, "", "--dir", dir, "session", "inspect", ids[1])
	if r.code != 0 || strings.Contains(r.stdout, "proposed_alias") || strings.Contains(r.stdout, "evil") {
		t.Fatalf("inspect of invalid:\n%s", r.stdout)
	}
	r = run(t, nil, "", "--dir", dir, "session", "inspect", ids[1], "--json")
	if r.code != 0 || strings.Contains(r.stdout, "proposed_alias") || strings.Contains(r.stdout, "evil") {
		t.Fatalf("inspect --json of invalid:\n%s", r.stdout)
	}
}

// An alias that can never be valid is a usage error before serve is asked,
// even when it is empty.
func TestSessionStartRefusesAnInvalidAliasAsUsage(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	for _, bad := range []string{"", " spaced", strings.Repeat("x", 64)} {
		r := run(t, nil, "", "--dir", dir, "session", "start", "--curve", "ecdsa", "--alias", bad)
		if r.code != 1 || !strings.Contains(r.stderr, "key alias") {
			t.Errorf("%q: exit %d, stderr %q", bad, r.code, r.stderr)
		}
	}
	// A valid one reaches for serve, which is not running.
	r := run(t, nil, "", "--dir", dir, "session", "start", "--curve", "ecdsa", "--alias", "cold")
	if r.code != 2 || !strings.Contains(r.stderr, "serve") {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
}
