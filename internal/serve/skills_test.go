package serve_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installAudits installs, as plugin install leaves it, a Plugin audits on n
// with two Skills: erc20, beside a reference file, and solana. A file
// secret.txt sits in the Plugin outside either Skill.
func installAudits(t *testing.T, n *node) {
	t.Helper()
	dir := filepath.Join(n.paths.PluginsDir(), "audits")
	for path, body := range map[string]string{
		"plugin.toml":                   "name = \"audits\"\nversion = \"1.0.0\"\ndescription = \"Contract audits\"\n",
		"secret.txt":                    "not a Skill's\n",
		"skills/erc20/SKILL.md":         "---\nname: erc20\ndescription: Audit an ERC-20 transfer.\n---\n# ERC-20\n\nRead checklist.md first.\n",
		"skills/erc20/ref/checklist.md": "1. Check the decimals.\n",
		"skills/solana/SKILL.md":        "---\nname: solana\ndescription: Audit a Solana transfer.\n---\nCheck the fee payer.\n",
	} {
		writeConfig(t, filepath.Join(dir, path), body)
	}
}

// grantSkills sets the skills of n's profile h, as agentNode writes it, to
// skills, a TOML array, with tools left unset.
func grantSkills(t *testing.T, n *node, h, skills string) {
	t.Helper()
	writeConfig(t, n.paths.AgentProfileFile(h), "provider = \"claude\"\nskills = "+skills+"\n\n[body]\nmodel = \"claude-test\"\nstream = true\nmax_tokens = 1024\n")
}

// An agent granted Skills is offered skill_load without it being in tools,
// and the stable preamble lists each granted Skill with its description,
// sorted whatever order skills lists them in. It loads a Skill's SKILL.md
// without its frontmatter and a file under that Skill; a path leaving the
// Skill, a Skill not granted and a file that is not there are tool errors
// (ADR 0030).
func TestAnAgentLoadsAGrantedSkillAndItsFiles(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	installAudits(t, a)
	grantSkills(t, a, "auditor", `["defios/review-sign-request", "audits/erc20"]`)
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		switch n {
		case 1:
			endCall(w, "skill_load", `{"name":"audits/erc20"}`)
		case 2:
			endCall(w, "skill_load", `{"name":"audits/erc20","file":"ref/checklist.md"}`)
		case 3:
			endCall(w, "skill_load", `{"name":"audits/erc20","file":"../../secret.txt"}`)
		case 4:
			endCall(w, "skill_load", `{"name":"audits/solana"}`)
		case 5:
			endCall(w, "skill_load", `{"name":"audits/erc20","file":"ref/nope.md"}`)
		default:
			endSay(w, "checked")
		}
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor audit this", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.MsgID)); !strings.HasSuffix(got, "commit:checked end") {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	if len(calls) != 6 {
		t.Fatalf("%d calls, want 6", len(calls))
	}
	if calls[0].Tools != "say skill_load" {
		t.Errorf("tools offered %q", calls[0].Tools)
	}
	listing := "- audits/erc20: Audit an ERC-20 transfer.\n- defios/review-sign-request: How to review a Sign Request before accepting or refusing it.\n"
	if !strings.Contains(calls[0].Stable, listing) || strings.Contains(calls[0].Depth, "audits/erc20") {
		t.Errorf("stable preamble lacks the Skills sorted:\n%s", calls[0].Stable)
	}
	if strings.Contains(calls[0].Stable, "audits/solana") {
		t.Errorf("preamble lists a Skill not granted:\n%s", calls[0].Stable)
	}
	body := lastTurn(calls[1])
	if !strings.Contains(body, "# ERC-20") || !strings.Contains(body, "Read checklist.md first.") || strings.Contains(body, "description:") || strings.Contains(body, `"is_error":true`) {
		t.Errorf("skill_load answered %s", body)
	}
	if got := lastTurn(calls[2]); !strings.Contains(got, "1. Check the decimals.") || strings.Contains(got, `"is_error":true`) {
		t.Errorf("skill_load of a file answered %s", got)
	}
	if got := lastTurn(calls[3]); !strings.Contains(got, `"is_error":true`) || strings.Contains(got, "not a Skill's") {
		t.Errorf("skill_load of a path leaving the Skill answered %s", got)
	}
	if got := lastTurn(calls[4]); !strings.Contains(got, `"is_error":true`) || !strings.Contains(got, "not granted") || strings.Contains(got, "fee payer") {
		t.Errorf("skill_load of a Skill not granted answered %s", got)
	}
	if got := lastTurn(calls[5]); !strings.Contains(got, `"is_error":true`) {
		t.Errorf("skill_load of a missing file answered %s", got)
	}
}

// A Skill file that is a symlink out of its Skill is refused, as a path
// leaving it is.
func TestSkillLoadRefusesASymlinkOutOfTheSkill(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	installAudits(t, a)
	dir := filepath.Join(a.paths.PluginsDir(), "audits")
	if err := os.Symlink(filepath.Join(dir, "secret.txt"), filepath.Join(dir, "skills", "erc20", "leak.md")); err != nil {
		t.Fatal(err)
	}
	grantSkills(t, a, "auditor", `["audits/erc20"]`)
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		if n == 1 {
			endCall(w, "skill_load", `{"name":"audits/erc20","file":"leak.md"}`)
			return
		}
		endSay(w, "done")
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor audit", nil)
	if err != nil {
		t.Fatal(err)
	}
	untilEnd(t, s, v.MsgID)
	calls := provider.seen()
	if len(calls) < 2 {
		t.Fatalf("%d calls", len(calls))
	}
	if got := lastTurn(calls[1]); !strings.Contains(got, `"is_error":true`) || strings.Contains(got, "not a Skill's") {
		t.Errorf("skill_load of a symlink out answered %s", got)
	}
}

// A granted Skill that is not installed fails the summon, naming the
// Handle and the entry, and the provider is never called.
func TestAGrantedSkillNotInstalledFailsTheSummon(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	grantSkills(t, a, "auditor", `["audits/erc20"]`)
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor audit", nil)
	if err != nil {
		t.Fatal(err)
	}
	var failed string
	for _, e := range untilEnd(t, s, v.MsgID) {
		if e.Type == "agent.error" {
			failed = e.Error
		}
	}
	if !strings.Contains(failed, "@auditor") || !strings.Contains(failed, "audits/erc20") {
		t.Errorf("agent.error = %q", failed)
	}
	if n := len(provider.seen()); n != 0 {
		t.Errorf("%d provider calls, want 0", n)
	}
}
