package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"defios.com/internal/network"
	"defios.com/internal/signreq"
	coslv1 "defios.com/proto/cosl/v1"
)

// ethNode answers what an Ethereum watcher reads: a receipt in block
// mined (none when 0), the head, and the Key's mined nonce.
func ethNode(t *testing.T, mined, head, nonce uint64) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "eth_chainId":
			resp["result"] = "0x1"
		case "eth_getTransactionReceipt":
			if mined == 0 {
				resp["result"] = nil
			} else {
				resp["result"] = map[string]any{"status": "0x1", "blockNumber": fmt.Sprintf("0x%x", mined)}
			}
		case "eth_blockNumber":
			resp["result"] = fmt.Sprintf("0x%x", head)
		case "eth_getTransactionCount":
			resp["result"] = fmt.Sprintf("0x%x", nonce)
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestTheEthereumWatcher(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	eth := func(url string, depth uint64, since time.Duration) watcher {
		n := network.Network{Alias: "eth", Type: network.Ethereum, ChainID: 1, Confirmations: depth}
		return watcher{
			urls: func() ([]string, error) { return []string{url}, nil }, since: now.Add(-since), now: func() time.Time { return now },
			net: n, chain: n.Chain(), id: make([]byte, 32), sender: "0x00",
			tx: &coslv1.SignTransaction{Intent: &coslv1.SignTransaction_Ethereum{Ethereum: &coslv1.EthereumIntent{Nonce: 5}}},
		}
	}
	for name, c := range map[string]struct {
		w      watcher
		result string // "" for pending
	}{
		"mined, default depth":        {eth(ethNode(t, 10, 10, 6), 0, time.Minute), signreq.ResultConfirmed},
		"mined, not yet 3 deep":       {eth(ethNode(t, 10, 11, 6), 3, time.Minute), ""},
		"mined, 3 deep":               {eth(ethNode(t, 10, 12, 6), 3, time.Minute), signreq.ResultConfirmed},
		"pending":                     {eth(ethNode(t, 0, 12, 5), 0, time.Minute), ""},
		"its nonce used by another":   {eth(ethNode(t, 0, 12, 6), 0, time.Minute), signreq.ResultDropped},
		"not mined within 10 minutes": {eth(ethNode(t, 0, 12, 5), 0, 11*time.Minute), signreq.ResultDropped},
	} {
		o, err := c.w.check(context.Background())
		switch {
		case c.result == "" && !errors.Is(err, errPending):
			t.Errorf("%s: %v, %v; want pending", name, o, err)
		case c.result != "" && (err != nil || o.Result != c.result):
			t.Errorf("%s: %v, %v; want %s", name, o, err, c.result)
		}
	}
}
