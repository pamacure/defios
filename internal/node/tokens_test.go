package node_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"defios.com/internal/node"
)

// messagesAPI is a fake Anthropic Messages API: every call streams a
// turn that ends without a say, billing 7 input tokens, 40 written to
// and 900 read from the cache, and 3 output tokens.
func messagesAPI(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range [][2]string{
			{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":7,"cache_creation_input_tokens":40,"cache_read_input_tokens":900,"output_tokens":1}}}`},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"looks fine"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`},
			{"message_stop", `{"type":"message_stop"}`},
		} {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev[0], ev[1])
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withAgents is alice, whose Group "alpha" includes the agents handles
// in its Group-level talk, each on the Provider "claude" at api, and
// bob, formed with her.
func withAgents(t *testing.T, api string, handles ...string) (alice, bob *member) {
	t.Helper()
	m := newMesh()
	ctx := context.Background()
	held := func() (string, bool, error) { return passphrase, true, nil }
	alice = joinPrepared(t, m, "alice", func(h *node.Host) {
		if _, err := h.AddProvider(ctx, "alpha", node.GroupTalk, "claude", node.ProviderEdit{
			BaseURL: ptr(api), APIPath: ptr("/v1/messages"), APIKey: ptr("$ANTHROPIC_KEY"),
		}); err != nil {
			t.Fatal(err)
		}
		if err := h.SetSecret(ctx, "alpha", "ANTHROPIC_KEY", "sk-test", held); err != nil {
			t.Fatal(err)
		}
		for _, handle := range handles {
			if _, err := h.AddAgentProfile(ctx, handle, node.ProfileEdit{
				Provider: ptr("claude"),
				Body:     map[string]any{"stream": true, "max_tokens": int64(1024)},
				Model:    ptr("claude-test"),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := h.IncludeAgent(ctx, "alpha", node.GroupTalk, handle); err != nil {
				t.Fatal(err)
			}
		}
	})
	bob = joinPrepared(t, m, "bob", func(*node.Host) {})
	f, err := alice.h.Propose(ctx, "alpha", []string{alice.id, bob.id})
	if err != nil {
		t.Fatal(err)
	}
	next(t, bob.events, proposalEvent)
	if _, err := bob.h.Accept(ctx, "alpha", f.GroupID); err != nil {
		t.Fatal(err)
	}
	for _, mb := range []*member{alice, bob} {
		until(t, mb.events, func(s node.Status) bool { return s.Leader != "" && s.MajorityReachable })
	}
	return alice, bob
}

// joinPrepared is joinWith, with prepare run on the Group "alpha" before
// it is served.
func joinPrepared(t *testing.T, m *mesh, name string, prepare func(*node.Host)) *member {
	t.Helper()
	p := create(t, name)
	h, err := node.Open(p, node.Options{NewTailnet: m.tailnet, StatusInterval: 20 * time.Millisecond, Sessions: exchangeSessions})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Lock() })
	if _, err := h.NewGroup(node.GroupSpec{Alias: "alpha", HeadscaleURL: "https://hs.example"}); err != nil {
		t.Fatal(err)
	}
	prepare(h)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mb := &member{h: h, p: p, id: h.NodeID(), events: h.Subscribe(ctx)}
	if err := h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	until(t, mb.events, func(s node.Status) bool { return s.TailnetUp })
	return mb
}

// tokensUntil waits for h's Token Usage of "alpha" to be want.
func tokensUntil(t *testing.T, h *node.Host, want node.TokenUsage) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, err := h.TokenUsage(context.Background(), "alpha")
		if err == nil && reflect.DeepEqual(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Token Usage = %#v, %v; want %#v", got, err, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Token Usage sums, per Handle, the input and output tokens of every
// model call in the Transcripts of the Chains this Node ran. Input counts
// the tokens written to and read from the cache too.
func TestAChainsTokensAddToItsHandlesTokenUsage(t *testing.T) {
	api := messagesAPI(t)
	alice, bob := withAgents(t, api.URL, "auditor", "scribe")
	ctx := context.Background()

	if got, err := alice.h.TokenUsage(ctx, "alpha"); err != nil || !reflect.DeepEqual(got, node.TokenUsage{Handles: []node.HandleTokens{}}) {
		t.Fatalf("before any Chain: %#v, %v; want none", got, err)
	}

	send(t, alice, "@auditor is this fine?")
	tokensUntil(t, alice.h, node.TokenUsage{Handles: []node.HandleTokens{
		{Handle: "auditor", Input: 947, Output: 3},
	}})

	send(t, alice, "@scribe and @auditor, again")
	tokensUntil(t, alice.h, node.TokenUsage{Handles: []node.HandleTokens{
		{Handle: "auditor", Input: 1894, Output: 6},
		{Handle: "scribe", Input: 947, Output: 3},
	}})

	// Transcripts are never replicated: bob saw the mentions but ran no
	// Chain.
	if got, err := bob.h.TokenUsage(ctx, "alpha"); err != nil || len(got.Handles) != 0 {
		t.Fatalf("bob's Token Usage: %#v, %v; want none", got, err)
	}
	if _, err := alice.h.TokenUsage(ctx, "ghost"); !errors.Is(err, node.ErrUnknownGroup) {
		t.Fatalf("ghost: %v; want ErrUnknownGroup", err)
	}
}
