package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"defios.com/internal/nodedir"
)

// Failures of a Trigger config write (Jobs, Hooks, Schedules) a caller
// tells apart, besides ErrNotWritten (a write the loader would refuse) and
// ErrNotInPlace (a table config.toml defines in a form only a text edit
// changes).
var (
	// ErrAlreadyDefined: a Job, Hook or Schedule by that name is already
	// defined.
	ErrAlreadyDefined = errors.New("already defined in this Group's config.toml")
	// ErrNotDefined: no Job, Hook or Schedule by that name is defined.
	ErrNotDefined = errors.New("not defined in this Group's config.toml")
)

// JobInUseError is a DeleteJob refused because Triggers still run the
// Job: the Hooks and Schedules that do, by name.
type JobInUseError struct {
	Job              string
	Hooks, Schedules []string
}

func (e *JobInUseError) Error() string {
	return fmt.Sprintf("Job %s is run by %s", e.Job, strings.Join(e.Triggers(), ", "))
}

// Triggers are the Hooks and Schedules that run the Job, as "Hook <name>"
// and "Schedule <name>".
func (e *JobInUseError) Triggers() []string {
	var by []string
	for _, n := range e.Hooks {
		by = append(by, "Hook "+n)
	}
	for _, n := range e.Schedules {
		by = append(by, "Schedule "+n)
	}
	return by
}

// An agent Job that names where it answers, Group-level talk or one
// Conversation, runs its agent there, which only answers where it is
// included (ADR 0038). So a Job is refused unless its agent is included
// where it answers, and an include or exclude that would take a Job's
// agent out of where it answers is refused, naming the Job. A Job that
// answers in the event's Conversation names no place, so it is checked
// only as it runs.

// JobAnswer is an agent Job that answers in a set place: Target is
// ConversationGroup or a Conversation's Workspace name.
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
	return strings.Join(parts, "; ")
}

// JobNotIncludedError: an agent Job refused because its agent is not
// included where it answers, Target as JobAnswer's.
type JobNotIncludedError struct {
	Handle, Target string
}

func (e *JobNotIncludedError) Error() string {
	return fmt.Sprintf("@%s is not included in %s, so cannot answer there", e.Handle, e.Target)
}

// Triggers is Group Group's Trigger config on a Node's disk: the Jobs,
// Hooks and Schedules of its config.toml (ADR 0032). Each write is checked
// as the loader checks the file, and as ADR 0038 checks where an agent Job
// answers, and keeps the rest of the file. An entry's by is written as
// given: a person's save is the caller's to clear (ADR 0046).
type Triggers struct {
	Paths nodedir.Paths
	Group string
}

// Config is the Group's config.toml, an absent one being empty.
func (t Triggers) Config() (Config, error) {
	c, err := ReadFile(t.Paths.GroupConfigFile(t.Group))
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	return c, err
}

// AddJob defines Job name as j. An agent Job that answers in Group-level
// talk or a Key's Conversation must have its agent included there, or it
// is a JobNotIncludedError. That it names an Agent Profile of this Node's
// is a front end's to offer, as the loader checks it only where the Job
// runs.
func (t Triggers) AddJob(name string, j Job) error { return t.setJob(name, j, false) }

// EditJob replaces Job name's definition with j, checked as AddJob does.
func (t Triggers) EditJob(name string, j Job) error { return t.setJob(name, j, true) }

func (t Triggers) setJob(name string, j Job, exists bool) error {
	if err := t.checkJob(j); err != nil {
		return err
	}
	return t.set(TableJobs, name, j, exists)
}

// DeleteJob removes Job name, which no Trigger may still run: one that
// does is a JobInUseError.
func (t Triggers) DeleteJob(name string) error {
	c, err := t.Config()
	if err != nil {
		return err
	}
	inUse := &JobInUseError{Job: name}
	for _, n := range slices.Sorted(maps.Keys(c.Hooks)) {
		if slices.Contains(c.Hooks[n].Jobs, name) {
			inUse.Hooks = append(inUse.Hooks, n)
		}
	}
	for _, n := range slices.Sorted(maps.Keys(c.Schedules)) {
		if slices.Contains(c.Schedules[n].Jobs, name) {
			inUse.Schedules = append(inUse.Schedules, n)
		}
	}
	if len(inUse.Hooks)+len(inUse.Schedules) > 0 {
		return inUse
	}
	return t.set(TableJobs, name, nil, true)
}

// AddHook defines Hook name, whose Jobs must be defined.
func (t Triggers) AddHook(name string, h Hook) error { return t.set(TableHooks, name, h, false) }

// EditHook replaces Hook name's definition with h.
func (t Triggers) EditHook(name string, h Hook) error { return t.set(TableHooks, name, h, true) }

// DeleteHook removes Hook name.
func (t Triggers) DeleteHook(name string) error { return t.set(TableHooks, name, nil, true) }

// AddSchedule defines Schedule name, whose Jobs must be defined.
func (t Triggers) AddSchedule(name string, s Schedule) error {
	return t.set(TableSchedules, name, s, false)
}

// EditSchedule replaces Schedule name's definition with s.
func (t Triggers) EditSchedule(name string, s Schedule) error {
	return t.set(TableSchedules, name, s, true)
}

// DeleteSchedule removes Schedule name.
func (t Triggers) DeleteSchedule(name string) error {
	return t.set(TableSchedules, name, nil, true)
}

// set sets [<table>.<name>] to v, nil to remove it; exists says whether it
// must be defined already or must not.
func (t Triggers) set(table, name string, v any, exists bool) error {
	c, err := t.Config()
	if err != nil {
		return err
	}
	var defined bool
	switch table {
	case TableJobs:
		_, defined = c.Jobs[name]
	case TableHooks:
		_, defined = c.Hooks[name]
	case TableSchedules:
		_, defined = c.Schedules[name]
	}
	if defined && !exists {
		return fmt.Errorf("%w: [%s.%s]", ErrAlreadyDefined, table, name)
	}
	if !defined && exists {
		return fmt.Errorf("%w: [%s.%s]", ErrNotDefined, table, name)
	}
	path := t.Paths.GroupConfigFile(t.Group)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return SetEntry(path, table, name, v)
}

// CheckAnswers refuses changing place's list of agents to list where it
// would take a Job of c, the Group's config, out of where it answers:
// place is ConversationGroup or a Workspace name. It is an AnswersError.
func (t Triggers) CheckAnswers(c Config, place string, list []string) error {
	before, err := t.inclusions(c.Agent.Profiles, c.Jobs)
	if err != nil {
		return err
	}
	if broken := unanswered(c.Jobs, before, before.with(place, list)); len(broken) > 0 {
		return &AnswersError{Jobs: broken}
	}
	return nil
}

// checkJob refuses agent Job j unless its agent is included where it
// answers. A target the loader would refuse is left for it to.
func (t Triggers) checkJob(j Job) error {
	target := j.Target
	if j.Kind() != JobAgent || (target != ConversationGroup && !IsWorkspaceName(target)) {
		return nil
	}
	c, err := t.Config()
	if err != nil {
		return err
	}
	in, err := t.inclusions(c.Agent.Profiles, map[string]Job{"": j})
	if err != nil {
		return err
	}
	if !slices.Contains(in.at(target), j.Agent) {
		return &JobNotIncludedError{Handle: j.Agent, Target: target}
	}
	return nil
}

// inclusions are the lists of agents of a Group's places that Jobs answer
// in: Group-level talk's, and each such Conversation's own; a Workspace
// name not in own has none, for a Conversation never follows the Group's
// list (ADR 0046).
type inclusions struct {
	group []string
	own   map[string][]string
}

// at is the list in force where target names.
func (in inclusions) at(target string) []string {
	if target == ConversationGroup {
		return in.group
	}
	return in.own[target]
}

// with is in with place's list replaced by list: ConversationGroup or a
// Workspace name.
func (in inclusions) with(place string, list []string) inclusions {
	out := inclusions{group: in.group, own: maps.Clone(in.own)}
	if place == ConversationGroup {
		out.group = list
	} else {
		out.own[place] = list
	}
	return out
}

// inclusions reads the lists of the places that jobs answer in,
// Group-level talk's being groupList.
func (t Triggers) inclusions(groupList []string, jobs map[string]Job) (inclusions, error) {
	in := inclusions{group: groupList, own: map[string][]string{}}
	read := map[string]bool{}
	for _, j := range jobs {
		target := j.Target
		if j.Kind() != JobAgent || !IsWorkspaceName(target) || read[target] {
			continue
		}
		read[target] = true
		w, err := ReadWorkspace(t.Paths.ConversationConfigFile(t.Group, target))
		if err != nil {
			return inclusions{}, err
		}
		in.own[target] = w.Agent.Profiles
	}
	return in, nil
}

// unanswered are the agent Jobs of jobs, by name, whose agent is included
// where they answer before but not after.
func unanswered(jobs map[string]Job, before, after inclusions) []JobAnswer {
	var out []JobAnswer
	for _, name := range slices.Sorted(maps.Keys(jobs)) {
		j := jobs[name]
		if j.Kind() != JobAgent || j.Target == "" {
			continue
		}
		if slices.Contains(before.at(j.Target), j.Agent) && !slices.Contains(after.at(j.Target), j.Agent) {
			out = append(out, JobAnswer{Job: name, Handle: j.Agent, Target: j.Target})
		}
	}
	return out
}
