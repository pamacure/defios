package cli_test

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// providerServe answers /v1/provider/* on dir/serve.sock, recording each
// request body by path.
func providerServe(t *testing.T, dir string) func() []string {
	t.Helper()
	l, err := net.Listen("unix", filepath.Join(dir, "serve.sock"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/provider/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+string(raw))
		mu.Unlock()
		w.Write([]byte(`{"seq":7,"term":1,"msg_id":"ab"}`))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// provider add writes a complete row: an alias that names a preset uses
// it, --preset names one for any alias, and flags override its fields.
// Without a preset both fields are required, and an api_path no shape
// matches is refused before anything is written.
func TestProviderAddWritesACompleteRow(t *testing.T) {
	dir := t.TempDir()
	seen := providerServe(t, dir)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"anthropic"}, `{"alias":"anthropic","api_path":"/v1/messages","base_url":"https://api.anthropic.com"}`},
		{[]string{"local", "--base-url", "http://127.0.0.1:8080", "--api-path", "/v1/chat/completions"},
			`{"alias":"local","api_path":"/v1/chat/completions","base_url":"http://127.0.0.1:8080"}`},
		{[]string{"openai"}, `{"alias":"openai","api_path":"/v1/responses","base_url":"https://api.openai.com"}`},
		{[]string{"openai-cc", "--preset", "openai", "--api-path", "/v1/chat/completions"},
			`{"alias":"openai-cc","api_path":"/v1/chat/completions","base_url":"https://api.openai.com"}`},
		{[]string{"work", "--preset", "anthropic"}, `{"alias":"work","api_path":"/v1/messages","base_url":"https://api.anthropic.com"}`},
		{[]string{"anthropic", "--base-url", "https://proxy.example"}, `{"alias":"anthropic","api_path":"/v1/messages","base_url":"https://proxy.example"}`},
	} {
		before := len(seen())
		r := run(t, nil, "", append([]string{"--dir", dir, "provider", "add"}, c.args...)...)
		if r.code != 0 || !strings.Contains(r.stdout, "seq 7") {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q", c.args, r.code, r.stdout, r.stderr)
		}
		if got := seen(); len(got) != before+1 || got[before] != "/v1/provider/add "+c.want {
			t.Fatalf("%v: sent %v, want %s", c.args, got[before:], c.want)
		}
	}
	before := len(seen())
	for _, args := range [][]string{
		{"x", "--base-url", "https://x.example", "--api-path", "/v1/foo"},
		{"x", "--api-path", "/v1/foo"},
		{"x"},
		{"x", "--base-url", "https://x.example"},
		{"x", "--preset", "acme"},
		{"anthropic", "--api-path", "/v1/decisions"},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "provider", "add"}, args...)...)
		if r.code != 1 {
			t.Errorf("%v: exit %d, stderr %q; want 1", args, r.code, r.stderr)
		}
	}
	if got := seen(); len(got) != before {
		t.Fatalf("refused adds were sent: %v", got[before:])
	}
}

// provider remove writes a remove of the alias.
func TestProviderRemove(t *testing.T) {
	dir := t.TempDir()
	seen := providerServe(t, dir)
	if r := run(t, nil, "", "--dir", dir, "provider", "remove", "local"); r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if got := seen(); len(got) != 1 || got[0] != `/v1/provider/remove {"alias":"local"}` {
		t.Fatalf("sent %v", got)
	}
}

// The model command is gone.
func TestThereIsNoModelCommand(t *testing.T) {
	if r := run(t, nil, "", "model", "list"); r.code == 0 {
		t.Fatalf("model list exit 0: %s", r.stdout)
	}
}
