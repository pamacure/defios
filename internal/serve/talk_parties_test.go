package serve_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// callParties is a model call of talk_parties.
func callParties(w http.ResponseWriter) {
	streamParts(w)
	endCall(w, "talk_parties", "{}")
}

// parties is the talk_parties result c's last turn answers, a party a
// line, sorted, as "<kind> <name> <node>" with " you" and " summonable"
// when set, and " summonable:false" when the field is present but false.
func parties(t *testing.T, c providerCall) []string {
	t.Helper()
	var blocks []struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lastTurn(c), "[user] ")), &blocks); err != nil || len(blocks) != 1 {
		t.Fatalf("last turn %s: %v", lastTurn(c), err)
	}
	var v struct {
		Parties []struct {
			Name       string `json:"name"`
			Kind       string `json:"kind"`
			Node       string `json:"node"`
			You        bool   `json:"you"`
			Summonable *bool  `json:"summonable"`
		} `json:"parties"`
	}
	if err := json.Unmarshal([]byte(blocks[0].Content), &v); err != nil {
		t.Fatalf("result %q: %v", blocks[0].Content, err)
	}
	var out []string
	for _, p := range v.Parties {
		s := p.Kind + " " + p.Name + " " + p.Node
		if p.You {
			s += " you"
		}
		if p.Summonable != nil {
			if *p.Summonable {
				s += " summonable"
			} else {
				s += " summonable:false"
			}
		}
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// talk_parties lists a person for each Roster Node, by the name talk
// lines use, and this Node's Agents included here, marking the caller.
// Holding talk_summon, it marks the Agents a summon would be accepted for
// now: not the caller, nor one already summoned this run (ADR 0041).
func TestTalkPartiesListsWhoIsInTheTalk(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor", "quant", "scribe")
	maxDepth2(t, a)
	grantTools(t, a, "auditor", `["talk_write", "talk_summon", "talk_parties"]`)
	scriptAs(provider, map[string][]func(http.ResponseWriter){
		"auditor": {summonOf("quant"), callParties, silent},
		"quant":   {say("checked")},
	})
	summon(t, a, "@auditor")
	got := parties(t, provider.seen()[2])
	want := []string{
		"agent auditor alice you",
		"agent quant alice summonable:false",
		"agent scribe alice summonable",
		"person alice alice",
		"person bob bob",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("parties = %q\nwant      %q", got, want)
	}
}

// Without talk_summon, talk_parties gives no party a summonable; holding
// it at the max depth, every other Agent is summonable:false (ADR 0041).
func TestTalkPartiesMarksSummonableOnlyForASummoner(t *testing.T) {
	for _, c := range []struct {
		name, tools string
		belowMax    bool
		quant       string
	}{
		{"without talk_summon", `["talk_parties"]`, true, "agent quant alice"},
		{"at the max depth", `["talk_summon", "talk_parties"]`, false, "agent quant alice summonable:false"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, _, _, provider := agentPair(t, "auditor", "quant")
			if c.belowMax {
				maxDepth2(t, a)
			}
			grantTools(t, a, "auditor", c.tools)
			scriptAs(provider, map[string][]func(http.ResponseWriter){
				"auditor": {callParties, silent},
			})
			summon(t, a, "@auditor")
			want := []string{"agent auditor alice you", c.quant, "person alice alice", "person bob bob"}
			if got := parties(t, provider.seen()[1]); !slices.Equal(got, want) {
				t.Fatalf("parties = %q\nwant      %q", got, want)
			}
		})
	}
}

// talk_parties is opt-in: a profile that does not name it is not offered
// it.
func TestTalkPartiesIsNotOfferedUnlessGranted(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	grantTools(t, a, "auditor", `["talk_write", "talk_summon"]`)
	summon(t, a, "@auditor")
	if tools := provider.seen()[0].Tools; strings.Contains(tools, "talk_parties") {
		t.Fatalf("tools = %s", tools)
	}
}
