package node

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"defios.com/internal/config"
)

// An agent Job that names where it answers, Group-level talk or one
// Conversation, runs its agent there, which only answers where it is
// included. So a Job is refused unless its agent is included where it
// answers, and an include or exclude that would take a Job's agent out of
// where it answers is refused, naming the Job. A Job that answers in the
// event's Conversation names no place, so it is checked only as it runs.

// JobAnswer is an agent Job that answers in a set place: Target is
// config.ConversationGroup or a Conversation's Workspace name.
type JobAnswer struct {
	Job, Handle, Target string
}

// AnswersError: an include, exclude or change of a list of agents refused
// because it would take the agents of Jobs out of where they answer.
type AnswersError struct {
	Jobs []JobAnswer
}

func (e *AnswersError) Error() string {
	parts := make([]string, len(e.Jobs))
	for i, j := range e.Jobs {
		parts[i] = fmt.Sprintf("@%s answers in %s for Job %s", j.Handle, j.Target, j.Job)
	}
	return "node: " + strings.Join(parts, "; ")
}

// JobNotIncludedError: an agent Job refused because its agent is not
// included where it answers, Target as JobAnswer's.
type JobNotIncludedError struct {
	Handle, Target string
}

func (e *JobNotIncludedError) Error() string {
	return fmt.Sprintf("node: @%s is not included in %s, so cannot answer there", e.Handle, e.Target)
}

// inclusions are the lists of agents of a Group's places that Jobs answer
// in: Group-level talk's, and each such Conversation's own; a Workspace
// name not in own follows the Group's.
type inclusions struct {
	group []string
	own   map[string][]string
}

// at is the list in force where target names.
func (in inclusions) at(target string) []string {
	if l, ok := in.own[target]; ok {
		return l
	}
	return in.group
}

// with is in with place's list replaced by list: config.ConversationGroup
// or a Workspace name.
func (in inclusions) with(place string, list []string) inclusions {
	out := inclusions{group: in.group, own: maps.Clone(in.own)}
	if place == config.ConversationGroup {
		out.group = list
	} else {
		out.own[place] = list
	}
	return out
}

// inclusions reads the lists of group's places that jobs answer in, its
// Group-level talk's being groupList.
func (h *Host) inclusions(group string, groupList []string, jobs map[string]config.Job) (inclusions, error) {
	in := inclusions{group: groupList, own: map[string][]string{}}
	read := map[string]bool{}
	for _, j := range jobs {
		t := j.Target
		if j.Kind() != config.JobAgent || !config.IsWorkspaceName(t) || read[t] {
			continue
		}
		read[t] = true
		w, err := config.ReadWorkspace(h.p.ConversationConfigFile(group, t))
		if err != nil {
			return inclusions{}, err
		}
		if w.Agent.Profiles != nil {
			in.own[t] = w.Agent.Profiles
		}
	}
	return in, nil
}

// unanswered are the agent Jobs of jobs, by name, whose agent is included
// where they answer before but not after.
func unanswered(jobs map[string]config.Job, before, after inclusions) []JobAnswer {
	var out []JobAnswer
	for _, name := range slices.Sorted(maps.Keys(jobs)) {
		j := jobs[name]
		if j.Kind() != config.JobAgent || j.Target == "" {
			continue
		}
		if slices.Contains(before.at(j.Target), j.Agent) && !slices.Contains(after.at(j.Target), j.Agent) {
			out = append(out, JobAnswer{Job: name, Handle: j.Agent, Target: j.Target})
		}
	}
	return out
}

// checkAnswers refuses changing place's list in group to list where it
// would take a Job's agent out of where it answers: place is
// config.ConversationGroup or a Workspace name, and c group's config.
func (h *Host) checkAnswers(group string, c config.Config, place string, list []string) error {
	before, err := h.inclusions(group, c.Agent.Profiles, c.Jobs)
	if err != nil {
		return err
	}
	if broken := unanswered(c.Jobs, before, before.with(place, list)); len(broken) > 0 {
		return &AnswersError{Jobs: broken}
	}
	return nil
}

// checkJob refuses agent Job j unless its agent is included where it
// answers. A target the config loader would refuse is left for it to.
func (h *Host) checkJob(group string, j config.Job) error {
	t := j.Target
	if j.Kind() != config.JobAgent || (t != config.ConversationGroup && !config.IsWorkspaceName(t)) {
		return nil
	}
	c, err := h.groupConfig(group)
	if err != nil {
		return err
	}
	in, err := h.inclusions(group, c.Agent.Profiles, map[string]config.Job{"": j})
	if err != nil {
		return err
	}
	if !slices.Contains(in.at(t), j.Agent) {
		return &JobNotIncludedError{Handle: j.Agent, Target: t}
	}
	return nil
}
