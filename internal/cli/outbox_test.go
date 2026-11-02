package cli_test

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/outbox"
	coslv1 "defios.com/proto/cosl/v1"
)

// queuedIDs are the hex msg_ids in the Outbox under dir, in queue order.
func queuedIDs(t *testing.T, dir string) []string {
	t.Helper()
	q, err := outbox.Read(filepath.Join(dir, "group", group.ImplicitAlias, "outbox.json"))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(q))
	for i, s := range q {
		out[i] = hex.EncodeToString(s.MsgID[:])
	}
	return out
}

// outbox list shows the queued messages in queue order, with no serve.
func TestOutboxListShowsQueuedInOrder(t *testing.T) {
	dir := t.TempDir()
	alice, _ := groupWithTalk(t, dir)
	queueTalk(t, dir, alice, "waiting")
	ids := queuedIDs(t, dir)

	r := run(t, nil, "", "--dir", dir, "outbox", "list")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if r.code != 0 || len(lines) != 2 {
		t.Fatalf("exit %d:\n%s%s", r.code, r.stdout, r.stderr)
	}
	for i, l := range lines {
		if !strings.HasPrefix(l, ids[i]+" ") || !strings.Contains(l, " app.freeform ") || !strings.HasSuffix(l, " waiting") {
			t.Errorf("line %d: %q", i, l)
		}
	}
	if !strings.Contains(lines[0], " group ") || !strings.Contains(lines[1], " "+strings.Repeat("0", 8)+" ") {
		t.Errorf("Conversations: %q", lines)
	}

	r = run(t, nil, "", "--dir", dir, "outbox", "list", "--json")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil || len(rows) != 2 {
		t.Fatalf("--json: %v\n%s", err, r.stdout)
	}
	if rows[0]["msg_id"] != ids[0] || rows[0]["type"] != "app.freeform" || rows[0]["conversation_id"] != nil || rows[0]["created_at"] == "" {
		t.Errorf("row 0 = %v", rows[0])
	}
	if rows[1]["conversation_id"] != strings.Repeat("0", 64) {
		t.Errorf("row 1 = %v", rows[1])
	}
	if _, ok := rows[0]["deadline"]; ok {
		t.Errorf("talk has a deadline: %v", rows[0])
	}
}

// A queued Sign Request is listed with its deadline, counted from its
// created_at.
func TestOutboxListShowsASignRequestsDeadline(t *testing.T) {
	dir := t.TempDir()
	alice, _ := groupWithTalk(t, dir)
	queueSigned(t, dir, alice, make([]byte, 32), cosl.TypeSignRequest, &coslv1.SignRequest{Reason: "pay", DeadlineSeconds: 600})

	r := run(t, nil, "", "--dir", dir, "outbox", "list", "--json")
	var rows []map[string]string
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("--json: %v\n%s%s", err, r.stdout, r.stderr)
	}
	created, err1 := time.Parse(time.RFC3339, rows[0]["created_at"])
	deadline, err2 := time.Parse(time.RFC3339, rows[0]["deadline"])
	if err1 != nil || err2 != nil || deadline.Sub(created) != 10*time.Minute || rows[0]["type"] != "sign.request" {
		t.Fatalf("row = %v, want a deadline 600s after created_at", rows[0])
	}
	r = run(t, nil, "", "--dir", dir, "outbox", "list")
	if !strings.Contains(r.stdout, " sign.request ") || !strings.Contains(r.stdout, " deadline "+rows[0]["deadline"]+" ") {
		t.Errorf("human: %q", r.stdout)
	}
}

// With nothing queued, outbox list prints nothing, and [] under --json.
func TestOutboxListEmpty(t *testing.T) {
	dir := t.TempDir()
	groupWithTalk(t, dir)
	if r := run(t, nil, "", "--dir", dir, "outbox", "list"); r.code != 0 || r.stdout != "" {
		t.Errorf("exit %d: %q %s", r.code, r.stdout, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "outbox", "list", "--json"); r.code != 0 || strings.TrimSpace(r.stdout) != "[]" {
		t.Errorf("--json: exit %d: %q %s", r.code, r.stdout, r.stderr)
	}
}

// With no serve, outbox drop erases exactly the message a prefix names.
// One on the log is refused as committed; a prefix that is not hex, or
// names more than one, is a usage error.
func TestOutboxDropWithoutServe(t *testing.T) {
	dir := t.TempDir()
	alice, _ := groupWithTalk(t, dir)
	queueTalk(t, dir, alice, "waiting")
	ids := queuedIDs(t, dir)

	r := run(t, nil, "", "--dir", dir, "outbox", "drop", ids[0][:12])
	if r.code != 0 || !strings.Contains(r.stdout, ids[0]) {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if got := queuedIDs(t, dir); len(got) != 1 || got[0] != ids[1] {
		t.Fatalf("left %v, want only %s", got, ids[1])
	}
	if r := run(t, nil, "", "--dir", dir, "outbox", "drop", ids[0]); r.code != 2 || !strings.Contains(r.stderr, "no queued message") {
		t.Errorf("drop again: exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "outbox", "drop", "xyz"); r.code != 1 {
		t.Errorf("drop xyz: exit %d: %s", r.code, r.stderr)
	}

	store, err := cosl.Open(filepath.Join(dir, "group", group.ImplicitAlias, "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.Log(cosl.Query{})
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	onLog := hex.EncodeToString(rows[0].MsgID[:])
	r = run(t, nil, "", "--dir", dir, "outbox", "drop", onLog[:9])
	if r.code != 2 || !strings.Contains(r.stderr, "committed at seq 1") {
		t.Errorf("drop of a committed message: exit %d: %s", r.code, r.stderr)
	}
	if got := queuedIDs(t, dir); len(got) != 1 {
		t.Fatalf("a refused drop changed the Outbox: %v", got)
	}
}

// fakeOutboxDrop answers /v1/outbox/drop on dir's socket with status and
// body, and returns the request body it saw. It holds the Outbox's lock, as
// a serve whose replica runs does.
func fakeOutboxDrop(t *testing.T, dir string, status int, kind, body string) *atomic.Value {
	t.Helper()
	unlock, err := outbox.Lock(filepath.Join(dir, "group", group.ImplicitAlias, "outbox.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	l, err := net.Listen("unix", filepath.Join(dir, "serve.sock"))
	if err != nil {
		t.Fatal(err)
	}
	seen := new(atomic.Value)
	seen.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/outbox/drop", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen.Store(string(b))
		if kind != "" {
			w.Header().Set("X-DeFiOS-Error", kind)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return seen
}

// While serve's replica holds the Outbox, outbox drop asks serve, since its
// Outbox is the one in memory, and leaves the file to it.
func TestOutboxDropWithServeAsksServe(t *testing.T) {
	dir := t.TempDir()
	alice, _ := groupWithTalk(t, dir)
	queueTalk(t, dir, alice, "waiting")
	ids := queuedIDs(t, dir)
	seen := fakeOutboxDrop(t, dir, http.StatusOK, "", `{"msg_id":"`+ids[1]+`"}`)

	r := run(t, nil, "", "--dir", dir, "outbox", "drop", ids[1][:8])
	if r.code != 0 || !strings.Contains(r.stdout, ids[1]) || !strings.Contains(seen.Load().(string), ids[1][:8]) {
		t.Fatalf("exit %d: %s%s; serve saw %q", r.code, r.stdout, r.stderr, seen.Load())
	}
	if got := queuedIDs(t, dir); len(got) != 2 {
		t.Fatalf("the CLI changed the file a serve owns: %v", got)
	}
}

// A serve that reports the message committed: the CLI says so.
func TestOutboxDropWithServeReportsCommitted(t *testing.T) {
	dir := t.TempDir()
	groupWithTalk(t, dir)
	fakeOutboxDrop(t, dir, http.StatusConflict, "committed", "outbox: message abababab committed at seq 7; it is on the log and cannot be dropped")
	r := run(t, nil, "", "--dir", dir, "outbox", "drop", "abab")
	if r.code != 2 || !strings.Contains(r.stderr, "committed at seq 7") {
		t.Errorf("exit %d: %s", r.code, r.stderr)
	}
}
