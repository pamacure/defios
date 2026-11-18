package node_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"defios.com/internal/node"
)

// A Livery is set, refused when another party holds it, and cleared with
// none; the free list is every Livery no party holds.
func TestALiveryIsSetRefusedWhenTakenAndCleared(t *testing.T) {
	p := nodeDir(t)
	h, err := node.Open(p, node.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if all, err := h.Liveries(ctx); err != nil || len(all) != 0 {
		t.Fatalf("a new Node's Liveries = %v, %v; want none", all, err)
	}
	if free, err := h.FreeLiveries(ctx); err != nil || len(free) != 64 || free[0] != "blue-stud" {
		t.Fatalf("free = %v, %v; want all 64, blue-stud first", free, err)
	}

	bob := strings.Repeat("b", 64)
	if err := h.SetLivery(ctx, "node:"+bob, "cyan-arch"); err != nil {
		t.Fatal(err)
	}
	if err := h.SetLivery(ctx, "node:"+bob+"/scout", "brown-tile"); err != nil {
		t.Fatal(err)
	}
	err = h.SetLivery(ctx, "node:"+bob+"/scout", "cyan-arch")
	var taken *node.LiveryTakenError
	if !errors.As(err, &taken) || taken.Livery != "cyan-arch" || taken.Holder != "node:"+bob {
		t.Fatalf("taking bob's Livery: %v; want LiveryTakenError naming node:%s", err, bob)
	}
	// Setting a party's own Livery again is no conflict.
	if err := h.SetLivery(ctx, "node:"+bob, "cyan-arch"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][2]string{
		{"node:" + bob, "green-arch"},
		{"node:" + bob, "cyan-blob"},
		{"node:" + bob, "cyan"},
		{"node:nothex", "cyan-stud"},
		{"agent:Bad", "cyan-stud"},
		{"group:x", "cyan-stud"},
	} {
		if err := h.SetLivery(ctx, bad[0], bad[1]); !errors.As(err, new(*node.InvalidError)) {
			t.Errorf("SetLivery(%q, %q) = %v; want InvalidError", bad[0], bad[1], err)
		}
	}
	if err := h.SetLivery(ctx, "agent:ghost", "cyan-stud"); !errors.Is(err, node.ErrUnknownProfile) {
		t.Errorf("a Livery for no profile: %v; want ErrUnknownProfile", err)
	}

	all, err := h.Liveries(ctx)
	if err != nil || all["node:"+bob] != "cyan-arch" || all["node:"+bob+"/scout"] != "brown-tile" || len(all) != 2 {
		t.Fatalf("Liveries = %v, %v", all, err)
	}
	free, _ := h.FreeLiveries(ctx)
	if len(free) != 62 || slices.Contains(free, "cyan-arch") || slices.Contains(free, "brown-tile") {
		t.Fatalf("free = %v", free)
	}

	if err := h.SetLivery(ctx, "node:"+bob, "none"); err != nil {
		t.Fatal(err)
	}
	if all, _ := h.Liveries(ctx); len(all) != 1 {
		t.Fatalf("after none: %v", all)
	}
	raw, err := os.ReadFile(p.LiveriesFile())
	if err != nil || !strings.Contains(string(raw), `"node:`+bob+`/scout" = "brown-tile"`) {
		t.Fatalf("liveries.toml = %q, %v", raw, err)
	}
}

// A duplicate written by hand, or brought back by a restore, still loads;
// a value that is no Livery reads as none.
func TestADuplicateLiveryStillLoads(t *testing.T) {
	p := nodeDir(t)
	h, err := node.Open(p, node.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if err := os.WriteFile(p.LiveriesFile(), []byte(`"node:`+a+`" = "blue-arch"
"node:`+b+`" = "blue-arch"
"node:`+b+`/x" = "mauve-arch"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	all, err := h.Liveries(ctx)
	if err != nil || all["node:"+a] != "blue-arch" || all["node:"+b] != "blue-arch" || len(all) != 2 {
		t.Fatalf("Liveries = %v, %v", all, err)
	}
	if free, _ := h.FreeLiveries(ctx); len(free) != 63 {
		t.Fatalf("free = %d; want 63", len(free))
	}
}

// agent add gives a new profile a random free Livery, or none once all 64
// are taken; a profile from before Liveries has none until one is set; a
// Livery is set, and cleared, through an edit; deleting a profile frees it.
func TestAnAgentProfileCarriesItsLivery(t *testing.T) {
	p := nodeDir(t)
	h, err := node.Open(p, node.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	a, err := h.AddAgentProfile(ctx, "auditor", node.ProfileEdit{Provider: ptr("claude")})
	if err != nil {
		t.Fatal(err)
	}
	if a.Livery == "" {
		t.Fatal("agent add gave no Livery")
	}
	if all, _ := h.Liveries(ctx); all["agent:auditor"] != a.Livery {
		t.Fatalf("Liveries = %v; want agent:auditor = %s", all, a.Livery)
	}

	// A profile from before: its directory is there, liveries.toml has no entry.
	if err := os.MkdirAll(p.AgentProfileDir("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.AgentProfileFile("old"), []byte("provider = \"claude\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := h.AgentProfile(ctx, "old")
	if err != nil || old.Livery != "" {
		t.Fatalf("a profile from before = %+v, %v; want no Livery", old, err)
	}
	if _, err := h.AgentProfiles(ctx); err != nil {
		t.Fatal(err)
	}
	if old, _ := h.AgentProfile(ctx, "old"); old.Livery != "" {
		t.Fatalf("listing gave the old profile %s", old.Livery)
	}

	if _, err := h.EditAgentProfile(ctx, "old", node.ProfileEdit{Livery: ptr(a.Livery)}); !errors.As(err, new(*node.LiveryTakenError)) {
		t.Fatalf("editing to auditor's Livery: %v; want LiveryTakenError", err)
	}
	old, err = h.EditAgentProfile(ctx, "old", node.ProfileEdit{Livery: ptr("slate-round")})
	if err != nil || old.Livery != "slate-round" {
		t.Fatalf("edited = %+v, %v", old, err)
	}
	if _, err := h.EditAgentProfile(ctx, "old", node.ProfileEdit{Livery: ptr("slate-round")}); !errors.Is(err, node.ErrProfileUnchanged) {
		t.Fatalf("the same Livery again: %v; want ErrProfileUnchanged", err)
	}
	if old, err = h.EditAgentProfile(ctx, "old", node.ProfileEdit{Livery: ptr("none")}); err != nil || old.Livery != "" {
		t.Fatalf("cleared = %+v, %v", old, err)
	}

	if err := h.DeleteAgentProfile(ctx, "auditor"); err != nil {
		t.Fatal(err)
	}
	if all, _ := h.Liveries(ctx); len(all) != 0 {
		t.Fatalf("after delete: %v", all)
	}

	// Every Livery taken: a new profile gets none.
	free, _ := h.FreeLiveries(ctx)
	for i, l := range free {
		if err := h.SetLivery(ctx, fmt.Sprintf("node:%064x", i+1), l); err != nil {
			t.Fatal(err)
		}
	}
	if full, err := h.AddAgentProfile(ctx, "late", node.ProfileEdit{Provider: ptr("claude")}); err != nil || full.Livery != "" {
		t.Fatalf("added with none free = %+v, %v", full, err)
	}
}

// A Node gives each other device a Livery once it holds its Roster, and
// each other device's agent one once its first message arrives, but none
// to itself. Removing the Group frees those of devices in no Group left.
func TestDevicesAndTheirAgentsAreGivenLiveriesAndFreedWithTheirGroup(t *testing.T) {
	alice, bob := formed(t)
	ctx := context.Background()
	all, err := alice.h.Liveries(ctx)
	if err != nil || all["node:"+bob.id] == "" || all["node:"+alice.id] != "" {
		t.Fatalf("alice's Liveries once formed = %v, %v; want bob's alone", all, err)
	}
	kept := all["node:"+bob.id]
	if all, _ := alice.h.Liveries(ctx); all["node:"+bob.id] != kept {
		t.Fatalf("bob's Livery changed on a second look: %v", all)
	}

	if _, err := bob.h.AddAgentProfile(ctx, "scout", node.ProfileEdit{Provider: ptr("claude")}); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.h.IncludeAgent(ctx, "alpha", node.GroupTalk, "scout"); err != nil {
		t.Fatal(err)
	}
	said, err := bob.h.Send(ctx, "alpha", node.GroupTalk, node.Outgoing{Text: "found it", Handle: "scout", Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	applied(t, alice, said)
	if all, _ := alice.h.Liveries(ctx); all["node:"+bob.id+"/scout"] == "" {
		t.Fatalf("alice's Liveries after scout spoke = %v; want node:%s/scout", all, bob.id)
	}
	if all, _ := bob.h.Liveries(ctx); all["node:"+bob.id+"/scout"] != "" || all["agent:scout"] == "" {
		t.Fatalf("bob's Liveries = %v; want its own scout as agent:scout", all)
	}

	// Remove alpha, the one Group alice shares with bob.
	if err := alice.h.Lock(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{alice.p.GroupConfigDir("alpha"), alice.p.GroupDir("alpha")} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := alice.h.Groups(ctx); err != nil {
		t.Fatal(err)
	}
	if all, _ := alice.h.Liveries(ctx); len(all) != 0 {
		t.Fatalf("alice's Liveries with alpha gone = %v; want none", all)
	}
}

// An add or edit refused for a Livery another party holds writes nothing,
// and keeps a BRIEF.md that was there before the add.
func TestAnAddOrEditRefusedForATakenLiveryChangesNothing(t *testing.T) {
	p := nodeDir(t)
	h, err := node.Open(p, node.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := h.AddAgentProfile(ctx, "auditor", node.ProfileEdit{Provider: ptr("claude"), Livery: ptr("blue-arch")}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.AgentProfileDir("scribe"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.AgentBriefFile("scribe"), []byte("You keep notes."), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddAgentProfile(ctx, "scribe", node.ProfileEdit{Provider: ptr("claude"), Livery: ptr("blue-arch")}); !errors.As(err, new(*node.LiveryTakenError)) {
		t.Fatalf("add with auditor's Livery: %v; want LiveryTakenError", err)
	}
	if raw, err := os.ReadFile(p.AgentBriefFile("scribe")); err != nil || string(raw) != "You keep notes." {
		t.Fatalf("BRIEF.md after the refused add = %q, %v", raw, err)
	}
	if _, err := os.Stat(p.AgentProfileFile("scribe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused add left config.toml: %v", err)
	}

	if _, err := h.AddAgentProfile(ctx, "scribe", node.ProfileEdit{Provider: ptr("claude")}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.EditAgentProfile(ctx, "scribe", node.ProfileEdit{Model: ptr("m"), Livery: ptr("blue-arch")}); !errors.As(err, new(*node.LiveryTakenError)) {
		t.Fatalf("edit to auditor's Livery: %v; want LiveryTakenError", err)
	}
	if s, _ := h.AgentProfile(ctx, "scribe"); s.Model != "" {
		t.Fatalf("the refused edit set the model: %+v", s)
	}
}

// An edit to the random Livery gives the profile a free one, never the
// one it had; with none free it is refused and changes nothing.
func TestAnEditToARandomLiveryGivesAFreeOne(t *testing.T) {
	p := nodeDir(t)
	h, err := node.Open(p, node.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := h.AddAgentProfile(ctx, "auditor", node.ProfileEdit{Provider: ptr("claude"), Livery: ptr("blue-arch")}); err != nil {
		t.Fatal(err)
	}
	free, _ := h.FreeLiveries(ctx)
	// Leave one free besides auditor's own.
	for i, l := range free[1:] {
		if err := h.SetLivery(ctx, fmt.Sprintf("node:%064x", i+1), l); err != nil {
			t.Fatal(err)
		}
	}
	a, err := h.EditAgentProfile(ctx, "auditor", node.ProfileEdit{Livery: ptr(node.RandomLivery)})
	if err != nil || a.Livery != free[0] {
		t.Fatalf("edit to random = %+v, %v; want the one free Livery, %s", a, err, free[0])
	}
	if err := h.SetLivery(ctx, fmt.Sprintf("node:%064x", 64), "blue-arch"); err != nil {
		t.Fatal(err) // the one auditor gave up
	}
	_, err = h.EditAgentProfile(ctx, "auditor", node.ProfileEdit{Model: ptr("m"), Livery: ptr(node.RandomLivery)})
	if !errors.Is(err, node.ErrNoFreeLivery) {
		t.Fatalf("edit to random with none free: %v; want ErrNoFreeLivery", err)
	}
	if a, _ := h.AgentProfile(ctx, "auditor"); a.Livery != free[0] || a.Model != "" {
		t.Fatalf("the refused edit changed auditor: %+v", a)
	}
}
