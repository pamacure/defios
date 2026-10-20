//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"tailscale.com/envknob"

	"defios.com/internal/cli"
	"defios.com/internal/serve"
)

const (
	headscaleImage = "headscale/headscale:v0.29.5"
	passphrase     = "e2e-passphrase"
)

// headscale is a stock control server in Docker: MagicDNS on, embedded
// DERP, no ACL policy (allow all).
type headscale struct {
	container testcontainers.Container
	url       string
}

func startHeadscale(t *testing.T) *headscale {
	t.Helper()
	ctx := context.Background()

	// Headscale's embedded DERP is reached at server_url, which is plain
	// http here; tailscale dials region DERP over TLS unless told not to.
	envknob.SetenvForTest(t, "TS_DEBUG_USE_DERP_HTTP", "1")

	httpPort, stunPort := freePort(t, "tcp"), freePort(t, "udp")
	url := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	cfg := fmt.Sprintf(`
server_url: %s
listen_addr: 0.0.0.0:8080
metrics_listen_addr: 127.0.0.1:9090
grpc_listen_addr: 127.0.0.1:50443
grpc_allow_insecure: false
noise:
  private_key_path: /var/lib/headscale/noise_private.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
  allocation: sequential
derp:
  server:
    enabled: true
    region_id: 999
    region_code: "headscale"
    region_name: "Headscale Embedded DERP"
    verify_clients: true
    stun_listen_addr: "0.0.0.0:%d"
    private_key_path: /var/lib/headscale/derp_server_private.key
    automatically_add_embedded_derp_region: true
    ipv4: 127.0.0.1
  urls: []
  paths: []
  auto_update_enabled: false
  update_frequency: 24h
disable_check_updates: true
database:
  type: sqlite
  sqlite:
    path: /var/lib/headscale/db.sqlite
log:
  level: info
  format: text
policy:
  mode: file
  path: ""
dns:
  magic_dns: true
  base_domain: defios.test
  override_local_dns: false
  nameservers:
    global:
      - 1.1.1.1
  search_domains: []
unix_socket: /var/run/headscale/headscale.sock
unix_socket_permission: "0770"
logtail:
  enabled: false
`, url, stunPort)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	req := testcontainers.ContainerRequest{
		Image:        headscaleImage,
		Cmd:          []string{"serve"},
		ExposedPorts: []string{"8080/tcp", fmt.Sprintf("%d/udp", stunPort)},
		Files: []testcontainers.ContainerFile{{
			HostFilePath:      cfgPath,
			ContainerFilePath: "/etc/headscale/config.yaml",
			FileMode:          0o644,
		}},
		HostConfigModifier: func(hc *container.HostConfig) {
			loopback := netip.MustParseAddr("127.0.0.1")
			hc.PortBindings = network.PortMap{
				network.MustParsePort("8080/tcp"):                      {{HostIP: loopback, HostPort: fmt.Sprint(httpPort)}},
				network.MustParsePort(fmt.Sprintf("%d/udp", stunPort)): {{HostIP: loopback, HostPort: fmt.Sprint(stunPort)}},
			}
		},
		WaitingFor: wait.ForHTTP("/health").WithPort("8080/tcp").WithStartupTimeout(90 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
	if err != nil {
		t.Fatalf("starting headscale: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			if logs, err := c.Logs(context.Background()); err == nil {
				b, _ := io.ReadAll(logs)
				t.Logf("headscale logs:\n%s", b)
			}
		}
		c.Terminate(context.Background())
	})
	return &headscale{container: c, url: url}
}

// exec runs a headscale CLI command inside the container and returns stdout.
func (h *headscale) exec(t *testing.T, args ...string) string {
	t.Helper()
	code, out, err := h.container.Exec(context.Background(), append([]string{"headscale"}, args...))
	if err != nil {
		t.Fatalf("headscale %v: %v", args, err)
	}
	raw, _ := io.ReadAll(out)
	// Exec output is docker-multiplexed; strip the 8-byte frame headers.
	text := demux(raw)
	if code != 0 {
		t.Fatalf("headscale %v: exit %d\n%s", args, code, text)
	}
	return text
}

// preauthKey creates a user and a reusable preauth key for it.
func (h *headscale) preauthKey(t *testing.T, user string) string {
	t.Helper()
	h.exec(t, "users", "create", user)
	var users []struct {
		ID   uint64 `json:"id"`
		Name string `json:"name"`
	}
	mustJSON(t, h.exec(t, "users", "list", "-o", "json"), &users)
	var id uint64
	for _, u := range users {
		if u.Name == user {
			id = u.ID
		}
	}
	if id == 0 {
		t.Fatalf("user %q not in %+v", user, users)
	}
	var key struct {
		Key string `json:"key"`
	}
	mustJSON(t, h.exec(t, "preauthkeys", "create", "--user", fmt.Sprint(id), "--reusable", "--expiration", "1h", "-o", "json"), &key)
	if key.Key == "" {
		t.Fatal("empty preauth key")
	}
	return key.Key
}

// demux strips Docker's stream multiplexing headers from exec output.
func demux(raw []byte) string {
	var out bytes.Buffer
	for len(raw) >= 8 {
		n := int(raw[4])<<24 | int(raw[5])<<16 | int(raw[6])<<8 | int(raw[7])
		raw = raw[8:]
		if n > len(raw) {
			n = len(raw)
		}
		out.Write(raw[:n])
		raw = raw[n:]
	}
	out.Write(raw)
	return out.String()
}

func mustJSON(t *testing.T, s string, v any) {
	t.Helper()
	// headscale may print a line of text before JSON; find the first bracket.
	i := strings.IndexAny(s, "[{")
	if i < 0 {
		t.Fatalf("no JSON in %q", s)
	}
	if err := json.Unmarshal([]byte(s[i:]), v); err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
}

func freePort(t *testing.T, network string) int {
	t.Helper()
	switch network {
	case "tcp":
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	default:
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).Port
	}
}

// node is one in-process DeFiOS Node driven through the CLI's entry point.
type node struct {
	t    *testing.T
	name string
	dir  string
	env  map[string]string
	sock string
}

func newNode(t *testing.T, hs *headscale, name string) *node {
	t.Helper()
	n := &node{
		t:    t,
		name: name,
		dir:  filepath.Join(t.TempDir(), name),
		env: map[string]string{
			"DEFIOS_PASSPHRASE": passphrase,
			"TS_AUTHKEY":        hs.preauthKey(t, name),
		},
	}
	n.sock = filepath.Join(n.dir, "serve.sock")
	r := n.run(context.Background(), "init", "--name", name, "--hostname", "defios-"+name, "--headscale-url", hs.url)
	if r.code != 0 {
		t.Fatalf("%s init: exit %d: %s", name, r.code, r.stderr)
	}
	return n
}

type result struct {
	code           int
	stdout, stderr string
}

// run invokes a verb exactly as the binary would, with this Node's --dir.
func (n *node) run(ctx context.Context, args ...string) result {
	var out, errb bytes.Buffer
	app := cli.App{
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: io.MultiWriter(&errb, testWriter{n.t, n.name}),
		Getenv: func(k string) string { return n.env[k] },
		Home:   n.dir,
	}
	code := app.Run(ctx, append([]string{"--dir", n.dir}, args...))
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

// startServe runs `serve` in the background until the test ends and waits
// for the tailnet to be up, so a stuck join fails fast instead of at the
// test timeout.
func (n *node) startServe(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan result, 1)
	go func() { done <- n.run(ctx, "serve") }()
	t.Cleanup(func() {
		cancel()
		select {
		case r := <-done:
			if r.code != 0 {
				t.Errorf("%s serve exited %d: %s", n.name, r.code, r.stderr)
			}
		case <-time.After(30 * time.Second):
			t.Errorf("%s serve did not stop", n.name)
		}
	})
	client := serve.Client{Socket: n.sock}
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := client.Status(ctx); err == nil && st.TailnetUp {
			return
		}
		select {
		case r := <-done:
			t.Fatalf("%s serve exited early (%d): %s", n.name, r.code, r.stderr)
		default:
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s serve never reported the tailnet up", n.name)
}

// inspect returns `node inspect --json`.
func (n *node) inspect(t *testing.T) (view struct {
	NodeID  string `json:"node_id"`
	ServeUp bool   `json:"serve_up"`
}) {
	t.Helper()
	r := n.run(context.Background(), "node", "inspect", "--json")
	if r.code != 0 {
		t.Fatalf("%s node inspect: exit %d: %s", n.name, r.code, r.stderr)
	}
	if err := json.Unmarshal([]byte(r.stdout), &view); err != nil {
		t.Fatalf("%s node inspect: %v\n%s", n.name, err, r.stdout)
	}
	return view
}

// nodeList polls `node list --json` until the predicate holds.
func (n *node) nodeList(t *testing.T, until func([]serve.NodeSummary) bool) []serve.NodeSummary {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	var last []serve.NodeSummary
	for time.Now().Before(deadline) {
		r := n.run(context.Background(), "node", "list", "--json")
		if r.code == 0 {
			last = nil
			if err := json.Unmarshal([]byte(r.stdout), &last); err == nil && until(last) {
				return last
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s node list never satisfied the predicate; last = %+v", n.name, last)
	return nil
}

// testWriter forwards serve's stderr to the test log, one line at a time.
type testWriter struct {
	t    *testing.T
	name string
}

var logMu sync.Mutex

func (w testWriter) Write(p []byte) (int, error) {
	logMu.Lock()
	defer logMu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		w.t.Logf("[%s] %s", w.name, line)
	}
	return len(p), nil
}
