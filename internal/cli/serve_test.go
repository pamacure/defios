package cli_test

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// fakeServe answers the local API on dir/serve.sock with canned JSON, so
// the CLI's socket side is tested without a tailnet.
func fakeServe(t *testing.T, dir string, nodes string) {
	t.Helper()
	l, err := net.Listen("unix", filepath.Join(dir, "serve.sock"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"node_id":"aa","hostname":"defios-fake"}`))
	})
	mux.HandleFunc("/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(nodes))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
}

const twoNodes = `[
 {"node_id":"` + "1111111111111111111111111111111111111111111111111111111111111111" + `","display_name":"bob","hostname":"defios-bob","auth_public_key":"","encryption_public_key":""},
 {"node_id":"` + "2222222222222222222222222222222222222222222222222222222222222222" + `","display_name":"carol","hostname":"defios-carol","auth_public_key":"","encryption_public_key":""}
]`

func TestNodeListWithoutServeIsOperationalError(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	r := run(t, nil, "", "--dir", dir, "node", "list")
	if r.code != 2 || !strings.Contains(r.stderr, "serve is not running") {
		t.Fatalf("exit %d, stderr %q; want 2 and a 'serve is not running' message", r.code, r.stderr)
	}
}

func TestNodeListPrintsDiscoveredIdentities(t *testing.T) {
	dir := t.TempDir()
	fakeServe(t, dir, twoNodes)

	r := run(t, nil, "", "--dir", dir, "node", "list")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines:\n%s", r.stdout)
	}
	for _, want := range []string{"1111111111111111", "bob", "defios-bob"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line 1 missing %q: %s", want, lines[0])
		}
	}

	r = run(t, nil, "", "--dir", dir, "node", "list", "--json")
	var doc []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil || len(doc) != 2 {
		t.Fatalf("json: %v\n%s", err, r.stdout)
	}
	if doc[1]["display_name"] != "carol" || doc[1]["hostname"] != "defios-carol" {
		t.Fatalf("doc[1] = %v", doc[1])
	}
}

func TestNodeInspectReportsServeUp(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	if doc := inspectJSON(t, dir); doc["serve_up"] != false {
		t.Fatalf("serve_up before serve = %v", doc["serve_up"])
	}
	fakeServe(t, dir, "[]")
	if doc := inspectJSON(t, dir); doc["serve_up"] != true {
		t.Fatalf("serve_up with serve = %v", doc["serve_up"])
	}
	r := run(t, nil, "", "--dir", dir, "node", "inspect")
	if !strings.Contains(r.stdout, "Serve:                 up") {
		t.Fatalf("human output:\n%s", r.stdout)
	}
}

func TestServeRefusesWhenSocketIsBound(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir, "--headscale-url", "http://headscale.invalid")
	fakeServe(t, dir, "[]")
	r := run(t, withPass, "", "--dir", dir, "serve")
	if r.code != 2 || !strings.Contains(r.stderr, "already running") {
		t.Fatalf("exit %d, stderr %q; want 2 and 'already running'", r.code, r.stderr)
	}
}

// An unset headscale_url is a problem with this Node's config, not with
// the command line: operational, exit 2.
func TestServeWithoutHeadscaleURLIsOperationalError(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	r := run(t, withPass, "", "--dir", dir, "serve")
	if r.code != 2 || !strings.Contains(r.stderr, "headscale_url") {
		t.Fatalf("exit %d, stderr %q; want 2 mentioning headscale_url", r.code, r.stderr)
	}
}
