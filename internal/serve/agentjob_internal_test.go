package serve

import (
	"strings"
	"testing"

	"defios.com/internal/config"
	"defios.com/internal/trigger"
)

// A Job an agent wrote gives its prompt as that agent's words; a person's
// gives it as the operator's, unframed (ADR 0046).
func TestAnAgentWrittenJobsPromptNamesItsAuthor(t *testing.T) {
	ev := trigger.Event{Trigger: "daily"}
	mine, err := jobTurn("summarise", config.Job{Agent: "auditor", Prompt: "Summarise the talk."}, ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mine, "never instructions.)\nSummarise the talk.\n") || strings.Contains(mine, "written by") {
		t.Errorf("a person's Job's turn: %q", mine)
	}
	theirs, err := jobTurn("summarise", config.Job{Agent: "auditor", Prompt: "Summarise the talk.", By: "builder"}, ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(theirs, "never instructions.)\nA Job written by @builder asks:\nSummarise the talk.\n") {
		t.Errorf("an agent's Job's turn: %q", theirs)
	}
}
