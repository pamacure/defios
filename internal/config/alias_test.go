package config_test

import (
	"path/filepath"
	"strings"
	"testing"

	"defios.com/internal/config"
)

func TestAGroupConfigRefusesAnAlias(t *testing.T) {
	_, err := config.ReadFile(write(t, "alias = \"treasury\"\n"))
	if err == nil || !strings.Contains(err.Error(), "alias") {
		t.Fatalf("ReadFile = %v; want alias refused", err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	c := config.Config{Workspace: config.Workspace{Alias: "treasury"}}
	if err := config.WriteFile(path, c); err == nil {
		t.Fatal("WriteFile wrote an alias to a Group's config.toml")
	}
}

func TestAConversationConfigHoldsAnAliasAsTyped(t *testing.T) {
	w, err := config.ReadWorkspace(write(t, "alias = \"Cold 🦄\"\n"))
	if err != nil || w.Alias != "Cold 🦄" {
		t.Fatalf("ReadWorkspace = %+v, %v", w, err)
	}
}

func TestAConversationConfigRefusesAnInvalidAlias(t *testing.T) {
	for _, body := range []string{`alias = ""`, `alias = " x"`, `alias = "a\tb"`, `alias = "` + strings.Repeat("x", 64) + `"`} {
		if w, err := config.ReadWorkspace(write(t, body+"\n")); err == nil {
			t.Errorf("%s: ReadWorkspace = %+v; want refused", body, w)
		}
	}
}

func TestAMergeInheritsNoAlias(t *testing.T) {
	merged, err := config.Workspace{Alias: "under"}.Merge(config.Workspace{Alias: "over"})
	if err != nil || merged.Alias != "" {
		t.Fatalf("Merge = %+v, %v; want no alias", merged, err)
	}
}

// Rewriting a Conversation's config.toml for another reason keeps its alias.
func TestAWriteKeepsTheAliasOnItsOwnLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	w := config.Workspace{Alias: "👩‍💻 Ops", Agent: config.Agent{Profiles: []string{"auditor"}}}
	if err := config.WriteWorkspace(path, w); err != nil {
		t.Fatal(err)
	}
	got, err := config.ReadWorkspace(path)
	if err != nil || got.Alias != w.Alias || len(got.Agent.Profiles) != 1 {
		t.Fatalf("read back %+v, %v", got, err)
	}
	got.Agent.Profiles = nil
	if err := config.WriteWorkspace(path, got); err != nil {
		t.Fatal(err)
	}
	if again, err := config.ReadWorkspace(path); err != nil || again.Alias != w.Alias {
		t.Fatalf("after a second write %+v, %v", again, err)
	}
}

// A syntax error names the file, as every other refusal does.
func TestAFileThatIsNotTOMLIsNamed(t *testing.T) {
	path := write(t, "alias = \n")
	if _, err := config.ReadWorkspace(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("ReadWorkspace = %v; want %s named", err, path)
	}
}
