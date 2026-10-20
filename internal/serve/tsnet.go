package serve

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	"tailscale.com/tsnet"
)

// TsnetOptions configures the production Tailnet against a stock headscale.
type TsnetOptions struct {
	Dir        string // state dir, $XDG_DATA_HOME/defios/tsnet
	Hostname   string // config.toml hostname
	ControlURL string // config.toml headscale_url
	AuthKey    string // TS_AUTHKEY (or config.toml auth_key, discouraged)
	Logf       func(format string, args ...any)
}

// NewTsnet wraps a tsnet.Server as a Tailnet.
func NewTsnet(o TsnetOptions) Tailnet {
	srv := &tsnet.Server{
		Dir:        o.Dir,
		Hostname:   o.Hostname,
		ControlURL: o.ControlURL,
		AuthKey:    o.AuthKey,
		Logf:       func(string, ...any) {}, // tsnet's debug firehose; UserLogf below is what operators see
	}
	if o.Logf != nil {
		srv.UserLogf = o.Logf
	}
	return &tsnetTailnet{srv: srv}
}

type tsnetTailnet struct {
	srv *tsnet.Server
}

func (t *tsnetTailnet) Up(ctx context.Context) error {
	_, err := t.srv.Up(ctx)
	return err
}

func (t *tsnetTailnet) Listen(network, addr string) (net.Listener, error) {
	return t.srv.Listen(network, addr)
}

// Dial reaches "<tsnet hostname>:<port>"; MagicDNS resolves the name.
func (t *tsnetTailnet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return t.srv.Dial(ctx, network, addr)
}

func (t *tsnetTailnet) HTTPClient() *http.Client { return t.srv.HTTPClient() }

func (t *tsnetTailnet) Close() error { return t.srv.Close() }

// Peers lists every other machine the control plane knows, by tailnet IPv4.
func (t *tsnetTailnet) Peers(ctx context.Context) ([]Peer, error) {
	lc, err := t.srv.LocalClient()
	if err != nil {
		return nil, err
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return nil, err
	}
	var peers []Peer
	for _, p := range st.Peer {
		var ip string
		for _, a := range p.TailscaleIPs {
			if a.Is4() {
				ip = a.String()
				break
			}
		}
		if ip == "" {
			continue
		}
		peers = append(peers, Peer{
			Hostname: strings.TrimSuffix(p.DNSName, "."),
			IP:       ip,
			Online:   p.Online,
		})
	}
	if len(peers) == 0 && len(st.Peer) > 0 {
		return nil, fmt.Errorf("tsnet: %d peers but none with an IPv4 address", len(st.Peer))
	}
	return peers, nil
}
