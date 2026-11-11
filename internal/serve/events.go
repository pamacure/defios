package serve

import (
	"context"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/outbox"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
	"defios.com/internal/talk"
)

// tellCommits tells Options.OnTalk each app.freeform the replica applies
// after it starts, Options.OnSession each Session whose status that moves,
// and Options.OnSignRequest each Sign Request it moves, or whose deadline
// passes, until ctx ends. A restore, which rewrites the replica, tells none
// of the talk it brings, only the Sessions and Sign Requests it moves.
func (s *server) tellCommits(ctx context.Context, store *cosl.Store, ob *outbox.Outbox) {
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
	signs, deadline, err := s.signStates(store, ob)
	if err != nil {
		s.opts.Logf("serve: events: %v", err)
	}
	watched := s.opts.OnSession != nil || s.opts.OnSignRequest != nil
	f := store.Follow(cosl.FallbackTick)
	tick := time.NewTicker(cosl.FallbackTick)
	defer tick.Stop()
	expiry := time.NewTimer(time.Hour)
	defer expiry.Stop()
	// tellSigns tells OnSignRequest each Sign Request moved since it last
	// looked.
	tellSigns := func() error {
		if s.opts.OnSignRequest == nil {
			return nil
		}
		now, next, err := s.signStates(store, ob)
		if err != nil {
			return err
		}
		for id, st := range now {
			if was, ok := signs[id]; !ok || !was.same(st) {
				s.opts.OnSignRequest(st.Summary)
			}
		}
		signs, deadline = now, next
		return nil
	}
	nudged := false
	for {
		toldSigns := false
		err := f.Act(func(wk cosl.Wake) error {
			// reread: the Sessions and Sign Requests may have moved.
			reread := wk.Mark.Restores != restores
			restores = wk.Mark.Restores
			if reread {
				last = wk.Mark.LastAppliedSeq
			} else {
				told, err := s.tellTalk(store, last)
				if err != nil {
					return err
				}
				if watched {
					rows, err := store.Log(cosl.Query{MsgTypes: signreq.MsgTypes, AfterSeq: last, Limit: 1})
					if err != nil {
						return err
					}
					reread = len(rows) > 0
				}
				last = max(last, told, wk.Mark.LastAppliedSeq)
			}
			if !reread || !watched {
				return nil
			}
			if s.opts.OnSession != nil {
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
			}
			toldSigns = true
			return tellSigns()
		})
		// A deadline passing, or this Node's stance queued in its Outbox,
		// moves a request with nothing on the log.
		if err == nil && !toldSigns && (nudged || !deadline.IsZero() && !time.Now().Before(deadline)) {
			if err = tellSigns(); err != nil && !deadline.IsZero() {
				deadline = time.Now().Add(time.Second) // retry, not spin
			}
		}
		nudged = false
		if err != nil {
			s.opts.Logf("serve: events: %v", err)
		}
		expiry.Stop() // since Go 1.23, a stopped timer delivers nothing stale
		if !deadline.IsZero() {
			// A moment past it, so the status has turned when it is read.
			expiry.Reset(max(time.Until(deadline), 0) + 10*time.Millisecond)
		}
		select {
		case <-ctx.Done():
			return
		case <-f.Changes():
		case <-tick.C:
		case <-expiry.C:
		case <-s.signNudge:
			nudged = true
		}
	}
}

// signState is a Sign Request as OnSignRequest last told it.
type signState struct{ signreq.Summary }

// same reports whether nothing OnSignRequest tells of moved from s to o.
func (s signState) same(o signState) bool {
	return s.Status == o.Status && s.Stance == o.Stance && s.Session == o.Session && s.Broadcasts == o.Broadcasts &&
		s.FullyBroadcast == o.FullyBroadcast && len(s.Accepts) == len(o.Accepts) && len(s.Refusals) == len(o.Refusals)
}

// signStates is every Sign Request the replica holds, by msg id, as `sign
// list` shows it with ob, and the next deadline of one open or waiting;
// zero for none. It reads what it is given, not the server's current
// replica: stopReplica holds the server lock waiting for it.
func (s *server) signStates(store *cosl.Store, ob *outbox.Outbox) (map[string]signState, time.Time, error) {
	if s.opts.OnSignRequest == nil {
		return nil, time.Time{}, nil
	}
	rows, err := store.Log(cosl.Query{MsgTypes: signreq.MsgTypes})
	if err != nil {
		return nil, time.Time{}, err
	}
	list, err := s.signSummaries(rows, ob)
	if err != nil {
		return nil, time.Time{}, err
	}
	out := make(map[string]signState, len(list))
	for _, sum := range list {
		out[sum.MsgID] = signState{sum}
	}
	return out, nextDeadline(rows, list), nil
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
