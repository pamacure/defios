package serve

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"defios.com/internal/signreq"
)

// draftTTL is how long a Draft lasts (ADR 0043): fees, and a Solana
// blockhash, go stale within minutes.
const draftTTL = 10 * time.Minute

// drafts are the transactions chain_fill completed, held in memory under a
// short random id for the Conversation, and so the Key, they were filled
// for. A serve restart loses them; the agent fills again.
type drafts struct {
	mu  sync.Mutex
	m   map[string]draft
	now func() time.Time // time.Now when nil
}

type draft struct {
	conv    []byte
	txs     []signreq.TxSpec
	expires time.Time
}

func (d *drafts) clock() time.Time {
	if d.now == nil {
		return time.Now()
	}
	return d.now()
}

// put holds txs for conv and is their Draft's id.
func (d *drafts) put(conv []byte, txs []signreq.TxSpec) string {
	b := make([]byte, 6)
	rand.Read(b)
	id := hex.EncodeToString(b)
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock()
	if d.m == nil {
		d.m = map[string]draft{}
	}
	for k, v := range d.m {
		if !now.Before(v.expires) {
			delete(d.m, k)
		}
	}
	d.m[id] = draft{conv: bytes.Clone(conv), txs: txs, expires: now.Add(draftTTL)}
	return id
}

// take removes the Draft id of conv and is its transactions; release
// gives it back, as it was, for a proposal that was refused. Another
// Conversation's Draft is no Draft of conv's. While taken, it is no
// Draft, so two proposals cannot both use it.
func (d *drafts) take(conv []byte, id string) (txs []signreq.TxSpec, release func(), err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.m[id]
	if !ok || !bytes.Equal(v.conv, conv) || !d.clock().Before(v.expires) {
		return nil, nil, fmt.Errorf("no Draft %q in this Conversation: a Draft lasts %v and is used up once proposed; fill the transactions again with chain_fill", id, draftTTL)
	}
	delete(d.m, id)
	return v.txs, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.m[id] = v
	}, nil
}
