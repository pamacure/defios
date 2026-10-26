package serve

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/model"
	"defios.com/internal/raftx"
	coslv1 "defios.com/proto/cosl/v1"
)

type modelRequest struct {
	Alias    string `json:"alias"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	APIPath  string `json:"api_path,omitempty"`
}

func (r modelRequest) config() *coslv1.ModelConfig {
	return &coslv1.ModelConfig{Alias: r.Alias, Provider: r.Provider, Model: r.Model, BaseUrl: r.BaseURL, ApiPath: r.APIPath}
}

// modelWrite authors one model.* message and commits it through Raft. The
// writer checks the message against the folded state first, so a mistaken
// add/edit/remove fails loudly instead of being skipped by the fold.
func (s *server) modelWrite(ctx context.Context, typ uint32, cfg *coslv1.ModelConfig, alias string) (SendView, error) {
	var payload proto.Message = cfg
	if typ == cosl.TypeModelRemove {
		payload = &coslv1.ModelRemove{Alias: alias}
	} else if err := model.Validate(cfg); err != nil {
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
	models, err := model.Fold(rows)
	if err != nil {
		return SendView{}, err
	}
	if err := model.Check(models, typ, cfg, alias); err != nil {
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

func (s *server) modelRoutes(mux *http.ServeMux) {
	for path, typ := range map[string]uint32{
		"/v1/model/add":    cosl.TypeModelCreate,
		"/v1/model/edit":   cosl.TypeModelUpdate,
		"/v1/model/remove": cosl.TypeModelRemove,
	} {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			var req modelRequest
			if !s.readLocal(w, r, &req) {
				return
			}
			v, err := s.modelWrite(r.Context(), typ, req.config(), req.Alias)
			if err != nil {
				writeError(w, err)
				return
			}
			writeJSON(w, v)
		})
	}
}

func modelClientRequest(cfg *coslv1.ModelConfig) modelRequest {
	return modelRequest{Alias: cfg.Alias, Provider: cfg.Provider, Model: cfg.Model, BaseURL: cfg.BaseUrl, APIPath: cfg.ApiPath}
}

// ModelAdd is `model add`: commit a model.create.
func (c Client) ModelAdd(ctx context.Context, cfg *coslv1.ModelConfig) (SendView, error) {
	var v SendView
	err := c.do(ctx, "/v1/model/add", modelClientRequest(cfg), &v)
	return v, err
}

// ModelEdit is `model edit`: commit a model.update superseding cfg.Alias.
func (c Client) ModelEdit(ctx context.Context, cfg *coslv1.ModelConfig) (SendView, error) {
	var v SendView
	err := c.do(ctx, "/v1/model/edit", modelClientRequest(cfg), &v)
	return v, err
}

// ModelRemove is `model remove`: commit a model.remove.
func (c Client) ModelRemove(ctx context.Context, alias string) (SendView, error) {
	var v SendView
	err := c.do(ctx, "/v1/model/remove", modelRequest{Alias: alias}, &v)
	return v, err
}
