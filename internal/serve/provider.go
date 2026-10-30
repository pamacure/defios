package serve

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/provider"
	"defios.com/internal/raftx"
	coslv1 "defios.com/proto/cosl/v1"
)

type providerRequest struct {
	Alias   string `json:"alias"`
	BaseURL string `json:"base_url,omitempty"`
	APIPath string `json:"api_path,omitempty"`
}

func (r providerRequest) config() *coslv1.ProviderConfig {
	return &coslv1.ProviderConfig{Alias: r.Alias, BaseUrl: r.BaseURL, ApiPath: r.APIPath}
}

// providerWrite authors one provider.* message and commits it through Raft.
// The writer checks the message against the folded state first, so a
// mistaken add/edit/remove fails loudly instead of being skipped by the
// fold.
func (s *server) providerWrite(ctx context.Context, typ uint32, cfg *coslv1.ProviderConfig, alias string) (SendView, error) {
	var payload proto.Message = cfg
	if typ == cosl.TypeProviderRemove {
		payload = &coslv1.ProviderRemove{Alias: alias}
	} else if err := provider.Validate(cfg); err != nil {
		return SendView{}, err
	}
	replica, store, running := s.current()
	rec, held := s.slot.Current()
	if !running || !held {
		return SendView{}, ErrNoReplica
	}
	rows, err := store.Log(cosl.Query{MsgTypes: provider.MsgTypes})
	if err != nil {
		return SendView{}, err
	}
	providers, err := provider.Fold(rows)
	if err != nil {
		return SendView{}, err
	}
	if err := provider.Check(providers, typ, alias); err != nil {
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

func (s *server) providerRoutes(mux *http.ServeMux) {
	for path, typ := range map[string]uint32{
		"/v1/provider/add":    cosl.TypeProviderCreate,
		"/v1/provider/edit":   cosl.TypeProviderUpdate,
		"/v1/provider/remove": cosl.TypeProviderRemove,
	} {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			var req providerRequest
			if !s.readLocal(w, r, &req) {
				return
			}
			v, err := s.providerWrite(r.Context(), typ, req.config(), req.Alias)
			if err != nil {
				writeError(w, err)
				return
			}
			writeJSON(w, v)
		})
	}
}

func providerClientRequest(cfg *coslv1.ProviderConfig) providerRequest {
	return providerRequest{Alias: cfg.Alias, BaseURL: cfg.BaseUrl, APIPath: cfg.ApiPath}
}

// ProviderAdd is `provider add`: commit a provider.create.
func (c Client) ProviderAdd(ctx context.Context, cfg *coslv1.ProviderConfig) (SendView, error) {
	var v SendView
	err := c.do(ctx, "/v1/provider/add", providerClientRequest(cfg), &v)
	return v, err
}

// ProviderEdit is `provider edit`: commit a provider.update superseding
// cfg.Alias.
func (c Client) ProviderEdit(ctx context.Context, cfg *coslv1.ProviderConfig) (SendView, error) {
	var v SendView
	err := c.do(ctx, "/v1/provider/edit", providerClientRequest(cfg), &v)
	return v, err
}

// ProviderRemove is `provider remove`: commit a provider.remove.
func (c Client) ProviderRemove(ctx context.Context, alias string) (SendView, error) {
	var v SendView
	err := c.do(ctx, "/v1/provider/remove", providerRequest{Alias: alias}, &v)
	return v, err
}
