package cli_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	signID   = strings.Repeat("a1", 32)
	nodeA    = strings.Repeat("11", 32)
	nodeB    = strings.Repeat("22", 32)
	nodeC    = strings.Repeat("33", 32)
	signList = `[{"seq":10,"msg_id":"` + signID + `","proposer":"` + nodeA + `","proposer_handle":"treasurer","status":"open","transactions":1,` +
		`"reason":"pay","accepted":["` + nodeA + `"],"needed":2,"refused":["` + nodeB + `"],"key_known":true,"stance":"unaddressed"}]`
	signDetail = `{"seq":10,"msg_id":"` + signID + `","proposer":"` + nodeA + `","proposer_handle":"treasurer","status":"open","transactions":1,` +
		`"reason":"pay","accepted":["` + nodeA + `","` + nodeC + `"],"needed":3,"refused":["` + nodeB + `"],"key_known":true,"stance":"refused",` +
		`"accepts":[{"node":"` + nodeA + `","handle":"treasurer","seq":10},{"node":"` + nodeC + `","seq":12}],` +
		`"refusals":[{"node":"` + nodeB + `","handle":"auditor","seq":11,"reason":"unknown payee"}],"details":[],"broadcast_receipts":[]}`
)

const filled = `{"transactions":[{"network":"sol","solana":{"feePayer":"F","recentBlockhash":"H"}}],` +
	`"notes":[{"index":0,"network":"sol","chain":"solana devnet","endpoint":"https://rpc.example","filled":["feePayer","recentBlockhash"],"warning":"recentBlockhash expires in about 60–90 seconds: propose promptly"}]}`

// signServe is a fake serve for the sign verbs. A refusal answers with
// refuseKind as its error, if set, or as queued.
type signServe struct {
	refuseKind string
	queued     bool

	mu      sync.Mutex
	refuses []string
	fills   []string
}

func newSignServe(t *testing.T, dir string, ss *signServe) *signServe {
	t.Helper()
	ensureNode(t, dir)
	l, err := net.Listen("unix", filepath.Join(dir, "serve.sock"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sign/list", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(signList)) })
	mux.HandleFunc("POST /v1/sign/inspect", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(signDetail)) })
	mux.HandleFunc("GET /v1/hooks/runs", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) })
	mux.HandleFunc("POST /v1/sign/fill", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ss.mu.Lock()
		ss.fills = append(ss.fills, string(b))
		ss.mu.Unlock()
		if strings.Contains(string(b), `"bitcoin"`) {
			w.Header().Set("X-DeFiOS-Error", "bitcoin-unfillable")
			http.Error(w, "transaction 0 (bitcoin mainnet): a bitcoin transaction cannot be filled", http.StatusUnprocessableEntity)
			return
		}
		w.Write([]byte(filled))
	})
	mux.HandleFunc("POST /v1/sign/refuse", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ss.mu.Lock()
		ss.refuses = append(ss.refuses, string(b))
		ss.mu.Unlock()
		switch {
		case ss.refuseKind != "":
			w.Header().Set("X-DeFiOS-Error", ss.refuseKind)
			http.Error(w, "serve: "+strings.ReplaceAll(ss.refuseKind, "-", " "), http.StatusConflict)
		case ss.queued:
			w.Write([]byte(`{"queued":true}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return ss
}

// sign refuse sends the reason, which it requires, and reports a queued
// refusal as a queued accept is reported. A refusal serve turns down is a
// usage error naming why.
func TestSignRefuse(t *testing.T) {
	dir := t.TempDir()
	ss := newSignServe(t, dir, &signServe{})
	if r := run(t, nil, "", "--dir", dir, "sign", "refuse", "a1"); r.code != 1 || !strings.Contains(r.stderr, "--reason") {
		t.Fatalf("no reason: exit %d, stderr %q", r.code, r.stderr)
	}
	r := run(t, nil, "", "--dir", dir, "sign", "refuse", "a1", "--reason", "unknown payee")
	if r.code != 0 || !strings.Contains(r.stdout, "refused a1") {
		t.Fatalf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if len(ss.refuses) != 1 || !strings.Contains(ss.refuses[0], `"msg_id":"a1"`) || !strings.Contains(ss.refuses[0], `"reason":"unknown payee"`) {
		t.Fatalf("refuse bodies = %v", ss.refuses)
	}

	ss.queued = true
	if r := run(t, nil, "", "--dir", dir, "sign", "refuse", "a1", "--reason", "no"); r.code != 5 || !strings.Contains(r.stdout, "queued") {
		t.Fatalf("queued: exit %d, stdout %q", r.code, r.stdout)
	}

	for _, kind := range []string{"already-refused", "already-accepted", "signer-set-full"} {
		ss.refuseKind = kind
		r := run(t, nil, "", "--dir", dir, "sign", "refuse", "a1", "--reason", "no")
		if r.code != 1 || !strings.Contains(r.stderr, strings.ReplaceAll(kind, "-", " ")) {
			t.Errorf("%s: exit %d, stderr %q; want a usage error", kind, r.code, r.stderr)
		}
	}
}

// list shows refusals next to accepts and this Node's stance; inspect lists
// every accept and refusal with its Node, Handle, reason and seq.
func TestSignListAndInspectShowStances(t *testing.T) {
	dir := t.TempDir()
	newSignServe(t, dir, &signServe{})
	r := run(t, nil, "", "--dir", dir, "sign", "list")
	if r.code != 0 || !strings.Contains(r.stdout, "1/2 accepted, 1 refused") || !strings.Contains(r.stdout, "unaddressed") {
		t.Fatalf("list: exit %d, stdout %q", r.code, r.stdout)
	}
	r = run(t, nil, "", "--dir", dir, "sign", "inspect", "a1")
	if r.code != 0 {
		t.Fatalf("inspect: exit %d, stderr %q", r.code, r.stderr)
	}
	for _, want := range []string{
		"proposer " + nodeA + "/@treasurer",
		"stance refused",
		"accepted 2/3\n",
		"refused 1\n",
		"  " + nodeA + "/@treasurer",
		"  " + nodeC + " ",
		"  " + nodeB + "/@auditor: unknown payee (seq 11)",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("inspect lacks %q:\n%s", want, r.stdout)
		}
	}
}

// The help tells refusing, a stance on the log, from declining, which aborts
// a started Session.
func TestSignRefuseAndDeclineHelp(t *testing.T) {
	r := run(t, nil, "", "sign", "refuse", "--help")
	if r.code != 0 || !strings.Contains(r.stdout, "sign.refuse") || !strings.Contains(r.stdout, "sign decline") || !strings.Contains(r.stdout, "--reason") {
		t.Errorf("refuse help:\n%s", r.stdout)
	}
	r = run(t, nil, "", "sign", "decline", "--help")
	if r.code != 0 || !strings.Contains(r.stdout, "sign refuse") {
		t.Errorf("decline help:\n%s", r.stdout)
	}
}

// sign fill sends the Key and the transactions read from stdin, prints the
// completed transactions alone to stdout, as sign propose reads them, and
// what was filled to stderr. Bitcoin, refused, is a usage error.
func TestSignFill(t *testing.T) {
	dir := t.TempDir()
	ss := newSignServe(t, dir, &signServe{})
	if r := run(t, nil, "", "--dir", dir, "sign", "fill", "--transactions", "-"); r.code != 1 || !strings.Contains(r.stderr, "--key") {
		t.Fatalf("no key: exit %d, stderr %q", r.code, r.stderr)
	}
	r := run(t, nil, `[{"network":"sol","solana":{}}]`, "--dir", dir, "sign", "fill", "--key", "eddsa:ab", "--transactions", "-")
	if r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	var txs []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &txs); err != nil || len(txs) != 1 || txs[0]["network"] != "sol" {
		t.Fatalf("stdout %q is not the transactions: %v", r.stdout, err)
	}
	if !strings.Contains(r.stderr, "transaction 0 (sol, solana devnet): filled feePayer, recentBlockhash from https://rpc.example") || !strings.Contains(r.stderr, "propose promptly") {
		t.Errorf("stderr %q", r.stderr)
	}
	if len(ss.fills) != 1 || !strings.Contains(ss.fills[0], `"key":"eddsa:ab"`) || !strings.Contains(ss.fills[0], `"network":"sol"`) {
		t.Fatalf("fill bodies = %v", ss.fills)
	}
	r = run(t, nil, `[{"network":"sol","solana":{}}]`, "--dir", dir, "--json", "sign", "fill", "--key", "eddsa:ab", "--transactions", "-")
	if r.code != 0 || !strings.Contains(r.stdout, `"notes"`) {
		t.Errorf("--json: exit %d, stdout %q", r.code, r.stdout)
	}
	r = run(t, nil, `[{"network":"btc","bitcoin":{}}]`, "--dir", dir, "sign", "fill", "--key", "ecdsa:ab", "--transactions", "-")
	if r.code != 1 || !strings.Contains(r.stderr, "cannot be filled") {
		t.Errorf("bitcoin: exit %d, stderr %q", r.code, r.stderr)
	}
}
