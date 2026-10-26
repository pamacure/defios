package serve_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/model"
	coslv1 "defios.com/proto/cosl/v1"
)

// Model configs written on one Node fold identically on both; a write the
// fold would skip is refused with the model sentinel over the socket, and a
// removed alias may be added again.
func TestModelAddEditRemove(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	ctx := context.Background()

	opus := &coslv1.ModelConfig{Alias: "agent", Provider: "anthropic", Model: "claude-opus-5-5"}
	if _, err := a.client().ModelAdd(ctx, opus); err != nil {
		t.Fatal(err)
	}
	waitModels(t, b, func(ms []model.Model) bool { return len(ms) == 1 && ms[0].ModelID == "claude-opus-5-5" })

	if _, err := b.client().ModelAdd(ctx, opus); !errors.Is(err, model.ErrExists) {
		t.Fatalf("add of live alias: %v", err)
	}
	if _, err := b.client().ModelEdit(ctx, &coslv1.ModelConfig{Alias: "ghost", Provider: "anthropic", Model: "m"}); !errors.Is(err, model.ErrUnknown) {
		t.Fatalf("edit of absent alias: %v", err)
	}
	if _, err := b.client().ModelEdit(ctx, &coslv1.ModelConfig{Alias: "agent", Provider: "openai", Model: "gpt-5"}); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("provider change: %v", err)
	}
	if _, err := b.client().ModelAdd(ctx, &coslv1.ModelConfig{Alias: "x", Provider: "anthropic"}); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("invalid config: %v", err)
	}
	if _, err := b.client().ModelRemove(ctx, "ghost"); !errors.Is(err, model.ErrUnknown) {
		t.Fatalf("remove of absent alias: %v", err)
	}

	edited := &coslv1.ModelConfig{Alias: "agent", Provider: "anthropic", Model: "claude-sonnet-5-5", BaseUrl: "https://llm.example", ApiPath: "/v1/messages"}
	if _, err := b.client().ModelEdit(ctx, edited); err != nil {
		t.Fatal(err)
	}
	waitModels(t, a, func(ms []model.Model) bool {
		return len(ms) == 1 && ms[0].ModelID == "claude-sonnet-5-5" && ms[0].BaseURL == "https://llm.example" && ms[0].APIPath == "/v1/messages"
	})

	if _, err := a.client().ModelRemove(ctx, "agent"); err != nil {
		t.Fatal(err)
	}
	waitModels(t, b, func(ms []model.Model) bool { return len(ms) == 0 })
	gpt := &coslv1.ModelConfig{Alias: "agent", Provider: "openai", Model: "gpt-5"}
	if _, err := b.client().ModelAdd(ctx, gpt); err != nil {
		t.Fatalf("re-add after remove: %v", err)
	}
	waitModels(t, a, func(ms []model.Model) bool { return len(ms) == 1 && ms[0].Provider == "openai" })
}

func waitModels(t *testing.T, n *node, ok func([]model.Model) bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last []model.Model
	for time.Now().Before(deadline) {
		store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
		if err == nil {
			rows, err := store.Log(cosl.Query{})
			store.Close()
			if ms, ferr := model.Fold(rows); err == nil && ferr == nil {
				if ok(ms) {
					return
				}
				last = ms
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: models never matched; last %+v", n.name, last)
}
