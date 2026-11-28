package serve_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"defios.com/internal/config"
	"defios.com/internal/group"
	"defios.com/internal/jobrun"
	"defios.com/internal/key"
)

// toolText is the text of the Tool result in turn.
func toolText(t *testing.T, turn string) string {
	t.Helper()
	m := regexp.MustCompile(`"content":"((?:[^"\\]|\\.)*)"`).FindStringSubmatch(turn)
	if m == nil {
		t.Fatalf("no tool result in %s", turn)
	}
	var text string
	if err := json.Unmarshal([]byte(`"`+m[1]+`"`), &text); err != nil {
		t.Fatal(err)
	}
	return text
}

// runIDs are the IDs of the Job Runs a job_runs result lists, in order.
func runIDs(t *testing.T, turn string) []uint64 {
	t.Helper()
	var runs []jobrun.Run
	if err := json.Unmarshal([]byte(toolText(t, turn)), &runs); err != nil {
		t.Fatalf("job_runs answered %s: %v", turn, err)
	}
	ids := []uint64{}
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	return ids
}

// job_runs lists this Group's Job Runs newest first, as Host.JobRuns does:
// all of them, one Job's, or the last N, and reads one by id. It refuses an
// unknown id, an id with a filter, and input it does not know.
func TestJobRunsListsTheGroupsJobRuns(t *testing.T) {
	a, _, _, provider := agentPair(t, "builder")
	writeConfig(t, a.paths.AgentProfileFile("builder"), "provider = \"claude\"\nmax_turns = 40\ntools = [\"talk_write\", \"job_runs\"]\n\n[body]\nmodel = \"claude-test\"\nstream = true\nmax_tokens = 1024\n")
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	writeConfig(t, path, readFile(t, path)+"\n[jobs.ping]\nexec = [\"true\"]\n\n[jobs.pong]\nexec = [\"false\"]\n")
	ctx := context.Background()
	for _, job := range []string{"ping", "pong", "ping"} {
		if _, err := a.client().JobRun(ctx, job, nil); err != nil {
			t.Fatal(err)
		}
	}
	got := runCalls(t, a, provider, [][2]string{
		{"job_runs", `{}`},
		{"job_runs", `{"job":"ping"}`},
		{"job_runs", `{"last":1}`},
		{"job_runs", `{"job":"pong","last":5}`},
		{"job_runs", `{"id":2}`},
		{"job_runs", `{"id":99}`},
		{"job_runs", `{"id":2,"job":"ping"}`},
		{"job_runs", `{"lsat":1}`},
		{"job_runs", `{"last":0}`},
	})
	for i, want := range [][]uint64{{3, 2, 1}, {3, 1}, {3}, {2}} {
		if ids := runIDs(t, got[i]); !slices.Equal(ids, want) {
			t.Errorf("call %d listed Job Runs %v, want %v", i, ids, want)
		}
	}
	var one jobrun.Run
	if err := json.Unmarshal([]byte(toolText(t, got[4])), &one); err != nil || one.ID != 2 || one.Job != "pong" || one.State != jobrun.Failed || !one.ByHand {
		t.Errorf("job_runs {id: 2} answered %s", got[4])
	}
	for i, want := range map[int]string{5: "no Job Run 99", 6: "id alone", 7: "its input must be", 8: "its input must be"} {
		if !strings.Contains(got[i], `"is_error":true`) || !strings.Contains(got[i], want) {
			t.Errorf("call %d answered %s; want a refusal saying %s", i, got[i], want)
		}
	}
}

// twoG is the compressed secp256k1 public key 2G.
var twoG, _ = hex.DecodeString("02c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5")

// keyListEntry is one Key key_list gives.
type keyListEntry struct {
	Alias       string            `json:"alias"`
	Workspace   string            `json:"workspace"`
	Key         string            `json:"key"`
	Curve       string            `json:"curve"`
	PublicKey   string            `json:"public_key"`
	NetworkType string            `json:"network_type"`
	Networks    []json.RawMessage `json:"networks"`
	Handles     []string          `json:"handles"`
}

// key_list gives each of the Group's Keys with its alias, Workspace name,
// curve, public key, Network type, its addresses on the Node's Networks of
// that type, the same key_address gives in its Conversation, and the
// Handles its own Conversation includes, never the Group's; and the
// Handles Group-level talk includes (ADR 0046).
func TestKeyListGivesEachKeyAndWhoIsIncluded(t *testing.T) {
	a, b, gid, provider := agentPair(t, "auditor", "builder")
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	writeConfig(t, path, strings.Replace(readFile(t, path), `profiles = ["auditor", "builder"]`, `profiles = ["builder"]`, 1)+
		"\n[networks.eth]\ntype = \"ethereum\"\nchain_id = 1\nrpc_urls = [\""+fakeEth(t)+"\"]\n\n[networks.btc]\ntype = \"bitcoin\"\nnetwork = \"mainnet\"\n")
	grantTools(t, a, "builder", `["talk_write", "key_list"]`)
	grantTools(t, a, "auditor", `["talk_write", "key_address"]`)
	eth := mintPublicKey(t, a, b, gid, generator)
	waitWorkspace(t, a, eth)
	includeIn(t, a, eth, "auditor")
	btc := mintPublicKey(t, a, b, gid, twoG)
	waitWorkspace(t, a, btc)
	file := a.paths.ConversationConfigFile(group.ImplicitAlias, btc)
	if err := config.WriteKeyAlias(file, "treasury"); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteKeyNetworkType(file, "bitcoin"); err != nil {
		t.Fatal(err)
	}

	got := runCalls(t, a, provider, [][2]string{{"key_list", `{}`}, {"key_list", `{"all":true}`}})
	var list struct {
		Keys  []keyListEntry `json:"keys"`
		Group struct {
			Handles []string `json:"handles"`
		} `json:"group"`
	}
	if err := json.Unmarshal([]byte(toolText(t, got[0])), &list); err != nil {
		t.Fatalf("key_list answered %s: %v", got[0], err)
	}
	if !slices.Equal(list.Group.Handles, []string{"builder"}) || len(list.Keys) != 2 {
		t.Fatalf("key_list answered %s", got[0])
	}
	byWorkspace := map[string]keyListEntry{}
	for _, k := range list.Keys {
		byWorkspace[k.Workspace] = k
	}
	e, tr := byWorkspace[eth], byWorkspace[btc]
	if e.Alias != "" || e.Curve != "ecdsa" || e.PublicKey != hex.EncodeToString(generator) || e.NetworkType != "ethereum" || !slices.Equal(e.Handles, []string{"auditor"}) {
		t.Errorf("the Ethereum Key is %+v", e)
	}
	if want := `{"network":"btc","type":"bitcoin","address":"bc1qq6hag67dl53wl99vzg42z8eyzfz2xlkvxechjp"}`; tr.Alias != "treasury" || tr.NetworkType != "bitcoin" ||
		tr.Handles == nil || len(tr.Handles) != 0 || len(tr.Networks) != 1 || string(tr.Networks[0]) != want {
		t.Errorf("the Bitcoin Key is %+v; want alias treasury, no Handles and only %s", tr, want)
	}
	if !strings.Contains(got[1], `"is_error":true`) || !strings.Contains(got[1], "its input must be {}") {
		t.Errorf("key_list {all: true} answered %s", got[1])
	}

	// key_address in the Ethereum Key's Conversation gives the same.
	before := len(provider.seen())
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		if n == before+1 {
			endCall(w, "key_address", `{}`)
			return
		}
		endTalkWrite(w, "noted")
	})
	conv := key.ConversationID("ecdsa", generator)
	if _, events := summonIn(t, a, conv[:], "@auditor where do we receive?"); !strings.HasSuffix(events, "commit:noted end") {
		t.Fatalf("events = %s", events)
	}
	var addr keyListEntry
	if err := json.Unmarshal([]byte(toolText(t, lastTurn(provider.seen()[before+1]))), &addr); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(addr.Networks) != fmt.Sprint(e.Networks) || len(e.Networks) != 1 || addr.Key != e.Key {
		t.Errorf("key_address gives %s, key_list %s", addr.Networks, e.Networks)
	}
}
