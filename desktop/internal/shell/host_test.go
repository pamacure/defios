package shell_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"defios.com/desktop/internal/prefs"
	"defios.com/desktop/internal/shell"
	"defios.com/internal/cli"
	"defios.com/internal/node"
	"defios.com/internal/nodedir"
	"defios.com/internal/secret"
	"defios.com/internal/serve"
)

// The Shell over a real Node, as the app runs it.

func defiosCLI(t *testing.T, dir string, args ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	env := map[string]string{"DEFIOS_PASSPHRASE": pw, "DEFIOS_ECDSA_PREPARAMS": "skip"}
	app := cli.App{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, Getenv: func(k string) string { return env[k] }, Home: t.TempDir()}
	if code := app.Run(context.Background(), append([]string{"--dir", dir}, args...)); code != 0 {
		t.Fatalf("defios %v: exit %d\n%s", args, code, errb.String())
	}
	return out.String()
}

type loopTailnet struct{}

func (loopTailnet) Up(context.Context) error { return nil }
func (loopTailnet) Close() error             { return nil }
func (loopTailnet) Listen(string, string) (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}
func (loopTailnet) Dial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("loop tailnet: no peers")
}
func (loopTailnet) HTTPClient() *http.Client                    { return http.DefaultClient }
func (loopTailnet) Peers(context.Context) ([]serve.Peer, error) { return nil, nil }

func hostShell(t *testing.T, p nodedir.Paths) *shell.Shell {
	return meshShell(t, p, func(serve.TsnetOptions) serve.Tailnet { return loopTailnet{} })
}

// meshShell is an app over the Node at p, on the tailnet newTailnet joins.
func meshShell(t *testing.T, p nodedir.Paths, newTailnet func(serve.TsnetOptions) serve.Tailnet) *shell.Shell {
	t.Helper()
	s := shell.New(shell.Deps{
		Create: func(c node.Creation) error {
			c.Preparams = func(func(string)) ([]byte, error) { return nil, nil }
			_, err := node.Create(p, c)
			return err
		},
		Open: func() (shell.Node, error) {
			h, err := node.Open(p, node.Options{
				NewTailnet:     newTailnet,
				StatusInterval: 20 * time.Millisecond,
			})
			if err != nil {
				return nil, err
			}
			return h, nil
		},
		Prefs:      prefs.Store{File: filepath.Join(t.TempDir(), "prefs.json")},
		Keychain:   &memKeychain{},
		Autostart:  &fakeAutostart{},
		RetryEvery: 20 * time.Millisecond,
	})
	s.Start(false)
	t.Cleanup(s.Close)
	return s
}

func TestTheAppHostsTheNodeAndTheCLIReachesItOverTheSocket(t *testing.T) {
	dir := t.TempDir()
	defiosCLI(t, dir, "init", "--name", "laptop")
	defiosCLI(t, dir, "group", "new", "alpha", "--headscale", "https://hs.example")
	defiosCLI(t, dir, "group", "new", "beta", "--headscale", "https://hs.example")
	p := nodedir.Resolve(nodedir.Lookup{Dir: dir})

	app := hostShell(t, p)
	if st := app.State(); st.View != shell.ViewLocked || st.Node.Name != "laptop" {
		t.Fatalf("lock screen: %+v", st)
	}
	// Unformed, beta opens on onboarding step 3.
	if st := app.Unlock("beta", pw); st.View != shell.ViewForming || st.Served != "beta" {
		t.Fatalf("unlock: %+v", st)
	}
	u, err := (serve.Client{Socket: p.Socket}).Usage(context.Background())
	if err != nil || u.Alias != "beta" {
		t.Fatalf("socket: %+v, %v", u, err)
	}

	// A second app on the same Node waits for the first to stop.
	second := hostShell(t, p)
	if st := second.State(); st.View != shell.ViewServedElsewhere {
		t.Fatalf("second app: %+v", st)
	}
	app.Lock()
	eventually(t, second, view(shell.ViewLocked))
	if st := second.Unlock("alpha", pw); st.Served != "alpha" {
		t.Fatalf("second app unlock: %+v", st)
	}
}

// mesh is one in-process tailnet: each Node's listeners are reachable at
// "<hostname>:<port>", and every Node that serves formation is a peer.
type mesh struct {
	mu    sync.Mutex
	addrs map[string]string // "defios-…:4634" -> loopback addr
}

type meshTailnet struct {
	m        *mesh
	hostname string
}

func (m *mesh) tailnet(o serve.TsnetOptions) serve.Tailnet {
	return &meshTailnet{m: m, hostname: o.Hostname}
}

func (t *meshTailnet) Up(context.Context) error { return nil }

func (t *meshTailnet) Close() error {
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	for k := range t.m.addrs {
		if strings.HasPrefix(k, t.hostname+":") {
			delete(t.m.addrs, k)
		}
	}
	return nil
}

func (t *meshTailnet) Listen(_, addr string) (net.Listener, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(addr)
	t.m.mu.Lock()
	t.m.addrs[t.hostname+":"+port] = l.Addr().String()
	t.m.mu.Unlock()
	return l, nil
}

func (t *meshTailnet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	t.m.mu.Lock()
	real, ok := t.m.addrs[addr]
	t.m.mu.Unlock()
	if !ok {
		return nil, errors.New("mesh: no route to " + addr)
	}
	return (&net.Dialer{}).DialContext(ctx, network, real)
}

func (t *meshTailnet) HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: t.Dial}}
}

func (t *meshTailnet) Peers(context.Context) ([]serve.Peer, error) {
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	var peers []serve.Peer
	for k := range t.m.addrs {
		host, port, _ := net.SplitHostPort(k)
		if port == "4634" && host != t.hostname {
			peers = append(peers, serve.Peer{Hostname: host, IP: host, Online: true})
		}
	}
	return peers, nil
}

// onboard is a fresh app taken through steps 1 and 2 on m.
func onboard(t *testing.T, m *mesh, name string) (*shell.Shell, nodedir.Paths) {
	t.Helper()
	p := nodedir.Resolve(nodedir.Lookup{Dir: t.TempDir()})
	s := meshShell(t, p, m.tailnet)
	if st := s.State(); st.View != shell.ViewNoNode {
		t.Fatalf("%s: a fresh Node directory opens on %s", name, st.View)
	}
	if st := s.Create(name, pw); st.View != shell.ViewNoGroup {
		t.Fatalf("%s: after Create: %+v", name, st)
	}
	if st := s.AddGroup("home", "https://hs.example", "tskey-auth-"+name, ""); st.View != shell.ViewForming {
		t.Fatalf("%s: after step 2: %+v", name, st)
	}
	return s, p
}

func TestTwoAppsFormAGroupEntirelyThroughOnboarding(t *testing.T) {
	m := &mesh{addrs: map[string]string{}}
	alice, p := onboard(t, m, "alice")
	bob, _ := onboard(t, m, "bob")

	// The auth key is a Vault Secret, which the config only names.
	cfg, err := os.ReadFile(p.GroupConfigFile("home"))
	if err != nil || strings.Contains(string(cfg), "tskey-auth-alice") || !strings.Contains(string(cfg), "$"+node.TailnetAuthKeySecret) {
		t.Fatalf("config.toml: %v\n%s", err, cfg)
	}
	h, err := node.Open(p, node.Options{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	given := func() (string, bool, error) { return pw, true, nil }
	if v, src, err := h.RevealSecret(context.Background(), "home", node.TailnetAuthKeySecret, given); err != nil || v != "tskey-auth-alice" || src != secret.SourceVault {
		t.Fatalf("auth key Secret = %q from %q, %v", v, src, err)
	}

	form(t, alice, bob)
}

// form forms alice's and bob's Group, each at step 3, through the app.
func form(t *testing.T, alice, bob *shell.Shell) {
	t.Helper()
	var st shell.State
	eventually(t, alice, func(shell.State) bool {
		st = alice.Discover()
		return st.Forming != nil && len(st.Forming.Discovered) == 1
	})
	found := st.Forming.Discovered[0]
	if found.Name != "bob" || found.ID != bob.State().Node.ID {
		t.Fatalf("alice discovered %+v", found)
	}
	if st := alice.Propose([]string{found.ID}); st.Problem != nil || st.Forming.Proposal == nil || !st.Forming.Proposal.Mine {
		t.Fatalf("propose: %+v", st)
	}

	st = eventually(t, bob, func(st shell.State) bool { return st.Forming != nil && st.Forming.Proposal != nil })
	if p := st.Forming.Proposal; p.Mine || len(p.Roster) != 2 {
		t.Fatalf("bob's invite: %+v", p)
	}
	if st := bob.Accept(); st.View != shell.ViewServed || st.Served != "home" {
		t.Fatalf("bob after Accept: %+v %+v", st, st.Forming)
	}
	eventually(t, alice, view(shell.ViewServed))
}

// A new display name, once the Group is served again, is what the other
// device's Roster shows.
func TestARenamedDeviceShowsItsNewNameToTheOtherOnceServedAgain(t *testing.T) {
	m := &mesh{addrs: map[string]string{}}
	alice, _ := onboard(t, m, "alice")
	bob, _ := onboard(t, m, "bob")
	form(t, alice, bob)

	d := alice.GroupSettings().Settings
	d.DisplayName = "alice-desk"
	if v := alice.SaveGroupSettings(d); !v.Saved || !v.Served {
		t.Fatalf("save: %+v %+v", v, v.Problem)
	}
	alice.Lock()
	if st := alice.Unlock("home", pw); st.View != shell.ViewServed {
		t.Fatalf("serve again: %+v", st)
	}
	id := alice.State().Node.ID
	eventually(t, bob, func(shell.State) bool {
		for _, dev := range bob.GroupSettings().Roster {
			if dev.ID == id {
				return dev.Name == "alice-desk" && dev.Reachable
			}
		}
		return false
	})
}
