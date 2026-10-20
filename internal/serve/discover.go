package serve

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/group"
	groupv1 "defios.com/proto/group/v1"
)

const (
	identityFetchTimeout = 5 * time.Second
	identityMaxBytes     = 64 << 10
)

// discover fetches and verifies every reachable peer's Identity (§6.1).
// Unreachable peers are skipped; an Identity whose Node ID is not derived
// from its auth public key is rejected and not listed (§11.3).
func (s *server) discover(ctx context.Context) ([]group.RosterNode, error) {
	peers, err := s.opts.Tailnet.Peers(ctx)
	if err != nil {
		return nil, fmt.Errorf("serve: listing peers: %w", err)
	}
	client := s.opts.Tailnet.HTTPClient()

	var mu sync.Mutex
	var found []group.RosterNode
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func(p Peer) {
			defer wg.Done()
			n, err := fetchIdentity(ctx, client, p)
			if err != nil {
				s.opts.Logf("serve: peer %s (%s): %v", p.Hostname, p.IP, err)
				return
			}
			mu.Lock()
			found = append(found, n)
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	slices.SortFunc(found, func(a, b group.RosterNode) int { return bytes.Compare(a.NodeID[:], b.NodeID[:]) })
	return found, nil
}

func fetchIdentity(ctx context.Context, client *http.Client, p Peer) (group.RosterNode, error) {
	ctx, cancel := context.WithTimeout(ctx, identityFetchTimeout)
	defer cancel()
	url := "http://" + net.JoinHostPort(p.IP, strconv.Itoa(FormationPort)) + "/v1/identity"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return group.RosterNode{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return group.RosterNode{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return group.RosterNode{}, fmt.Errorf("identity: %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, identityMaxBytes))
	if err != nil {
		return group.RosterNode{}, err
	}
	var id groupv1.Identity
	if err := proto.Unmarshal(raw, &id); err != nil {
		return group.RosterNode{}, fmt.Errorf("identity: %w", err)
	}
	return group.VerifyIdentity(&id)
}
