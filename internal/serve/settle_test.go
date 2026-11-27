package serve_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"defios.com/internal/signreq"
)

// waitStatus waits for n to read the request msgID as status, and returns
// its Detail then.
func waitStatus(t *testing.T, n *node, msgID, status string) signreq.Detail {
	t.Helper()
	var d signreq.Detail
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		if d, err = n.client().SignInspect(context.Background(), msgID[:8]); err == nil && d.Status == status {
			return d
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("status %s, want %s; outcomes %+v, receipts %+v", d.Status, status, d.Outcomes, d.BroadcastReceipts)
	return d
}

// The t-th accept is the last step (ADR 0045): the Node that starts the
// Session signs the request with the Key's nonce read fresh from the chain,
// broadcasts it once signed, and records that it was mined.
func TestASignedRequestIsRefreshedBroadcastAndConfirmed(t *testing.T) {
	c := newChain(t)
	c.nonce, c.mines = 7, true // the proposal's nonce 0 is stale
	_, b, _, msgID, _ := signedRequestWith(t, true, "ethereum", c.srv.URL)

	d := waitStatus(t, b, msgID, signreq.Confirmed)
	if !slices.Contains(d.Details[0].Lines, "nonce 7") {
		t.Errorf("signed with %v, want nonce 7", d.Details[0].Lines)
	}
	// One Node broadcast it, the one that started the Session, and it
	// watched it to its outcome.
	if c.sends() != 1 || len(d.BroadcastReceipts) != 1 {
		t.Fatalf("%d sends, receipts %+v; want one", c.sends(), d.BroadcastReceipts)
	}
	if len(d.Outcomes) != 1 || d.Outcomes[0].Result != signreq.ResultConfirmed || d.Outcomes[0].Block != 16 ||
		d.Outcomes[0].Node != d.BroadcastReceipts[0].Broadcaster || d.Outcomes[0].TxHash != d.BroadcastReceipts[0].Transactions[0].TxHash {
		t.Errorf("outcomes %+v", d.Outcomes)
	}
	last := d.History[len(d.History)-1]
	if last.Kind != signreq.EventOutcome || last.Result != signreq.ResultConfirmed {
		t.Errorf("history ends %+v", last)
	}
}

// A mined transaction that reverted fails its request, and broadcasting it
// again cannot help.
func TestARevertedTransactionFailsTheRequest(t *testing.T) {
	c := newChain(t)
	c.mines, c.reverts = true, true
	_, b, _, msgID, _ := signedRequestWith(t, true, "ethereum", c.srv.URL)

	d := waitStatus(t, b, msgID, signreq.Failed)
	if d.Failure != signreq.ResultReverted || d.Retry {
		t.Errorf("failure %s, retry %v", d.Failure, d.Retry)
	}
}

// A Solana request is signed with the cluster's blockhash as of its start,
// and a transaction whose blockhash expires before it lands is dropped,
// which a rebroadcast may still fix.
func TestASolanaRequestIsRefreshedAndDropped(t *testing.T) {
	c := newSolanaChain(t)
	c.loses = true
	_, b, _, msgID, _ := signedRequestWith(t, true, "solana", c.srv.URL)

	d := waitStatus(t, b, msgID, signreq.Failed)
	if !slices.Contains(d.Details[0].Lines, "recent_blockhash "+c.blockhash) {
		t.Errorf("signed with %v, want the cluster's blockhash %s", d.Details[0].Lines, c.blockhash)
	}
	if d.Failure != signreq.ResultDropped || !d.Retry || !strings.Contains(d.Outcomes[0].Detail, "expired") {
		t.Errorf("failure %s, retry %v, outcomes %+v", d.Failure, d.Retry, d.Outcomes)
	}
	if d.DeadlineSeconds != signreq.DefaultDeadlineSeconds {
		t.Errorf("deadline %ds, want the default", d.DeadlineSeconds)
	}
}
