package serve

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"google.golang.org/protobuf/proto"
)

// Status is what the local API reports about this serve.
type Status struct {
	NodeID    string `json:"node_id"`
	Hostname  string `json:"hostname"`
	TailnetUp bool   `json:"tailnet_up"`
}

// ErrTailnetNotUp is returned for tailnet operations before Up completes.
var ErrTailnetNotUp = errors.New("serve: tailnet is still coming up")

// NodeSummary is one discovered, verified Identity.
type NodeSummary struct {
	NodeID              string `json:"node_id"`
	DisplayName         string `json:"display_name"`
	Hostname            string `json:"hostname"`
	AuthPublicKey       string `json:"auth_public_key"`
	EncryptionPublicKey string `json:"encryption_public_key"`
}

// formationHandler is what other Nodes reach on :4634.
func (s *server) formationHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/identity", func(w http.ResponseWriter, r *http.Request) {
		raw, err := proto.Marshal(s.self)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Write(raw)
	})
	return mux
}

// localHandler is what the CLI reaches over the unix socket. Framing is
// HTTP+JSON; it is this Node's private API, not a public one (§3.5).
func (s *server) localHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, Status{
			NodeID:    s.opts.Node.NodeID.String(),
			Hostname:  s.opts.Config.Hostname,
			TailnetUp: s.tailnetUp.Load(),
		})
	})
	mux.HandleFunc("GET /v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		if !s.tailnetUp.Load() {
			http.Error(w, ErrTailnetNotUp.Error(), http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		nodes, err := s.discover(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out := make([]NodeSummary, 0, len(nodes))
		for _, n := range nodes {
			out = append(out, NodeSummary{
				NodeID:              n.NodeID.String(),
				DisplayName:         n.DisplayName,
				Hostname:            n.TsnetHostname,
				AuthPublicKey:       hex.EncodeToString(n.AuthPublicKey),
				EncryptionPublicKey: hex.EncodeToString(n.EncryptionPublicKey),
			})
		}
		writeJSON(w, out)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ErrNotRunning means no serve answers on the socket.
var ErrNotRunning = errors.New("serve is not running")

// Client is the CLI's side of the unix socket.
type Client struct {
	Socket string
}

// IsUp reports whether a serve answers on socket.
func IsUp(socket string) bool {
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (c Client) http() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second, // bounds discovery; a wedged serve must not hang the CLI
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", c.Socket)
			},
		},
	}
}

func (c Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://serve"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			return fmt.Errorf("%w (no socket at %s)", ErrNotRunning, c.Socket)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		return ErrTailnetNotUp
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("serve: %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Status asks the running serve who it is.
func (c Client) Status(ctx context.Context) (Status, error) {
	var st Status
	err := c.get(ctx, "/v1/status", &st)
	return st, err
}

// Nodes asks the running serve to discover tailnet peers and return the
// Node Identities that verify.
func (c Client) Nodes(ctx context.Context) ([]NodeSummary, error) {
	var nodes []NodeSummary
	err := c.get(ctx, "/v1/nodes", &nodes)
	return nodes, err
}
