package shell_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"defios.com/desktop/internal/prefs"
	"defios.com/desktop/internal/shell"
	"defios.com/internal/cli"
	"defios.com/internal/node"
	"defios.com/internal/nodedir"
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
	t.Helper()
	s := shell.New(shell.Deps{
		Open: func() (shell.Node, error) {
			h, err := node.Open(p, node.Options{
				NewTailnet:     func(serve.TsnetOptions) serve.Tailnet { return loopTailnet{} },
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
	if st := app.Unlock("beta", pw); st.View != shell.ViewServed || st.Served != "beta" {
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
	if st := second.Unlock("alpha", pw); st.View != shell.ViewServed {
		t.Fatalf("second app unlock: %+v", st)
	}
}
