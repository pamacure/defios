// Package serve is the long-running process: it holds the unlocked Vault,
// speaks to other Nodes over tsnet, and to the local CLI over a unix socket
// (docs/spec/mvp.md §1, §3.5, §4.1, §6.1).
package serve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/nodedir"
	groupv1 "defios.com/proto/group/v1"
)

// Ports on tsnet (§4.1).
const (
	RaftPort      = 4632
	ForwardPort   = 4633
	FormationPort = 4634
)

// ErrAlreadyRunning means another serve has the socket bound.
var ErrAlreadyRunning = errors.New("serve: another serve is already running on this data dir")

// Peer is one machine on the tailnet as the control plane reports it.
type Peer struct {
	Hostname string
	IP       string // tailnet IPv4
	Online   bool
}

// Tailnet is the slice of tsnet.Server this package uses. The production
// adapter is NewTsnet; tests substitute a loopback fake.
type Tailnet interface {
	Up(ctx context.Context) error
	Listen(network, addr string) (net.Listener, error)
	HTTPClient() *http.Client
	Peers(ctx context.Context) ([]Peer, error)
	Close() error
}

// Options is everything Run needs; the Vault has already been unlocked
// into Node by the caller.
type Options struct {
	Paths   nodedir.Paths
	Node    *identity.Node
	Config  config.Config
	Tailnet Tailnet
	Logf    func(format string, args ...any)
}

// Run binds the socket, joins the tailnet, serves formation HTTP and the
// local API, and returns when ctx is done or a listener fails.
func Run(ctx context.Context, o Options) error {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	socket, err := bindSocket(o.Paths.Socket)
	if err != nil {
		return err
	}
	defer socket.Close()

	if err := o.Tailnet.Up(ctx); err != nil {
		return fmt.Errorf("serve: tailnet: %w", err)
	}
	defer o.Tailnet.Close()

	self, err := group.SignIdentity(o.Node, o.Config.DisplayName, o.Config.Hostname)
	if err != nil {
		return err
	}
	formation, err := o.Tailnet.Listen("tcp", fmt.Sprintf(":%d", FormationPort))
	if err != nil {
		return fmt.Errorf("serve: formation listener: %w", err)
	}
	defer formation.Close()

	s := &server{opts: o, self: self}
	formationSrv := &http.Server{Handler: s.formationHandler()}
	localSrv := &http.Server{Handler: s.localHandler()}

	errc := make(chan error, 2)
	go func() { errc <- formationSrv.Serve(formation) }()
	go func() { errc <- localSrv.Serve(socket) }()
	o.Logf("serve: node %s up as %s; socket %s", o.Node.NodeID, o.Config.Hostname, o.Paths.Socket)

	select {
	case <-ctx.Done():
		err = nil
	case err = <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	formationSrv.Shutdown(shutdownCtx)
	localSrv.Shutdown(shutdownCtx)
	return err
}

// server is the running state behind both HTTP handlers.
type server struct {
	opts Options
	self *groupv1.Identity // signed once at startup
}

// bindSocket listens on path with mode 0600. A live listener there means
// another serve; a dead socket file is replaced.
func bindSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("serve: %w", err)
	}
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		conn.Close()
		return nil, ErrAlreadyRunning
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("serve: %w", err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("serve: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, fmt.Errorf("serve: %w", err)
	}
	return l, nil
}
