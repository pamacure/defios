package serve_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/provider"
	coslv1 "defios.com/proto/cosl/v1"
)

// Provider configs written on one Node fold identically on both; a write
// the fold would skip is refused with the provider sentinel over the
// socket, an edit may change both fields, and a removed alias may be added
// again.
func TestProviderAddEditRemove(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	ctx := context.Background()

	anth := &coslv1.ProviderConfig{Alias: "agent", BaseUrl: "https://api.anthropic.com", ApiPath: "/v1/messages"}
	if _, err := a.client().ProviderAdd(ctx, anth); err != nil {
		t.Fatal(err)
	}
	waitProviders(t, b, func(ps []provider.Provider) bool { return len(ps) == 1 && ps[0].BaseURL == "https://api.anthropic.com" })

	if _, err := b.client().ProviderAdd(ctx, anth); !errors.Is(err, provider.ErrExists) {
		t.Fatalf("add of live alias: %v", err)
	}
	if _, err := b.client().ProviderEdit(ctx, &coslv1.ProviderConfig{Alias: "ghost", BaseUrl: "https://x.example", ApiPath: "/v1/messages"}); !errors.Is(err, provider.ErrUnknown) {
		t.Fatalf("edit of absent alias: %v", err)
	}
	if _, err := b.client().ProviderAdd(ctx, &coslv1.ProviderConfig{Alias: "x", BaseUrl: "https://x.example", ApiPath: "/v1/foo"}); !errors.Is(err, provider.ErrInvalid) {
		t.Fatalf("api_path with no shape: %v", err)
	}
	if _, err := b.client().ProviderRemove(ctx, "ghost"); !errors.Is(err, provider.ErrUnknown) {
		t.Fatalf("remove of absent alias: %v", err)
	}

	edited := &coslv1.ProviderConfig{Alias: "agent", BaseUrl: "http://127.0.0.1:8080", ApiPath: "/v1/chat/completions"}
	if _, err := b.client().ProviderEdit(ctx, edited); err != nil {
		t.Fatal(err)
	}
	waitProviders(t, a, func(ps []provider.Provider) bool {
		return len(ps) == 1 && ps[0].BaseURL == "http://127.0.0.1:8080" && ps[0].APIPath == "/v1/chat/completions"
	})

	if _, err := a.client().ProviderRemove(ctx, "agent"); err != nil {
		t.Fatal(err)
	}
	waitProviders(t, b, func(ps []provider.Provider) bool { return len(ps) == 0 })
	if _, err := b.client().ProviderAdd(ctx, anth); err != nil {
		t.Fatalf("re-add after remove: %v", err)
	}
	waitProviders(t, a, func(ps []provider.Provider) bool { return len(ps) == 1 && ps[0].APIPath == "/v1/messages" })
}

func waitProviders(t *testing.T, n *node, ok func([]provider.Provider) bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last []provider.Provider
	for time.Now().Before(deadline) {
		store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
		if err == nil {
			rows, err := store.Log(cosl.Query{})
			store.Close()
			if ps, ferr := provider.Fold(rows); err == nil && ferr == nil {
				if ok(ps) {
					return
				}
				last = ps
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: providers never matched; last %+v", n.name, last)
}
