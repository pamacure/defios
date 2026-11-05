package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"defios.com/internal/config"
	"defios.com/internal/key"
	"defios.com/internal/nodedir"
)

// mainKey's Conversation Workspace is ecdsa-02a1b2c3d4e5 (key.WorkspaceNames).
var mainKey = key.Key{Curve: "ecdsa", PublicKey: []byte{0x02, 0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6, 0x07}}

func mainConv() []byte {
	id := key.ConversationID(mainKey.Curve, mainKey.PublicKey)
	return id[:]
}

// node is a temp Node's paths, with the Group "home"'s config.toml and the
// Conversation Workspace ecdsa-02a1b2c3d4e5's written when not empty.
func node(t *testing.T, group, conv string) nodedir.Paths {
	t.Helper()
	p := nodedir.Resolve(nodedir.Lookup{Dir: t.TempDir()})
	for path, body := range map[string]string{
		p.GroupConfigFile("home"):                              group,
		p.ConversationConfigFile("home", "ecdsa-02a1b2c3d4e5"): conv,
	} {
		if body == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// withProfiles writes each Agent Profile's agents/<handle>/config.toml in p.
func withProfiles(t *testing.T, p nodedir.Paths, profiles map[string]string) nodedir.Paths {
	t.Helper()
	for h, body := range profiles {
		if err := os.MkdirAll(p.AgentProfileDir(h), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.AgentProfileFile(h), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

const mainnet = "[networks.mainnet]\ntype = \"ethereum\"\nchain_id = 1\nrpc_urls = [\"$KEY_ETH\"]\n"

// A Group with no config.toml reads as empty, at any place.
func TestLoadReadsAnAbsentGroupFileAsEmpty(t *testing.T) {
	p := node(t, "", "")
	for name, conv := range map[string][]byte{"Group-level": nil, "a Conversation": mainConv()} {
		e, err := config.Load(p, "home", conv, []key.Key{mainKey})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(e.Group, config.Config{}) || !reflect.DeepEqual(e.Workspace, config.Workspace{}) {
			t.Fatalf("%s: %+v; want empty", name, e)
		}
		if e.GroupFile() != p.GroupConfigFile("home") {
			t.Fatalf("%s: group file %q", name, e.GroupFile())
		}
	}
}

// Group-level talk, and a Conversation with no config.toml, follow the
// Group's settings; only a Conversation names its file.
func TestLoadWithoutAConversationFileIsTheGroups(t *testing.T) {
	p := node(t, mainnet+"[agent]\nmax_depth = 2\n", "")
	for name, tc := range map[string]struct {
		conv     []byte
		keys     []key.Key
		convFile string
	}{
		"Group-level":          {nil, []key.Key{mainKey}, ""},
		"no config.toml":       {mainConv(), []key.Key{mainKey}, p.ConversationConfigFile("home", "ecdsa-02a1b2c3d4e5")},
		"no Key minted for it": {mainConv(), nil, ""},
	} {
		e, err := config.Load(p, "home", tc.conv, tc.keys)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if e.Workspace.Agent.MaxDepth != 2 || len(e.Workspace.Networks) != 1 || e.ConversationFile() != tc.convFile {
			t.Fatalf("%s: workspace %+v, conversation file %q", name, e.Workspace, e.ConversationFile())
		}
	}
}

// A Conversation's config.toml is found by its Key's Workspace name and
// merged over the Group's; the Group's file is kept as read.
func TestLoadMergesTheConversationFileFoundByItsWorkspaceName(t *testing.T) {
	p := node(t, "[agent]\nmax_depth = 2\n", mainnet+"[agent]\nmax_depth = 3\n")
	e, err := config.Load(p, "home", mainConv(), []key.Key{mainKey})
	if err != nil {
		t.Fatal(err)
	}
	if e.Workspace.Agent.MaxDepth != 3 || len(e.Workspace.Networks) != 1 || e.Group.Agent.MaxDepth != 2 || len(e.Group.Networks) != 0 {
		t.Fatalf("workspace %+v, group %+v", e.Workspace, e.Group)
	}
	nets, err := e.Networks()
	if err != nil || len(nets) != 1 || nets[0].Alias != "mainnet" {
		t.Fatalf("networks %+v, %v", nets, err)
	}
}

// A Conversation's Networks that, merged over the Group's, name one chain
// twice are refused, naming both files.
func TestLoadRefusesAMergeConflictNamingBothFiles(t *testing.T) {
	p := node(t, mainnet, "[networks.eth]\ntype = \"ethereum\"\nchain_id = 1\nrpc_urls = [\"$KEY_ETH\"]\n")
	_, err := config.Load(p, "home", mainConv(), []key.Key{mainKey})
	if err == nil || !strings.Contains(err.Error(), p.GroupConfigFile("home")) || !strings.Contains(err.Error(), p.ConversationConfigFile("home", "ecdsa-02a1b2c3d4e5")) {
		t.Fatalf("load = %v; want a refusal naming both files", err)
	}
}

// A file that does not parse fails Load, naming it.
func TestLoadFailsOnAFileThatDoesNotParse(t *testing.T) {
	for name, p := range map[string]nodedir.Paths{
		"group":        node(t, "nope = 1\n", ""),
		"conversation": node(t, "", "hostname = \"x\"\n"),
	} {
		_, err := config.Load(p, "home", mainConv(), []key.Key{mainKey})
		if err == nil || !strings.Contains(err.Error(), "config.toml") {
			t.Fatalf("%s: load = %v; want an error naming the file", name, err)
		}
	}
}

// A Conversation's [agent] profiles naming no Agent Profile fails
// Included, saying which files to fix, but not Load or Networks: signing
// keeps working with the agent list broken.
func TestABadProfilesListFailsIncludedButNotNetworks(t *testing.T) {
	p := withProfiles(t, node(t, mainnet+"[agent]\nprofiles = [\"auditor\"]\n", "[agent]\nprofiles = [\"ghost\"]\n"),
		map[string]string{"auditor": "provider = \"claude\"\n"})
	e, err := config.Load(p, "home", mainConv(), []key.Key{mainKey})
	if err != nil {
		t.Fatal(err)
	}
	want := "fix [agent] profiles in " + p.ConversationConfigFile("home", "ecdsa-02a1b2c3d4e5") + ", or add " + p.AgentProfileFile("ghost")
	if _, err := e.Included(); err == nil || !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), want) {
		t.Fatalf("included = %v; want it to name ghost and %q", err, want)
	}
	if nets, err := e.Networks(); err != nil || len(nets) != 1 {
		t.Fatalf("networks = %+v, %v", nets, err)
	}
}

// ConversationFiles is every Conversation config.toml of the Group.
func TestConversationFilesListsTheGroups(t *testing.T) {
	p := node(t, mainnet, "[agent]\nmax_depth = 3\n")
	got, err := config.ConversationFiles(p, "home")
	if want := []string{p.ConversationConfigFile("home", "ecdsa-02a1b2c3d4e5")}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %v, %v; want %v", got, err, want)
	}
}
