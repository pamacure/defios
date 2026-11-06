package serve

import (
	"context"
	"net/http"

	"defios.com/internal/chainfill"
	"defios.com/internal/network"
	"defios.com/internal/secret"
	"defios.com/internal/signreq"
)

type signFillRequest struct {
	Key          string           `json:"key"`
	Transactions []signreq.TxSpec `json:"transactions"`
}

// signFill completes txs for the Key keyQuery names from this Node's
// Networks for it, as sign propose resolves them (ADR 0029). It reads the
// chains and writes nothing.
func (s *server) signFill(ctx context.Context, keyQuery string, txs []signreq.TxSpec) (chainfill.Result, error) {
	_, store, running := s.current()
	if !running {
		return chainfill.Result{}, ErrNoReplica
	}
	k, _, nets, err := s.keyNetworks(store, keyQuery)
	if err != nil {
		return chainfill.Result{}, err
	}
	r := &secret.Resolver{Group: s.opts.Alias, Getenv: s.opts.Getenv, Vault: s.opts.Secrets}
	return chainfill.Fill(ctx, k, nets, txs, func(n network.Network) ([]string, error) {
		return endpoints(r, nets, n.Chain())
	})
}

func (s *server) fillRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sign/fill", func(w http.ResponseWriter, r *http.Request) {
		var req signFillRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		v, err := s.signFill(r.Context(), req.Key, req.Transactions)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, v)
	})
}

// SignFill is `sign fill`: txs completed from this Node's Networks.
func (c Client) SignFill(ctx context.Context, keyQuery string, txs []signreq.TxSpec) (chainfill.Result, error) {
	var v chainfill.Result
	err := c.do(ctx, "/v1/sign/fill", signFillRequest{Key: keyQuery, Transactions: txs}, &v)
	return v, err
}
