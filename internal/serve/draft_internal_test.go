package serve

import (
	"testing"
	"time"

	"defios.com/internal/signreq"
)

// A Draft is its own Conversation's, for draftTTL: another Conversation's
// id, or an expired one, is no Draft. A released Draft is usable again.
func TestADraftIsItsConversationsForItsTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	d := drafts{now: func() time.Time { return now }}
	mine, theirs := []byte("mine"), []byte("theirs")
	id := d.put(mine, []signreq.TxSpec{{Network: "eth"}})
	if _, _, err := d.take(theirs, id); err == nil {
		t.Error("another Conversation took the Draft")
	}
	txs, release, err := d.take(mine, id)
	if err != nil || len(txs) != 1 || txs[0].Network != "eth" {
		t.Fatalf("take = %v, %v", txs, err)
	}
	if _, _, err := d.take(mine, id); err == nil {
		t.Error("a taken Draft was taken again")
	}
	release()
	now = now.Add(draftTTL - time.Second)
	if _, release, err = d.take(mine, id); err != nil {
		t.Fatalf("released, then take = %v", err)
	}
	release()
	now = now.Add(time.Second)
	if _, _, err := d.take(mine, id); err == nil {
		t.Error("an expired Draft was taken")
	}
}
