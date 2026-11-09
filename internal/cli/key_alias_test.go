package cli_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"defios.com/internal/group"
	"defios.com/internal/nodedir"
)

// aliasNode is an initialised Node with two minted Keys.
func aliasNode(t *testing.T) (dir, ref, otherRef string) {
	t.Helper()
	dir = t.TempDir()
	mustInit(t, dir)
	pk, other := seedMinted(t, dir)
	return dir, "ecdsa:" + hex.EncodeToString(pk), "ecdsa:" + hex.EncodeToString(other)
}

// keyConfigFile is the Conversation config.toml of the Key ref in Group alias.
func keyConfigFile(dir, alias, ref string) string {
	curve, pub, _ := strings.Cut(ref, ":")
	return nodedir.Resolve(nodedir.Lookup{Dir: dir}).ConversationConfigFile(alias, curve+"-"+pub[:12])
}

func handEdit(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustKey(t *testing.T, dir string, args ...string) result {
	t.Helper()
	r := run(t, nil, "", append([]string{"--dir", dir, "key"}, args...)...)
	if r.code != 0 {
		t.Fatalf("key %v: exit %d, stderr %q", args, r.code, r.stderr)
	}
	return r
}

func TestKeyAliasIsShownBesideTheReference(t *testing.T) {
	dir, ref, otherRef := aliasNode(t)
	mustKey(t, dir, "alias", ref, "Treasury")

	body, err := os.ReadFile(keyConfigFile(dir, group.ImplicitAlias, ref))
	if err != nil || !strings.Contains(string(body), `alias = "Treasury"`) {
		t.Fatalf("config.toml = %q, %v", body, err)
	}

	list := mustKey(t, dir, "list").stdout
	if !strings.Contains(list, "Treasury  "+ref+"  t=2 n=2  share=no\n") {
		t.Fatalf("list:\n%s", list)
	}
	if !strings.Contains(list, "\n"+otherRef+"  t=2 n=2  share=no\n") && !strings.HasPrefix(list, otherRef+"  t=2") {
		t.Fatalf("a Key with no alias changed:\n%s", list)
	}
	if out := mustKey(t, dir, "inspect", ref).stdout; !strings.HasPrefix(out, "key Treasury  "+ref+"\n") {
		t.Fatalf("inspect:\n%s", out)
	}
	if out := mustKey(t, dir, "inspect", otherRef).stdout; !strings.HasPrefix(out, "key "+otherRef+"\n") {
		t.Fatalf("inspect of a Key with no alias:\n%s", out)
	}

	var views []map[string]any
	if err := json.Unmarshal([]byte(mustKey(t, dir, "list", "--json").stdout), &views); err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		switch v["key"] {
		case ref:
			if v["alias"] != "Treasury" {
				t.Errorf("json %v: want alias Treasury", v)
			}
		case otherRef:
			if _, set := v["alias"]; set {
				t.Errorf("json %v: want no alias field", v)
			}
		default:
			t.Errorf("json key = %v; want curve:hex", v["key"])
		}
	}
}

func TestKeyAliasRenamesAndClears(t *testing.T) {
	dir, ref, _ := aliasNode(t)
	file := keyConfigFile(dir, group.ImplicitAlias, ref)
	mustKey(t, dir, "alias", "--clear", ref)
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("clearing no alias wrote %s: %v", file, err)
	}

	handEdit(t, file, "[agent]\nmax_depth = 2\n")
	mustKey(t, dir, "alias", ref, "first")
	mustKey(t, dir, "alias", "FIRST", "First") // a Key may recase its own alias
	if out := mustKey(t, dir, "inspect", ref).stdout; !strings.HasPrefix(out, "key First  "+ref) {
		t.Fatalf("recased:\n%s", out)
	}
	mustKey(t, dir, "alias", ref, "second")
	if out := mustKey(t, dir, "inspect", ref).stdout; !strings.HasPrefix(out, "key second  "+ref) {
		t.Fatalf("renamed:\n%s", out)
	}

	mustKey(t, dir, "alias", "--clear", "second")
	if out := mustKey(t, dir, "inspect", ref).stdout; !strings.HasPrefix(out, "key "+ref+"\n") {
		t.Fatalf("cleared:\n%s", out)
	}
	body, err := os.ReadFile(file)
	if err != nil || strings.Contains(string(body), "alias") || !strings.Contains(string(body), "max_depth = 2") {
		t.Fatalf("config.toml = %q, %v; want no alias and the rest kept", body, err)
	}
}

func TestKeyAliasRefusesAnInvalidOrTakenAlias(t *testing.T) {
	dir, ref, otherRef := aliasNode(t)
	file := keyConfigFile(dir, group.ImplicitAlias, otherRef)
	for _, bad := range []string{"", " padded", "a\tb", strings.Repeat("x", 64)} {
		r := run(t, nil, "", "--dir", dir, "key", "alias", otherRef, bad)
		if r.code != 1 || !strings.Contains(r.stderr, "key alias") {
			t.Errorf("alias %q: exit %d, stderr %q", bad, r.code, r.stderr)
		}
	}

	mustKey(t, dir, "alias", ref, "Treasury")
	r := run(t, nil, "", "--dir", dir, "key", "alias", otherRef, "tREASURY")
	if r.code != 1 || !strings.Contains(r.stderr, ref) {
		t.Fatalf("taken alias: exit %d, stderr %q", r.code, r.stderr)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("a refused alias wrote %s: %v", file, err)
	}
}

func TestKeyAliasIsUniquePerGroup(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	pk, _ := seedMintedAlias(t, dir, group.ImplicitAlias)
	seedMintedAlias(t, dir, "custody")
	ref := "ecdsa:" + hex.EncodeToString(pk)
	mustKey(t, dir, "alias", "--group", group.ImplicitAlias, ref, "treasury")
	mustKey(t, dir, "alias", "--group", "custody", ref, "treasury")
	out := mustKey(t, dir, "inspect", "--group", "custody", "treasury").stdout
	if !strings.HasPrefix(out, "key treasury  "+ref) {
		t.Fatalf("inspect --group custody:\n%s", out)
	}
}

func TestKeyInspectFindsAKeyByAlias(t *testing.T) {
	dir, ref, _ := aliasNode(t)
	mustKey(t, dir, "alias", ref, "Straße 🦄")
	out := mustKey(t, dir, "inspect", "STRASSE 🦄").stdout
	if !strings.HasPrefix(out, "key Straße 🦄  "+ref) {
		t.Fatalf("inspect by alias:\n%s", out)
	}
	if r := run(t, nil, "", "--dir", dir, "key", "inspect", "Straße"); r.code != 2 || !strings.Contains(r.stderr, "no such key") {
		t.Fatalf("a part of an alias: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestKeyInspectRefusesAnAliasOnTwoKeysNamingBoth(t *testing.T) {
	dir, ref, otherRef := aliasNode(t)
	handEdit(t, keyConfigFile(dir, group.ImplicitAlias, ref), "alias = \"twin\"\n")
	handEdit(t, keyConfigFile(dir, group.ImplicitAlias, otherRef), "alias = \"TWIN\"\n")
	r := run(t, nil, "", "--dir", dir, "key", "inspect", "twin")
	if r.code != 1 || !strings.Contains(r.stderr, "ambiguous") || !strings.Contains(r.stderr, ref) || !strings.Contains(r.stderr, otherRef) {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	mustKey(t, dir, "inspect", ref) // curve:hex is not ambiguous
}

func TestABrokenConfigFailsOnlyAnAliasLookup(t *testing.T) {
	dir, ref, otherRef := aliasNode(t)
	mustKey(t, dir, "alias", otherRef, "savings")
	broken := keyConfigFile(dir, group.ImplicitAlias, ref)
	handEdit(t, broken, "alias = \n")

	r := run(t, nil, "", "--dir", dir, "key", "inspect", "savings")
	if r.code != 2 || !strings.Contains(r.stderr, broken) {
		t.Fatalf("alias lookup: exit %d, stderr %q", r.code, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "key", "alias", otherRef, "fresh")
	if r.code != 2 || !strings.Contains(r.stderr, broken) {
		t.Fatalf("setting an alias that the broken file might hold: exit %d, stderr %q", r.code, r.stderr)
	}

	byRef := mustKey(t, dir, "inspect", ref)
	if !strings.HasPrefix(byRef.stdout, "key "+ref+"\n") || !strings.Contains(byRef.stderr, broken) {
		t.Fatalf("inspect by curve:hex:\n%s\n%s", byRef.stdout, byRef.stderr)
	}
	var view struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal([]byte(mustKey(t, dir, "inspect", ref, "--json").stdout), &view); err != nil {
		t.Fatal(err)
	}
	mustKey(t, dir, "inspect", view.ConversationID)

	if out := mustKey(t, dir, "inspect", otherRef); !strings.HasPrefix(out.stdout, "key savings  "+otherRef) || strings.Contains(out.stderr, broken) {
		t.Fatalf("another Key's file broken:\n%s\n%s", out.stdout, out.stderr)
	}
	list := mustKey(t, dir, "list")
	if !strings.Contains(list.stdout, ref+"  t=2") || !strings.Contains(list.stdout, "savings  "+otherRef) || !strings.Contains(list.stderr, broken) {
		t.Fatalf("list:\n%s\n%s", list.stdout, list.stderr)
	}

	// Over 63 code points a query is never looked up as an alias, so the
	// broken file does not matter to it.
	r = run(t, nil, "", "--dir", dir, "key", "inspect", strings.Repeat("🦄", 64))
	if r.code != 2 || !strings.Contains(r.stderr, "no such key") || strings.Contains(r.stderr, broken) {
		t.Fatalf("long query: exit %d, stderr %q", r.code, r.stderr)
	}
}
