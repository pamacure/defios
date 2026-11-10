package node_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"defios.com/internal/cli"
	"defios.com/internal/node"
	"defios.com/internal/nodedir"
	"defios.com/internal/serve"
	"defios.com/internal/vault"
)

const passphrase = "hunter2hunter2"

// defios runs one CLI verb against dir, failing the test on a non-zero exit.
func defios(t *testing.T, dir string, args ...string) {
	t.Helper()
	var out, errb bytes.Buffer
	env := map[string]string{"DEFIOS_PASSPHRASE": passphrase, "DEFIOS_ECDSA_PREPARAMS": "skip"}
	app := cli.App{
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &errb,
		Getenv: func(k string) string { return env[k] },
		Home:   t.TempDir(),
	}
	if code := app.Run(context.Background(), append([]string{"--dir", dir}, args...)); code != 0 {
		t.Fatalf("defios %v: exit %d\n%s", args, code, errb.String())
	}
}

// nodeDir is a Node with Groups "alpha" and "beta", both unformed.
func nodeDir(t *testing.T) nodedir.Paths {
	t.Helper()
	dir := t.TempDir()
	defios(t, dir, "init", "--name", "laptop")
	defios(t, dir, "group", "new", "alpha", "--headscale", "https://hs.example")
	defios(t, dir, "group", "new", "beta", "--headscale", "https://hs.example")
	return nodedir.Resolve(nodedir.Lookup{Dir: dir})
}

// loopTailnet is a tailnet of one: it comes up at once, listens on
// loopback and has no peers.
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

func open(t *testing.T, p nodedir.Paths) *node.Host {
	t.Helper()
	h, err := node.Open(p, node.Options{
		NewTailnet:     func(serve.TsnetOptions) serve.Tailnet { return loopTailnet{} },
		StatusInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Lock() })
	return h
}

// servedAlias asks the Node's socket which Group it serves.
func servedAlias(t *testing.T, p nodedir.Paths) string {
	t.Helper()
	u, err := (serve.Client{Socket: p.Socket}).Usage(context.Background())
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	return u.Alias
}

// until reads events until one satisfies ok.
func until(t *testing.T, events <-chan node.Event, ok func(node.Status) bool) node.Status {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, open := <-events:
			if !open {
				t.Fatal("subscription closed")
			}
			if ev.Status != nil && ok(*ev.Status) {
				return *ev.Status
			}
		case <-timeout:
			t.Fatal("no matching event")
		}
	}
}

func TestOpenWithoutANodeIsErrNoNode(t *testing.T) {
	_, err := node.Open(nodedir.Resolve(nodedir.Lookup{Dir: t.TempDir()}), node.Options{})
	if !errors.Is(err, node.ErrNoNode) {
		t.Fatalf("err = %v; want ErrNoNode", err)
	}
}

func TestUnlockServesTheGroupAndLockStops(t *testing.T) {
	p := nodeDir(t)
	h := open(t, p)

	if err := h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	if got := servedAlias(t, p); got != "alpha" {
		t.Fatalf("socket serves %q; want alpha", got)
	}
	if st := h.Status(); !st.Serving || st.Group != "alpha" {
		t.Fatalf("status = %+v; want serving alpha", st)
	}

	if err := h.Lock(); err != nil {
		t.Fatal(err)
	}
	if serve.IsUp(p.Socket) {
		t.Fatal("socket still served after Lock")
	}
	if st := h.Status(); st.Serving || st.Group != "" {
		t.Fatalf("status after Lock = %+v; want locked", st)
	}
	// Lock forgot the passphrase: there is nothing to switch from.
	if err := h.Switch("beta"); !errors.Is(err, node.ErrLocked) {
		t.Fatalf("Switch after Lock: %v; want ErrLocked", err)
	}
}

func TestUnlockAgainAfterLock(t *testing.T) {
	p := nodeDir(t)
	h := open(t, p)
	if err := h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := h.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := h.Unlock(passphrase, "beta"); err != nil {
		t.Fatal(err)
	}
	if got := servedAlias(t, p); got != "beta" {
		t.Fatalf("socket serves %q; want beta", got)
	}
}

func TestSwitchServesTheOtherGroupWithTheHeldPassphrase(t *testing.T) {
	p := nodeDir(t)
	h := open(t, p)
	if err := h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := h.Switch("beta"); err != nil {
		t.Fatal(err)
	}
	if got := servedAlias(t, p); got != "beta" {
		t.Fatalf("socket serves %q; want beta", got)
	}
	if st := h.Status(); !st.Serving || st.Group != "beta" {
		t.Fatalf("status = %+v; want serving beta", st)
	}
}

func TestSwitchToAnUnknownGroupKeepsServing(t *testing.T) {
	p := nodeDir(t)
	h := open(t, p)
	if err := h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := h.Switch("gamma"); !errors.Is(err, node.ErrUnknownGroup) {
		t.Fatalf("Switch: %v; want ErrUnknownGroup", err)
	}
	if got := servedAlias(t, p); got != "alpha" {
		t.Fatalf("socket serves %q; want alpha still", got)
	}
}

func TestUnlockWhileServingIsRefused(t *testing.T) {
	p := nodeDir(t)
	h := open(t, p)
	if err := h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := h.Unlock(passphrase, "beta"); !errors.Is(err, node.ErrUnlocked) {
		t.Fatalf("second Unlock: %v; want ErrUnlocked", err)
	}
}

func TestASecondHostOnTheSameNodeIsRefused(t *testing.T) {
	p := nodeDir(t)
	first := open(t, p)
	if err := first.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	second := open(t, p)
	if err := second.Check("beta"); !errors.Is(err, node.ErrServedElsewhere) {
		t.Fatalf("Check: %v; want ErrServedElsewhere", err)
	}
	if err := second.Unlock(passphrase, "beta"); !errors.Is(err, node.ErrServedElsewhere) {
		t.Fatalf("Unlock: %v; want ErrServedElsewhere", err)
	}
	if got := servedAlias(t, p); got != "alpha" {
		t.Fatalf("socket serves %q; want the first host's alpha", got)
	}
	if st := second.Status(); st.Serving {
		t.Fatalf("refused host reports %+v", st)
	}
}

func TestUnlockWithTheWrongPassphrase(t *testing.T) {
	p := nodeDir(t)
	h := open(t, p)
	if err := h.Unlock("wrong passphrase", "alpha"); !errors.Is(err, vault.ErrBadPassphrase) {
		t.Fatalf("err = %v; want vault.ErrBadPassphrase", err)
	}
	if serve.IsUp(p.Socket) {
		t.Fatal("socket served after a refused Unlock")
	}
}

func TestUnlockAnUnknownGroup(t *testing.T) {
	p := nodeDir(t)
	h := open(t, p)
	if err := h.Unlock(passphrase, "gamma"); !errors.Is(err, node.ErrUnknownGroup) {
		t.Fatalf("err = %v; want ErrUnknownGroup", err)
	}
}

func TestSubscriptionCarriesNodeStatus(t *testing.T) {
	p := nodeDir(t)
	h := open(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := h.Subscribe(ctx)
	until(t, events, func(s node.Status) bool { return !s.Serving }) // the status when subscribing

	if err := h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	st := until(t, events, func(s node.Status) bool { return s.Serving && s.TailnetUp })
	if st.Group != "alpha" {
		t.Fatalf("status = %+v; want alpha", st)
	}
	// An unformed Group has no Roster, so no majority to reach.
	if len(st.Roster) != 0 || st.MajorityReachable || st.Leader != "" {
		t.Fatalf("unformed Group status = %+v", st)
	}

	if err := h.Lock(); err != nil {
		t.Fatal(err)
	}
	if st := until(t, events, func(s node.Status) bool { return !s.Serving }); st.Err != nil {
		t.Fatalf("Lock reported %v", st.Err)
	}

	cancel()
	for range events { // closes once ctx is done
	}
}
