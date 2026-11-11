package node

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/outbox"
	"defios.com/internal/signreq"
	coslv1 "defios.com/proto/cosl/v1"
)

// Outbox failures a front end tells apart: outbox.ErrBadMsgID,
// outbox.ErrNotQueued, outbox.ErrAmbiguous, outbox.ErrInFlight and
// outbox.ErrCommitted (the message is on the log: it cannot be dropped).

// Queued is one message this Node wrote while no leader was known, waiting
// in its Outbox to commit (ADR 0025).
type Queued struct {
	MsgID          string // hex
	Type           string // its msg_type's name
	ConversationID string // hex; GroupTalk for the Group-level talk
	CreatedAt      time.Time
	// Deadline is a Sign Request's, past which it lapses; zero otherwise.
	Deadline time.Time
	Summary  string
}

// Outbox is groupAlias's queued messages, oldest first. A running serve
// persists each change before it makes it, so the file is current.
func (h *Host) Outbox(_ context.Context, groupAlias string) ([]Queued, error) {
	if err := h.knownGroup(groupAlias); err != nil {
		return nil, err
	}
	signed, err := outbox.Read(h.p.OutboxFile(groupAlias))
	if err != nil {
		return nil, err
	}
	return queuedOf(signed)
}

func queuedOf(signed []cosl.Signed) ([]Queued, error) {
	out := make([]Queued, 0, len(signed))
	for _, s := range signed {
		env, err := cosl.Unmarshal(s.Bytes)
		if err != nil {
			return nil, err
		}
		q := Queued{
			MsgID:          hex.EncodeToString(s.MsgID[:]),
			Type:           cosl.TypeName(env.MsgType),
			ConversationID: hex.EncodeToString(env.ConversationId),
			CreatedAt:      time.Unix(0, env.CreatedAt),
			Summary:        cosl.Summary(env.MsgType, env.Payload),
		}
		if env.MsgType == cosl.TypeSignRequest {
			msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
			if err != nil {
				return nil, err
			}
			q.Deadline = signreq.Deadline(env.CreatedAt, msg.(*coslv1.SignRequest))
		}
		out = append(out, q)
	}
	return out, nil
}

// DropQueued erases the queued message of groupAlias whose msg_id starts
// with prefix, and returns its msg_id: it never reaches the log. Nothing
// else is dropped, not even a queued reply to it. The process serving the
// Group owns its Outbox and drops it then; otherwise the file is edited
// here, under the Outbox's lock.
func (h *Host) DropQueued(ctx context.Context, groupAlias, prefix string) (string, error) {
	if _, err := outbox.MsgIDPrefix(prefix); err != nil {
		return "", &InvalidError{Err: err}
	}
	if err := h.knownGroup(groupAlias); err != nil {
		return "", err
	}
	unlock, err := outbox.Lock(h.p.OutboxFile(groupAlias), false)
	if errors.Is(err, outbox.ErrBusy) {
		// The lock's holder serves this Group's replica: no need to ask
		// which Group it serves.
		var api servedAPI = h.socket()
		if cur := h.serving(); cur != nil && cur.group == groupAlias {
			api = cur.run
		}
		v, err := api.OutboxDrop(ctx, prefix)
		return v.MsgID, wrapServed(err)
	}
	if err != nil {
		return "", err
	}
	defer unlock()
	store, err := h.replica(groupAlias)
	if err != nil {
		return "", err
	}
	defer store.Close()
	ob, err := outbox.Open(outbox.Config{Path: h.p.OutboxFile(groupAlias)})
	if err != nil {
		return "", err
	}
	s, err := outbox.Withdraw(ob, store, prefix)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(s.MsgID[:]), nil
}
