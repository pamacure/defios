package serve

import (
	"context"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
	"defios.com/internal/talk"
)

// tellCommits tells Options.OnTalk each app.freeform the replica applies
// after it starts, and Options.OnSession each Session whose status that
// moves, until ctx ends. A restore, which rewrites the replica, tells none
// of the talk it brings, only the Sessions it moves.
func (s *server) tellCommits(ctx context.Context, store *cosl.Store) {
	m, err := store.Mark()
	if err != nil {
		s.opts.Logf("serve: events: %v", err)
		return
	}
	last, restores := m.LastAppliedSeq, m.Restores
	list, err := sessionStatuses(store)
	if err != nil {
		s.opts.Logf("serve: events: %v", err)
	}
	statuses := byID(list)
	f := store.Follow(cosl.FallbackTick)
	tick := time.NewTicker(cosl.FallbackTick)
	defer tick.Stop()
	for {
		err := f.Act(func(wk cosl.Wake) error {
			// reread: the Sessions may have moved.
			reread := wk.Mark.Restores != restores
			restores = wk.Mark.Restores
			if reread {
				last = wk.Mark.LastAppliedSeq
			} else {
				told, err := s.tellTalk(store, last)
				if err != nil {
					return err
				}
				if s.opts.OnSession != nil {
					rows, err := store.Log(cosl.Query{MsgTypes: signreq.LookupMsgTypes, AfterSeq: last, Limit: 1})
					if err != nil {
						return err
					}
					reread = len(rows) > 0
				}
				last = max(last, told, wk.Mark.LastAppliedSeq)
			}
			if !reread || s.opts.OnSession == nil {
				return nil
			}
			now, err := sessionStatuses(store)
			if err != nil {
				return err
			}
			for _, sum := range now {
				if was, ok := statuses[sum.SessionID]; !ok || was != sum.Status {
					s.opts.OnSession(sum)
				}
			}
			statuses = byID(now)
			return nil
		})
		if err != nil {
			s.opts.Logf("serve: events: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-f.Changes():
		case <-tick.C:
		}
	}
}

// tellTalk tells OnTalk the app.freeform rows after seq last, and returns
// the highest seq it told, last if none.
func (s *server) tellTalk(store *cosl.Store, last uint64) (uint64, error) {
	if s.opts.OnTalk == nil {
		return last, nil
	}
	rows, err := store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeAppFreeform}, AfterSeq: last})
	if err != nil {
		return last, err
	}
	rec, _ := s.slot.Current()
	roster := rec.Roster()
	for _, row := range rows {
		m, err := talk.Decode(row, &roster)
		if err != nil {
			return last, err
		}
		s.opts.OnTalk(row.ConversationID, m)
		last = row.Seq
	}
	return last, nil
}

// sessionStatuses are the replica's Sessions, as `session list` reads them.
func sessionStatuses(store *cosl.Store) ([]session.Summary, error) {
	rows, err := store.Log(cosl.Query{MsgTypes: signreq.LookupMsgTypes})
	if err != nil {
		return nil, err
	}
	return session.List(rows, SignInfoOf(rows))
}

// byID is each Session's status by its id.
func byID(list []session.Summary) map[string]string {
	out := make(map[string]string, len(list))
	for _, sum := range list {
		out[sum.SessionID] = sum.Status
	}
	return out
}

// SignInfoOf lets a reader of rows, which hold signreq.LookupMsgTypes,
// verify a signing Session's completes against the Key and Digests its
// sign.request implies, as the Session layer does.
func SignInfoOf(rows []cosl.Row) session.ReadOption {
	return session.WithSignInfo(func(id [32]byte) (session.SignInfo, error) {
		sg, err := signreq.Lookup(rows, id)
		if err != nil {
			return session.SignInfo{}, err
		}
		return session.SignInfo{Curve: sg.Curve, PublicKey: sg.PublicKey, T: sg.T, Digests: sg.Digests, Deadline: sg.Deadline}, nil
	})
}
