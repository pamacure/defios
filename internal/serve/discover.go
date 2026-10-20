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
	"defios.com/internal/identity"
	groupv1 "defios.com/proto/group/v1"
)

const (
	identityFetchTimeout = 5 * time.Second
	identityMaxBytes     = 64 << 10
)

// discovered is one tailnet peer whose Identity verified: the Roster view
// of it, the signed wire Identity a proposal carries, and where it was.
type discovered struct {
	group.RosterNode
	identity *groupv1.Identity
	peer     Peer
}

// discover fetches the Identity of every reachable tailnet peer (§6.1) and
// keeps the Nodes that verify. Unreachable peers are skipped; an Identity
// whose Node ID is not derived from its auth public key is rejected and not
// listed (§11.3).
func (s *server) discover(ctx context.Context) ([]discovered, error) {
	peers, err := s.opts.Tailnet.Peers(ctx)
	if err != nil {
		return nil, fmt.Errorf("serve: listing peers: %w", err)
	}
	client := s.opts.Tailnet.HTTPClient()

	var mu sync.Mutex
	var found []discovered
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func(p Peer) {
			defer wg.Done()
			d, err := fetchIdentity(ctx, client, p)
			if err != nil {
				s.opts.Logf("serve: peer %s (%s): %v", p.Hostname, p.IP, err)
				return
			}
			mu.Lock()
			found = append(found, d)
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	slices.SortFunc(found, func(a, b discovered) int { return bytes.Compare(a.NodeID[:], b.NodeID[:]) })
	return found, nil
}

// locate discovers and indexes by Node ID.
func (s *server) locate(ctx context.Context) (map[identity.ID]discovered, error) {
	found, err := s.discover(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[identity.ID]discovered, len(found))
	for _, d := range found {
		byID[d.NodeID] = d
	}
	return byID, nil
}

func fetchIdentity(ctx context.Context, client *http.Client, p Peer) (discovered, error) {
	ctx, cancel := context.WithTimeout(ctx, identityFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, formationURL(p, "/v1/identity"), nil)
	if err != nil {
		return discovered{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return discovered{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return discovered{}, fmt.Errorf("identity: %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, identityMaxBytes))
	if err != nil {
		return discovered{}, err
	}
	id := new(groupv1.Identity)
	if err := proto.Unmarshal(raw, id); err != nil {
		return discovered{}, fmt.Errorf("identity: %w", err)
	}
	n, err := group.VerifyIdentity(id)
	if err != nil {
		return discovered{}, err
	}
	return discovered{RosterNode: n, identity: id, peer: p}, nil
}

// formationURL addresses path on p's formation port.
func formationURL(p Peer, path string) string {
	return "http://" + net.JoinHostPort(p.IP, strconv.Itoa(FormationPort)) + path
}
