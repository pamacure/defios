package cli_test

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"defios.com/internal/group"
	"defios.com/internal/identity"
	groupv1 "defios.com/proto/group/v1"
)

// formGroup writes a Group record under dir/group directly through the
// Slot, as a serve would have, so list/inspect can be tested without one.
// valid=false leaves it pending (bob has not accepted).
func formGroup(t *testing.T, dir string, valid bool) (gid group.ID, alice, bob *identity.Node) {
	t.Helper()
	return formGroupAlias(t, dir, group.ImplicitAlias, valid)
}

// formGroupAlias writes a Group record under dir/group/<alias> the way a
// serve would, so multi-Group list/inspect can be tested without one. Each
// call mints fresh Nodes, so distinct aliases hold distinct Groups.
func formGroupAlias(t *testing.T, dir, alias string, valid bool) (gid group.ID, alice, bob *identity.Node) {
	t.Helper()
	alice, _ = identity.Generate()
	bob, _ = identity.Generate()
	ia, _ := group.SignIdentity(alice, "alice", "defios-alice")
	ib, _ := group.SignIdentity(bob, "bob", "defios-bob")
	slot, err := group.OpenSlot(filepath.Join(dir, "group"), alias, alice)
	if err != nil {
		t.Fatal(err)
	}
	p, err := slot.Propose([]*groupv1.Identity{ia, ib}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	prop, _ := group.VerifyProposal(p)
	if valid {
		acc, _ := group.SignAccept(bob, prop.GroupID)
		if _, err := slot.ReceiveAccept(acc); err != nil {
			t.Fatal(err)
		}
	}
	return prop.GroupID, alice, bob
}

func TestMultipleGroupsIsolatedAndListed(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	mustGroupNew(t, dir) // "default", becomes current
	if r := run(t, nil, "", "--dir", dir, "group", "new", "custody", "--headscale", "https://hs.example"); r.code != 0 {
		t.Fatalf("group new custody: exit %d, stderr %q", r.code, r.stderr)
	}
	// custody is now current (group new auto-focuses). Form a distinct Group
	// under each alias.
	gidDefault, _, _ := formGroupAlias(t, dir, group.ImplicitAlias, true)
	gidCustody, _, _ := formGroupAlias(t, dir, "custody", false)

	// list shows both Groups, custody marked current, with per-Group status.
	r := run(t, nil, "", "--dir", dir, "group", "list", "--json")
	if r.code != 0 {
		t.Fatalf("group list: exit %d, stderr %q", r.code, r.stderr)
	}
	var rows []groupRow
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil || len(rows) != 2 {
		t.Fatalf("group list json: %v\n%s", err, r.stdout)
	}
	byAlias := map[string]groupRow{}
	for _, row := range rows {
		byAlias[row.Alias] = row
	}
	if d := byAlias[group.ImplicitAlias]; d.Status != "valid" || d.GroupID != gidDefault.String() || d.Current {
		t.Fatalf("default row = %+v", d)
	}
	if c := byAlias["custody"]; c.Status != "pending" || c.GroupID != gidCustody.String() || !c.Current {
		t.Fatalf("custody row = %+v", c)
	}

	// Reads default to the Current Group (custody).
	if doc := inspectJSON(t, dir); doc["group_id"] != gidCustody.String() {
		t.Fatalf("inspect current=custody: group_id = %v, want %s", doc["group_id"], gidCustody)
	}
	// Switching focus switches which Group reads see; the other is untouched.
	if r := run(t, nil, "", "--dir", dir, "group", "use", group.ImplicitAlias); r.code != 0 {
		t.Fatalf("group use default: exit %d, stderr %q", r.code, r.stderr)
	}
	if doc := inspectJSON(t, dir); doc["group_id"] != gidDefault.String() {
		t.Fatalf("inspect current=default: group_id = %v, want %s", doc["group_id"], gidDefault)
	}
	// A non-current Group is still inspectable by its Group ID (#27).
	r = run(t, nil, "", "--dir", dir, "group", "inspect", gidCustody.String(), "--json")
	if r.code != 0 {
		t.Fatalf("inspect custody by id: exit %d, stderr %q", r.code, r.stderr)
	}
	var gv map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &gv); err != nil || gv["group_id"] != gidCustody.String() {
		t.Fatalf("inspect custody by id: %v, group_id = %v", err, gv["group_id"])
	}
}

// TestGroupFlagTargetsNonCurrentGroup checks that --group reads a Group by
// alias regardless of which is current, that omitting it falls back to the
// Current Group, and that an unknown alias errors clearly (#28).
func TestGroupFlagTargetsNonCurrentGroup(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	mustGroupNew(t, dir) // "default", current
	if r := run(t, nil, "", "--dir", dir, "group", "new", "custody", "--headscale", "https://hs.example"); r.code != 0 {
		t.Fatalf("group new custody: exit %d, stderr %q", r.code, r.stderr)
	}
	gidDefault, _, _ := formGroupAlias(t, dir, group.ImplicitAlias, true)
	gidCustody, _, _ := formGroupAlias(t, dir, "custody", true)
	// custody is current (group new auto-focused it); make default current so
	// the --group read genuinely targets a non-current Group.
	if r := run(t, nil, "", "--dir", dir, "group", "use", group.ImplicitAlias); r.code != 0 {
		t.Fatalf("group use default: exit %d, stderr %q", r.code, r.stderr)
	}

	// group inspect --group reads the named Group, not the current one.
	r := run(t, nil, "", "--dir", dir, "group", "inspect", "--group", "custody", "--json")
	if r.code != 0 {
		t.Fatalf("inspect --group custody: exit %d, stderr %q", r.code, r.stderr)
	}
	var gv map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &gv); err != nil || gv["group_id"] != gidCustody.String() {
		t.Fatalf("inspect --group custody: %v, group_id = %v, want %s", err, gv["group_id"], gidCustody)
	}
	// Omitting --group falls back to the Current Group (default).
	if doc := inspectJSON(t, dir); doc["group_id"] != gidDefault.String() {
		t.Fatalf("inspect without --group: group_id = %v, want %s", doc["group_id"], gidDefault)
	}

	// An unknown alias errors clearly, naming the Groups this Node holds.
	r = run(t, nil, "", "--dir", dir, "group", "inspect", "--group", "nope")
	if r.code == 0 || !strings.Contains(r.stderr, `no Group "nope"`) || !strings.Contains(r.stderr, "custody") {
		t.Fatalf("unknown alias: exit %d, stderr %q", r.code, r.stderr)
	}
	// A Group ID arg and --group together is a usage error.
	r = run(t, nil, "", "--dir", dir, "group", "inspect", gidCustody.String(), "--group", "custody")
	if r.code == 0 || !strings.Contains(r.stderr, "not both") {
		t.Fatalf("id + --group: exit %d, stderr %q", r.code, r.stderr)
	}
}

// groupRow mirrors one `group list --json` row for tests.
type groupRow struct {
	Alias   string `json:"alias"`
	Status  string `json:"status"`
	GroupID string `json:"group_id"`
	Current bool   `json:"current"`
}

func TestGroupListShowsUnformedScaffold(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	mustGroupNew(t, dir) // scaffolded, not yet formed
	r := run(t, nil, "", "--dir", dir, "group", "list", "--json")
	var rows []groupRow
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("group list json: %v\n%s", err, r.stdout)
	}
	if rows[0].Status != "unformed" || rows[0].GroupID != "" || !rows[0].Current {
		t.Fatalf("row = %+v", rows[0])
	}
}

func TestGroupListWithoutGroupIsEmpty(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	r := run(t, nil, "", "--dir", dir, "group", "list")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "" {
		t.Fatalf("exit %d, stdout %q", r.code, r.stdout)
	}
	r = run(t, nil, "", "--dir", dir, "group", "list", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "[]" {
		t.Fatalf("exit %d, stdout %q", r.code, r.stdout)
	}
}

func TestGroupListShowsStatusWithoutServe(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	gid, _, _ := formGroup(t, dir, false)
	r := run(t, nil, "", "--dir", dir, "group", "list")
	if r.code != 0 || !strings.Contains(r.stdout, group.ImplicitAlias+"  pending  "+gid.String()) {
		t.Fatalf("exit %d, stdout %q", r.code, r.stdout)
	}

	dir2 := t.TempDir()
	mustInit(t, dir2)
	gid2, _, _ := formGroup(t, dir2, true)
	r = run(t, nil, "", "--dir", dir2, "group", "list", "--json")
	var doc []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil || len(doc) != 1 {
		t.Fatalf("json: %v\n%s", err, r.stdout)
	}
	if doc[0]["alias"] != group.ImplicitAlias || doc[0]["group_id"] != gid2.String() || doc[0]["status"] != "valid" {
		t.Fatalf("doc = %v", doc[0])
	}
}

func TestGroupInspectPrintsRosterWithoutServe(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	gid, alice, bob := formGroup(t, dir, true)

	r := run(t, nil, "", "--dir", dir, "group", "inspect")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{gid.String(), "valid", alice.NodeID.String(), bob.NodeID.String(), "alice", "bob", "defios-alice", "defios-bob"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("missing %q in:\n%s", want, r.stdout)
		}
	}

	r = run(t, nil, "", "--dir", dir, "group", "inspect", gid.String(), "--json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var doc struct {
		GroupID string `json:"group_id"`
		Status  string `json:"status"`
		Roster  []struct {
			NodeID              string `json:"node_id"`
			DisplayName         string `json:"display_name"`
			Hostname            string `json:"hostname"`
			Reachable           bool   `json:"reachable"`
			Joined              bool   `json:"joined"`
			AuthPublicKey       string `json:"auth_public_key"`
			EncryptionPublicKey string `json:"encryption_public_key"`
		} `json:"roster"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, r.stdout)
	}
	if doc.GroupID != gid.String() || doc.Status != "valid" || len(doc.Roster) != 2 {
		t.Fatalf("doc = %+v", doc)
	}
	// Sorted by Node ID, nobody reachable (no serve), nobody joined yet.
	if doc.Roster[0].NodeID > doc.Roster[1].NodeID {
		t.Fatal("roster not sorted")
	}
	for _, n := range doc.Roster {
		if n.Reachable || n.Joined || len(n.AuthPublicKey) != 64 || len(n.EncryptionPublicKey) != 64 {
			t.Fatalf("row = %+v", n)
		}
	}
}

func TestGroupInspectErrors(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	r := run(t, nil, "", "--dir", dir, "group", "inspect")
	if r.code != 2 || !strings.Contains(r.stderr, "no Group") {
		t.Fatalf("no group: exit %d, %q", r.code, r.stderr)
	}
	formGroup(t, dir, true)
	// A Group ID this Node does not hold is ErrUnknownGroup, naming the ID
	// asked for (#27: any held Group is inspectable by ID, not just current).
	other := group.GroupID([]identity.ID{{1}})
	r = run(t, nil, "", "--dir", dir, "group", "inspect", other.String())
	if r.code != 2 || !strings.Contains(r.stderr, "not this Node's Group") || !strings.Contains(r.stderr, other.String()) {
		t.Fatalf("other group: exit %d, %q", r.code, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "group", "inspect", "zz")
	if r.code != 1 {
		t.Fatalf("bad hex: exit %d, %q", r.code, r.stderr)
	}
}

// A Record whose stored proposal no longer verifies is a verify failure.
func TestGroupInspectTamperedRecordIsVerifyFailure(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	formGroup(t, dir, true)
	path := filepath.Join(dir, "group", group.ImplicitAlias, "roster.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version  int      `json:"version"`
		Proposal []byte   `json:"proposal"`
		Accepts  [][]byte `json:"accepts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc.Proposal[len(doc.Proposal)-1] ^= 1 // last byte is inside the initiator's sig
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	r := run(t, nil, "", "--dir", dir, "group", "inspect")
	if r.code != 4 || !strings.Contains(r.stderr, "does not verify") {
		t.Fatalf("exit %d, stderr %q; want 4 and 'does not verify'", r.code, r.stderr)
	}
}

func TestNodeInspectShowsGroupID(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	gid, _, _ := formGroup(t, dir, false)
	if doc := inspectJSON(t, dir); doc["group_id"] != gid.String() {
		t.Fatalf("group_id = %v", doc["group_id"])
	}
}

func TestGroupCreateUsageAndServeErrors(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	self := inspectJSON(t, dir)["node_id"].(string)
	other := identity.ID{7}.String()

	cases := []struct {
		name string
		args []string
		code int
		msg  string
	}{
		{"no args", nil, 1, ""},
		{"bad hex", []string{self, "nothex"}, 1, "Node ID"},
		{"self missing", []string{other, identity.ID{8}.String()}, 1, "this Node"},
		{"only self", []string{self}, 1, "exactly one other"},
		{"three nodes", []string{self, other, identity.ID{8}.String()}, 1, "exactly one other"},
		{"no serve", []string{self, other}, 2, "serve is not running"},
	}
	for _, c := range cases {
		r := run(t, nil, "", append([]string{"--dir", dir, "group", "create"}, c.args...)...)
		if r.code != c.code || !strings.Contains(r.stderr, c.msg) {
			t.Errorf("%s: exit %d, stderr %q; want %d containing %q", c.name, r.code, r.stderr, c.code, c.msg)
		}
	}
	r := run(t, nil, "", "--dir", dir, "group", "accept", "zz")
	if r.code != 1 {
		t.Errorf("accept bad hex: exit %d", r.code)
	}
	r = run(t, nil, "", "--dir", dir, "group", "accept", other)
	if r.code != 2 || !strings.Contains(r.stderr, "serve is not running") {
		t.Errorf("accept without serve: exit %d, %q", r.code, r.stderr)
	}
}

// fakeGroupServe answers the local formation API with a canned status and
// body, recording the last request path.
func fakeGroupServe(t *testing.T, dir string, status int, kind, body string) *atomic.Value {
	t.Helper()
	return fakeLocalServe(t, dir, "/v1/group/", status, kind, body)
}

// fakeLocalServe answers every local API path under prefix with a canned
// status, error kind, and body, recording the last request path.
func fakeLocalServe(t *testing.T, dir, prefix string, status int, kind, body string) *atomic.Value {
	t.Helper()
	l, err := net.Listen("unix", filepath.Join(dir, "serve.sock"))
	if err != nil {
		t.Fatal(err)
	}
	last := new(atomic.Value)
	last.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		last.Store(r.URL.Path)
		if kind != "" {
			w.Header().Set("X-DeFiOS-Error", kind)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return last
}

func TestGroupCreateAcceptDeclineThroughServe(t *testing.T) {
	gid := group.GroupID([]identity.ID{{1}, {2}})
	dir := t.TempDir()
	mustInit(t, dir)
	self := inspectJSON(t, dir)["node_id"].(string)

	last := fakeGroupServe(t, dir, 200, "", `{"group_id":"`+gid.String()+`","status":"pending"}`)
	r := run(t, nil, "", "--dir", dir, "group", "create", self, identity.ID{7}.String())
	if r.code != 0 || last.Load() != "/v1/group/create" || !strings.Contains(r.stdout, gid.String()) || !strings.Contains(r.stdout, "pending") {
		t.Fatalf("create: exit %d, path %s, stdout %q, stderr %q", r.code, last.Load(), r.stdout, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "group", "create", "--json", self, identity.ID{7}.String())
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil || doc["status"] != "pending" {
		t.Fatalf("create --json: %v %s", err, r.stdout)
	}

	dir = t.TempDir()
	mustInit(t, dir)
	last = fakeGroupServe(t, dir, 200, "", `{"group_id":"`+gid.String()+`","status":"valid"}`)
	r = run(t, nil, "", "--dir", dir, "group", "accept", gid.String())
	if r.code != 0 || last.Load() != "/v1/group/accept" || !strings.Contains(r.stdout, "valid") {
		t.Fatalf("accept: exit %d, path %s, stdout %q", r.code, last.Load(), r.stdout)
	}

	dir = t.TempDir()
	mustInit(t, dir)
	last = fakeGroupServe(t, dir, 200, "", `{"group_id":"`+gid.String()+`","status":"declined"}`)
	r = run(t, nil, "", "--dir", dir, "group", "decline", gid.String(), "--reason", "nope")
	if r.code != 0 || last.Load() != "/v1/group/decline" || !strings.Contains(r.stdout, "declined") {
		t.Fatalf("decline: exit %d, path %s, stdout %q", r.code, last.Load(), r.stdout)
	}
}

func TestGroupVerbExitCodesFromServe(t *testing.T) {
	gid := group.GroupID([]identity.ID{{1}, {2}})
	cases := []struct {
		name   string
		status int
		kind   string
		body   string
		code   int
	}{
		{"node refused", 422, "rejected", "serve: node 1234 refused (409): already has a Group", 3},
		{"one group", 409, "one-group", "group: this Node already has a pending or valid Group", 2},
		{"no group", 404, "no-group", "group: this Node has no Group", 2},
		{"unreachable", 502, "", "node abcd is not reachable on the tailnet", 2},
		{"tailnet down", 503, "tailnet-down", "serve: tailnet is still coming up", 2},
		{"undelivered", 200, "", `{"group_id":"` + gid.String() + `","status":"pending","undelivered":["` + identity.ID{9}.String() + `"]}`, 2},
	}
	for _, c := range cases {
		dir := t.TempDir()
		mustInit(t, dir)
		fakeGroupServe(t, dir, c.status, c.kind, c.body)
		r := run(t, nil, "", "--dir", dir, "group", "accept", gid.String())
		if r.code != c.code {
			t.Errorf("%s: exit %d, stderr %q; want %d", c.name, r.code, r.stderr, c.code)
		}
	}
}
