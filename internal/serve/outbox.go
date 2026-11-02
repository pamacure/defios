package serve

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"defios.com/internal/outbox"
)

type outboxDropRequest struct {
	// MsgID is a hex prefix of the msg_id of the queued message to drop.
	MsgID string `json:"msg_id"`
}

// DropView is the queued message a drop erased: its hex msg_id.
type DropView struct {
	MsgID string `json:"msg_id"`
}

// outboxRoutes withdraw this Node's queued messages (ADR 0025). Dropping
// one needs no tailnet: it never reaches the log.
func (s *server) outboxRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/outbox/drop", func(w http.ResponseWriter, r *http.Request) {
		var req outboxDropRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, localMaxBytes)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ob, store, ok := s.writer()
		if !ok {
			writeError(w, ErrNoReplica)
			return
		}
		dropped, err := outbox.Withdraw(ob, store, req.MsgID)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, DropView{MsgID: hex.EncodeToString(dropped.MsgID[:])})
	})
}

// OutboxDrop is `outbox drop`: erase the queued message whose msg_id starts
// with the hex prefix msgID.
func (c Client) OutboxDrop(ctx context.Context, msgID string) (DropView, error) {
	var v DropView
	err := c.do(ctx, "/v1/outbox/drop", outboxDropRequest{MsgID: msgID}, &v)
	return v, err
}
