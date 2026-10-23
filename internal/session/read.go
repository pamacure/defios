package session

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"fmt"
	"slices"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
)

// Summary is one `session list` row.
type Summary struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Curve     string `json:"curve"`
	T         uint32 `json:"t"`
	N         uint32 `json:"n"`
	// SignRequest is the msg_id of the sign.request a signing Session signs.
	SignRequest string `json:"sign_request,omitempty"`
}

// Completion is one participant's session.complete, as `session inspect` shows it.
type Completion struct {
	Sender    string `json:"sender"`
	Curve     string `json:"curve"`
	PublicKey string `json:"public_key"`
	// Signatures are a signing Session's r||s||v hex, one per Digest.
	Signatures []string `json:"signatures,omitempty"`
}

// Detail is `session inspect`: the summary plus participants and completes seen.
type Detail struct {
	Summary
	Participants []string     `json:"participants"`
	Completes    []Completion `json:"completes"`
	Reason       string       `json:"reason,omitempty"`
}

// ReadOption tunes how Sessions are read off replica rows.
type ReadOption func(*verifier)

// WithSignInfo makes a signing Session complete only on a session.complete
// whose signatures verify against the Key and Digests lookup resolves. Without
// it, readers trust a complete row.
func WithSignInfo(lookup func(requestID [32]byte) (SignInfo, error)) ReadOption {
	return func(v *verifier) {
		*v = func(r *rec, c seen) bool {
			info, err := lookup(r.signReq)
			return err == nil && info.verifies(c.signatures)
		}
	}
}

func verifierOf(opts []ReadOption) verifier {
	var v verifier
	for _, o := range opts {
		o(&v)
	}
	return v
}

// List reads Sessions off replica rows, in start order.
func List(rows []cosl.Row, opts ...ReadOption) ([]Summary, error) {
	recs, err := parse(rows, verifierOf(opts))
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.summary())
	}
	return out, nil
}

// Inspect reads one Session. ErrUnknownSession when id never started.
func Inspect(rows []cosl.Row, id ID, opts ...ReadOption) (Detail, error) {
	recs, err := parse(rows, verifierOf(opts))
	if err != nil {
		return Detail{}, err
	}
	r := find(recs, id)
	if r == nil {
		return Detail{}, ErrUnknownSession
	}
	return r.detail(), nil
}

// rec is one Session reconstructed from the replica.
type rec struct {
	id           ID
	sender       identity.ID
	createdAt    int64
	startSeq     uint64
	curve        string
	t, n         uint32
	participants []identity.ID
	boxes        []*coslv1.SealedBox
	isSign       bool // a signing Session; signReq is the sign.request's msg_id
	signReq      [32]byte
	abortAt      []uint64
	abortReason  string
	completes    []seen
	completeSeq  uint64
	agreed       bool
	conflict     bool
	rounds       []round
	status       string
	reason       string
	valid        bool
	invalid      string
}

type seen struct {
	seq        uint64
	sender     identity.ID
	curve      string
	publicKey  []byte
	signatures [][]byte
}

type round struct {
	seq    uint64
	sender identity.ID
	msg    *coslv1.TssSession
}

func (r *rec) summary() Summary {
	sum := Summary{SessionID: r.id.String(), Status: r.status, Curve: r.curve, T: r.t, N: r.n}
	if r.isSign {
		sum.SignRequest = hex.EncodeToString(r.signReq[:])
	}
	return sum
}

func (r *rec) detail() Detail {
	parts := make([]string, len(r.participants))
	for i, id := range r.participants {
		parts[i] = id.String()
	}
	completes := make([]Completion, len(r.completes))
	for i, c := range r.completes {
		completes[i] = Completion{
			Sender:    c.sender.String(),
			Curve:     c.curve,
			PublicKey: hex.EncodeToString(c.publicKey),
		}
		for _, sig := range c.signatures {
			completes[i].Signatures = append(completes[i].Signatures, hex.EncodeToString(sig))
		}
	}
	return Detail{Summary: r.summary(), Participants: parts, Completes: completes, Reason: r.reason}
}

// verifier says whether a signing Session's session.complete carries valid
// signatures. Nil trusts the row, as the read-only listings do.
type verifier func(r *rec, c seen) bool

func parse(rows []cosl.Row, verify verifier) ([]*rec, error) {
	var order []ID
	by := map[ID]*rec{}
	for _, row := range rows {
		env, err := row.Decode()
		if err != nil {
			return nil, err
		}
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			continue // unknown or undecodable; readers skip
		}
		switch p := msg.(type) {
		case *coslv1.SessionStart:
			id, ok := idBytes(p.SessionId)
			if !ok {
				continue
			}
			if _, exists := by[id]; exists {
				continue
			}
			r := &rec{
				id:        id,
				sender:    row.Sender,
				createdAt: row.CreatedAt,
				startSeq:  row.Seq,
				curve:     p.Curve,
				t:         p.T,
				n:         p.N,
				boxes:     p.NonceBoxes,
				status:    Open,
			}
			if req, ok := idBytes(p.SignRequestId); ok {
				r.isSign, r.signReq = true, req
			}
			for _, raw := range p.Participants {
				pid, ok := idBytes(raw)
				if !ok {
					continue
				}
				r.participants = append(r.participants, identity.ID(pid))
			}
			by[id] = r
			order = append(order, id)
		case *coslv1.SessionAbort:
			id, ok := idBytes(p.SessionId)
			if !ok {
				continue
			}
			r := by[id]
			if r == nil {
				continue
			}
			if r.abortReason == "" {
				r.abortReason = p.Reason
			}
			r.abortAt = append(r.abortAt, row.Seq)
		case *coslv1.SessionComplete:
			id, ok := idBytes(p.SessionId)
			if !ok {
				continue
			}
			r := by[id]
			if r == nil {
				continue
			}
			r.completes = append(r.completes, seen{
				seq: row.Seq, sender: row.Sender, curve: p.Curve, publicKey: append([]byte(nil), p.PublicKey...),
				signatures: cloneAll(p.Signatures),
			})
		case *coslv1.TssSession:
			id, ok := idBytes(p.SessionId)
			if !ok {
				continue
			}
			r := by[id]
			if r == nil {
				continue
			}
			r.rounds = append(r.rounds, round{seq: row.Seq, sender: row.Sender, msg: p})
		}
	}
	out := make([]*rec, 0, len(order))
	for _, id := range order {
		r := by[id]
		r.recompute(verify)
		out = append(out, r)
	}
	return out, nil
}

func (r *rec) recompute(verify verifier) {
	if r.isSign {
		r.agreed, r.completeSeq = signed(r, verify)
	} else {
		r.agreed, r.completeSeq, r.conflict = agreement(r)
	}
	switch {
	case r.agreed:
		r.status = Complete
	case len(r.abortAt) > 0:
		r.status = Aborted
		r.reason = r.abortReason
	default:
		r.status = Open
	}
}

// signed reports whether a signer has completed with valid signatures. A
// signature proves itself, so the first one that verifies completes the
// Session: no n-of-n agreement (docs/adr/0010-signing-session-semantics.md).
func signed(r *rec, verify verifier) (bool, uint64) {
	for _, c := range r.completes {
		if !slices.Contains(r.participants, c.sender) || len(c.signatures) == 0 {
			continue
		}
		if verify != nil && !verify(r, c) {
			continue
		}
		return true, c.seq
	}
	return false, 0
}

func cloneAll(in [][]byte) [][]byte {
	out := make([][]byte, len(in))
	for i, b := range in {
		out[i] = append([]byte(nil), b...)
	}
	return out
}

// agreement reports whether n distinct participants have completed with
// the same curve and public key as each other and as the start. conflict
// is a complete that disagrees; the Session is not a Key.
func agreement(r *rec) (agreed bool, at uint64, conflict bool) {
	if r.n == 0 || len(r.completes) == 0 {
		return false, 0, false
	}
	var curve string
	var pk []byte
	seen := map[identity.ID]struct{}{}
	for _, c := range r.completes {
		if c.curve != r.curve {
			return false, 0, true
		}
		if pk == nil {
			curve, pk = c.curve, c.publicKey
		} else if c.curve != curve || !bytes.Equal(c.publicKey, pk) {
			return false, 0, true
		}
		if _, ok := seen[c.sender]; ok {
			continue
		}
		seen[c.sender] = struct{}{}
		if len(seen) == int(r.n) {
			at = c.seq
		}
	}
	return len(seen) == int(r.n), at, false
}

// Completed is a Key the replica has minted: n session.complete rows agree
// on curve and canonical public key.
type Completed struct {
	SessionID ID
	Curve     string
	PublicKey []byte
	T, N      int
}

// CompletedSessions reads minted Keys off the replica. There is no keys table.
func CompletedSessions(rows []cosl.Row) ([]Completed, error) {
	recs, err := parse(rows, nil)
	if err != nil {
		return nil, err
	}
	var out []Completed
	for _, r := range recs {
		if r.isSign || r.status != Complete || len(r.completes) == 0 {
			continue
		}
		out = append(out, Completed{
			SessionID: r.id,
			Curve:     r.curve,
			PublicKey: append([]byte(nil), r.completes[0].publicKey...),
			T:         int(r.t),
			N:         int(r.n),
		})
	}
	return out, nil
}

// validate is Key policy (§7.1), not a COSL commit rule. An invalid start
// still occupies the one-open slot until it is aborted.
func (r *rec) validate(roster group.Roster) {
	ids := roster.NodeIDs()
	n := len(ids)
	switch {
	case r.curve != "ecdsa" && r.curve != "eddsa":
		r.invalid = fmt.Sprintf("curve %q is not ecdsa or eddsa", r.curve)
	case int(r.n) != n || len(r.participants) != n:
		r.invalid = "participants are not the roster"
	case !(2*int(r.t) > n && int(r.t) <= n):
		r.invalid = fmt.Sprintf("t=%d is not valid for n=%d", r.t, n)
	default:
		for i, id := range ids {
			if r.participants[i] != id {
				r.invalid = "participants are not the roster"
				return
			}
		}
		if !boxesMatch(r.boxes, ids) {
			r.invalid = "nonce boxes do not match the roster"
			return
		}
		r.valid = true
	}
}

func boxesMatch(boxes []*coslv1.SealedBox, ids []identity.ID) bool {
	if len(boxes) != len(ids) {
		return false
	}
	seen := map[identity.ID]struct{}{}
	for _, b := range boxes {
		id, ok := idBytes(b.To)
		if !ok {
			return false
		}
		if len(b.EphemeralPublicKey) != 32 || len(b.Nonce) != 24 || len(b.Ciphertext) == 0 {
			return false
		}
		seen[identity.ID(id)] = struct{}{}
	}
	if len(seen) != len(ids) {
		return false
	}
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			return false
		}
	}
	return true
}

// plan walks start, abort, and completing rows in seq order. runnable is
// the one open valid Session that may drive an engine. need is every still-
// open Session the layer must abort, with the reason.
func plan(recs []*rec) (runnable *ID, need map[ID]string) {
	need = map[ID]string{}
	type ev struct {
		seq  uint64
		kind int // 1 start, 2 abort, 3 complete
		id   ID
	}
	var evs []ev
	by := map[ID]*rec{}
	for _, r := range recs {
		by[r.id] = r
		evs = append(evs, ev{r.startSeq, 1, r.id})
		for _, seq := range r.abortAt {
			evs = append(evs, ev{seq, 2, r.id})
		}
		if r.agreed {
			evs = append(evs, ev{r.completeSeq, 3, r.id})
		}
	}
	slices.SortStableFunc(evs, func(a, b ev) int { return cmp.Compare(a.seq, b.seq) })

	var slot ID
	var have bool
	for _, e := range evs {
		switch e.kind {
		case 1:
			r := by[e.id]
			switch {
			case have:
				need[e.id] = "another session is open"
			case !r.valid:
				need[e.id] = r.invalid
				slot, have = e.id, true
			default:
				slot, have = e.id, true
			}
		case 2, 3:
			delete(need, e.id)
			if have && slot == e.id {
				have = false
			}
		}
	}
	if have && by[slot].valid && need[slot] == "" {
		id := slot
		runnable = &id
	}
	return runnable, need
}

func find(recs []*rec, id ID) *rec {
	for _, r := range recs {
		if r.id == id {
			return r
		}
	}
	return nil
}

func idBytes(b []byte) (ID, bool) {
	if len(b) != 32 {
		return ID{}, false
	}
	var id ID
	copy(id[:], b)
	return id, true
}
