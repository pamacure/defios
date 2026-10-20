package serve_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/config"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/nodedir"
	"defios.com/internal/serve"
	groupv1 "defios.com/proto/group/v1"
)

// fakeNet is one in-process tailnet: every fakeTailnet on it registers
// its listeners as "<hostname>:<port>", so Raft and the forward gate can
// dial by hostname the way tsnet does.
type fakeNet struct {
	mu     sync.Mutex
	byName map[string]string // "defios-bob:4632" -> real addr
}

func newFakeNet() *fakeNet { return &fakeNet{byName: map[string]string{}} }

// fakeTailnet is one Node's view: loopback listeners, routes from fake
// tailnet IPs to them (formation HTTP), and the peers it is told about.
type fakeTailnet struct {
	net       *fakeNet
	hostname  string
	mu        sync.Mutex
	listeners map[string]net.Listener // ":4634" -> listener
	routes    map[string]string       // "100.64.0.2:4634" -> real addr
	peers     []serve.Peer
}

func newFakeTailnet(n *fakeNet, hostname string) *fakeTailnet {
	return &fakeTailnet{net: n, hostname: hostname, listeners: map[string]net.Listener{}, routes: map[string]string{}}
}

func (f *fakeTailnet) Up(context.Context) error { return nil }
func (f *fakeTailnet) Close() error             { return nil }

func (f *fakeTailnet) Listen(network, addr string) (net.Listener, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(addr)
	f.mu.Lock()
	f.listeners[addr] = l
	f.mu.Unlock()
	f.net.mu.Lock()
	f.net.byName[f.hostname+":"+port] = l.Addr().String()
	f.net.mu.Unlock()
	return l, nil
}

// Dial resolves "<hostname>:<port>" on the shared net, else a routed IP.
func (f *fakeTailnet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	f.net.mu.Lock()
	real, ok := f.net.byName[addr]
	f.net.mu.Unlock()
	if !ok {
		f.mu.Lock()
		real, ok = f.routes[addr]
		f.mu.Unlock()
	}
	if !ok {
		return nil, errors.New("fake tailnet: no route to " + addr)
	}
	return (&net.Dialer{}).DialContext(ctx, network, real)
}

// formationAddr is where this fake's :4634 listener really is.
func (f *fakeTailnet) formationAddr(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		l, ok := f.listeners[":4634"]
		f.mu.Unlock()
		if ok {
			return l.Addr().String()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("formation listener never bound")
	return ""
}

func (f *fakeTailnet) route(fakeIP, real string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[fakeIP+":4634"] = real
}

func (f *fakeTailnet) HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: f.Dial}}
}

func (f *fakeTailnet) Peers(context.Context) ([]serve.Peer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]serve.Peer(nil), f.peers...), nil
}

type node struct {
	id      *identity.Node
	paths   nodedir.Paths
	tailnet *fakeTailnet
	name    string
	stop    func() // stops the running serve; set by start
}

func newNode(t *testing.T, name string) *node { return newNodeOn(t, newFakeNet(), name) }

// newNodeOn mints a Node on a shared fake tailnet.
func newNodeOn(t *testing.T, n *fakeNet, name string) *node {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), name)
	return &node{id: id, paths: nodedir.Resolve(nodedir.Lookup{Dir: dir}), tailnet: newFakeTailnet(n, "defios-"+name), name: name}
}

// restart reuses this Node's dir and identity with a fresh fake tailnet
// view on the same net, as a new serve process would.
func (n *node) restart(t *testing.T) {
	t.Helper()
	n.stop()
	n.tailnet = newFakeTailnet(n.tailnet.net, n.tailnet.hostname)
	n.start(t)
}

func (n *node) options() serve.Options {
	return serve.Options{
		Paths:   n.paths,
		Node:    n.id,
		Config:  config.Config{DisplayName: n.name, Hostname: "defios-" + n.name},
		Tailnet: n.tailnet,
		Logf:    func(string, ...any) {},
	}
}

// start runs serve in the background and waits for the socket to answer.
// The returned func stops it and waits; the test's cleanup calls it too.
func (n *node) start(t *testing.T) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve.Run(ctx, n.options()) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("serve.Run returned %v", err)
			}
		})
	}
	client := serve.Client{Socket: n.paths.Socket}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := client.Status(ctx); err == nil && st.TailnetUp {
			t.Cleanup(stop)
			n.stop = stop
			return stop
		}
		select {
		case err := <-done:
			t.Fatalf("serve.Run exited early: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("serve never came up")
	return stop
}

func TestStatusOverSocketAndSocketMode(t *testing.T) {
	a := newNode(t, "alice")
	a.start(t)

	st, err := serve.Client{Socket: a.paths.Socket}.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.NodeID != a.id.NodeID.String() || st.Hostname != "defios-alice" || !st.TailnetUp {
		t.Fatalf("status = %+v", st)
	}
	info, err := os.Lstat(a.paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %o, want 0600", info.Mode().Perm())
	}
	if !serve.IsUp(a.paths.Socket) {
		t.Fatal("IsUp = false while serve runs")
	}
}

func TestSecondServeOnSameDirRefuses(t *testing.T) {
	a := newNode(t, "alice")
	a.start(t)
	err := serve.Run(context.Background(), a.options())
	if !errors.Is(err, serve.ErrAlreadyRunning) {
		t.Fatalf("second Run: err = %v, want ErrAlreadyRunning", err)
	}
}

func TestStaleSocketFileIsReplaced(t *testing.T) {
	a := newNode(t, "alice")
	if err := os.MkdirAll(filepath.Dir(a.paths.Socket), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", a.paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close() // leaves a dead socket file behind
	a.start(t)
}

func TestClientWithoutServeIsErrNotRunning(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "serve.sock")
	_, err := serve.Client{Socket: sock}.Status(context.Background())
	if !errors.Is(err, serve.ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
	if serve.IsUp(sock) {
		t.Fatal("IsUp = true with no socket")
	}
}

func TestIdentityEndpointServesSignedIdentity(t *testing.T) {
	a := newNode(t, "alice")
	a.start(t)
	resp, err := http.Get("http://" + a.tailnet.formationAddr(t) + "/v1/identity")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var id groupv1.Identity
	if err := proto.Unmarshal(raw, &id); err != nil {
		t.Fatal(err)
	}
	node, err := group.VerifyIdentity(&id)
	if err != nil {
		t.Fatal(err)
	}
	if node.NodeID != a.id.NodeID || node.DisplayName != "alice" || node.TsnetHostname != "defios-alice" {
		t.Fatalf("identity = %+v", node)
	}
}

func TestNodesDiscoversVerifiedPeersOnly(t *testing.T) {
	a, b := newNode(t, "alice"), newNode(t, "bob")
	a.start(t)
	b.start(t)

	// An imposter: a valid signature under its own key but claiming a's Node ID.
	imposter, _ := identity.Generate()
	bad, _ := group.SignIdentity(imposter, "imposter", "defios-imposter")
	bad.NodeId = a.id.NodeID[:]
	bad.Sig = nil
	unsigned, _ := proto.MarshalOptions{Deterministic: true}.Marshal(bad)
	bad.Sig = ed25519.Sign(ed25519.NewKeyFromSeed(imposter.AuthSeed), unsigned)
	badBytes, _ := proto.Marshal(bad)
	imposterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(badBytes)
	}))
	defer imposterSrv.Close()

	a.tailnet.route("100.64.0.2", b.tailnet.formationAddr(t))
	a.tailnet.route("100.64.0.3", imposterSrv.Listener.Addr().String())
	a.tailnet.peers = []serve.Peer{
		{Hostname: "defios-bob", IP: "100.64.0.2", Online: true},
		{Hostname: "defios-imposter", IP: "100.64.0.3", Online: true},
		{Hostname: "defios-gone", IP: "100.64.0.4", Online: false}, // no route
	}

	nodes, err := serve.Client{Socket: a.paths.Socket}.Nodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("nodes = %+v, want only bob", nodes)
	}
	if nodes[0].NodeID != b.id.NodeID.String() || nodes[0].DisplayName != "bob" || nodes[0].Hostname != "defios-bob" {
		t.Fatalf("node = %+v", nodes[0])
	}
}
