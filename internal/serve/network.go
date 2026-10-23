package serve

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/network"
	"defios.com/internal/raftx"
	coslv1 "defios.com/proto/cosl/v1"
)

type networkRequest struct {
	Alias       string   `json:"alias"`
	Type        string   `json:"type,omitempty"`
	RPCURLs     []string `json:"rpc_urls,omitempty"`
	ChainID     uint64   `json:"chain_id,omitempty"`
	Network     string   `json:"network,omitempty"`
	Cluster     string   `json:"cluster,omitempty"`
	GenesisHash string   `json:"genesis_hash,omitempty"`
}

func (r networkRequest) config() *coslv1.NetworkConfig {
	return &coslv1.NetworkConfig{
		Alias: r.Alias, Type: r.Type, RpcUrls: r.RPCURLs, ChainId: r.ChainID,
		Network: r.Network, Cluster: r.Cluster, GenesisHash: r.GenesisHash,
	}
}

// networkWrite authors one network.* message and commits it through Raft.
// The writer checks the message against the folded state first, so a
// mistaken add/edit/remove fails loudly instead of being skipped by the fold.
func (s *server) networkWrite(ctx context.Context, typ uint32, cfg *coslv1.NetworkConfig, alias string) (SendView, error) {
	var payload proto.Message = cfg
	if typ == cosl.TypeNetworkRemove {
		payload = &coslv1.NetworkRemove{Alias: alias}
	} else if err := network.Validate(cfg); err != nil {
		return SendView{}, err
	}
	replica, store, running := s.current()
	rec, held := s.slot.Current()
	if !running || !held {
		return SendView{}, ErrNoReplica
	}
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		return SendView{}, err
	}
	nets, err := network.Fold(rows)
	if err != nil {
		return SendView{}, err
	}
	if err := network.Check(nets, typ, cfg, alias); err != nil {
		return SendView{}, err
	}
	author := cosl.Author{Node: s.opts.Node, GroupID: rec.GroupID()}
	signed, err := author.Sign(typ, nil, payload)
	if err != nil {
		return SendView{}, fmt.Errorf("serve: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, raftx.ApplyTimeout)
	defer cancel()
	pos, err := replica.Append(ctx, signed)
	if err != nil {
		return SendView{}, err
	}
	return SendView{Seq: pos.Index, Term: pos.Term, MsgID: hex.EncodeToString(signed.MsgID[:])}, nil
}

func (s *server) networkRoutes(mux *http.ServeMux) {
	for path, typ := range map[string]uint32{
		"/v1/network/add":    cosl.TypeNetworkCreate,
		"/v1/network/edit":   cosl.TypeNetworkUpdate,
		"/v1/network/remove": cosl.TypeNetworkRemove,
	} {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			var req networkRequest
			if !s.readLocal(w, r, &req) {
				return
			}
			v, err := s.networkWrite(r.Context(), typ, req.config(), req.Alias)
			if err != nil {
				writeError(w, err)
				return
			}
			writeJSON(w, v)
		})
	}
}

func networkClientRequest(cfg *coslv1.NetworkConfig) networkRequest {
	return networkRequest{
		Alias: cfg.Alias, Type: cfg.Type, RPCURLs: cfg.RpcUrls, ChainID: cfg.ChainId,
		Network: cfg.Network, Cluster: cfg.Cluster, GenesisHash: cfg.GenesisHash,
	}
}

// NetworkAdd is `network add`: commit a network.create.
func (c Client) NetworkAdd(ctx context.Context, cfg *coslv1.NetworkConfig) (SendView, error) {
	var v SendView
	err := c.do(ctx, "/v1/network/add", networkClientRequest(cfg), &v)
	return v, err
}

// NetworkEdit is `network edit`: commit a network.update superseding cfg.Alias.
func (c Client) NetworkEdit(ctx context.Context, cfg *coslv1.NetworkConfig) (SendView, error) {
	var v SendView
	err := c.do(ctx, "/v1/network/edit", networkClientRequest(cfg), &v)
	return v, err
}

// NetworkRemove is `network remove`: commit a network.remove tombstone.
func (c Client) NetworkRemove(ctx context.Context, alias string) (SendView, error) {
	var v SendView
	err := c.do(ctx, "/v1/network/remove", networkRequest{Alias: alias}, &v)
	return v, err
}
