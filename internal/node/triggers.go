package node

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/jobrun"
	"defios.com/internal/serve"
	"defios.com/internal/trigger"
)

// Failures of Trigger config (Jobs, Hooks, Schedules) a front end tells
// apart, besides InvalidError (a write the config loader would refuse)
// and config.ErrNotInPlace (a table config.toml defines in a form only a
// text edit changes).
var (
	// ErrAlreadyDefined: a Job, Hook or Schedule by that name is already
	// defined.
	ErrAlreadyDefined = config.ErrAlreadyDefined
	// ErrNotDefined: no Job, Hook or Schedule by that name is defined.
	ErrNotDefined = config.ErrNotDefined
	// ErrNoJob: RunJob named a Job the served Group's config.toml does
	// not define.
	ErrNoJob = serve.ErrNoJob
)

// JobInUseError is a DeleteJob refused because Triggers still run the
// Job: the Hooks and Schedules that do, by name.
type JobInUseError = config.JobInUseError

// Job is one Job of a Group's config.toml (ADR 0032).
type Job struct {
	Name string
	config.Job
}

// Hook is one Hook of a Group's config.toml.
type Hook struct {
	Name string
	config.Hook
}

// Schedule is one Schedule of a Group's config.toml and its next firing,
// zero if it fires no more.
type Schedule struct {
	Name string
	config.Schedule
	Next time.Time
}

// JobResult is how a Job run ended: OK, or the Error it failed on.
type JobResult = serve.JobView

// ErrNoJobRun: JobRun named no Job Run the Group keeps.
var ErrNoJobRun = jobrun.ErrUnknown

// JobRuns lists the Job Runs group keeps (ADR 0040), newest first: Job
// job's alone if job is set, and the last last alone if last > 0. The
// Group need not be served.
func (h *Host) JobRuns(ctx context.Context, group, job string, last int) ([]JobRun, error) {
	if err := h.knownGroup(group); err != nil {
		return nil, err
	}
	return jobrun.Latest(h.p.JobRunsDir(group), job, last)
}

// JobRun is the Job Run group keeps by id, or ErrNoJobRun. The Group need
// not be served.
func (h *Host) JobRun(ctx context.Context, group string, id uint64) (JobRun, error) {
	if err := h.knownGroup(group); err != nil {
		return JobRun{}, err
	}
	return jobrun.Get(h.p.JobRunsDir(group), id)
}

// Jobs lists group's Jobs by name. The Group need not be served.
func (h *Host) Jobs(ctx context.Context, group string) ([]Job, error) {
	c, err := h.groupConfig(group)
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(c.Jobs))
	for _, name := range slices.Sorted(maps.Keys(c.Jobs)) {
		out = append(out, Job{Name: name, Job: c.Jobs[name]})
	}
	return out, nil
}

// Hooks lists group's Hooks by name. The Group need not be served.
func (h *Host) Hooks(ctx context.Context, group string) ([]Hook, error) {
	c, err := h.groupConfig(group)
	if err != nil {
		return nil, err
	}
	out := make([]Hook, 0, len(c.Hooks))
	for _, name := range slices.Sorted(maps.Keys(c.Hooks)) {
		out = append(out, Hook{Name: name, Hook: c.Hooks[name]})
	}
	return out, nil
}

// Schedules lists group's Schedules by name, each with its next firing
// after now. The Group need not be served.
func (h *Host) Schedules(ctx context.Context, group string) ([]Schedule, error) {
	c, err := h.groupConfig(group)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]Schedule, 0, len(c.Schedules))
	for _, name := range slices.Sorted(maps.Keys(c.Schedules)) {
		s := c.Schedules[name]
		out = append(out, Schedule{Name: name, Schedule: s, Next: s.Next(now)})
	}
	return out, nil
}

// groupConfig is group's config.toml, an absent one being empty.
func (h *Host) groupConfig(group string) (config.Config, error) {
	if err := h.knownGroup(group); err != nil {
		return config.Config{}, err
	}
	return h.triggers(group).Config()
}

// triggers is group's Trigger config on this Node's disk.
func (h *Host) triggers(group string) config.Triggers {
	return config.Triggers{Paths: h.p, Group: group}
}

// AddJob defines Job name in group's config.toml, as config.Triggers'
// AddJob does, as a person's save: it clears j's by (ADR 0046).
func (h *Host) AddJob(ctx context.Context, group, name string, j config.Job) error {
	j.By = ""
	return h.writeTriggers(group, func(t config.Triggers) error { return t.AddJob(name, j) })
}

// EditJob replaces Job name's definition with j, checked as AddJob does,
// and clears its by.
func (h *Host) EditJob(ctx context.Context, group, name string, j config.Job) error {
	j.By = ""
	return h.writeTriggers(group, func(t config.Triggers) error { return t.EditJob(name, j) })
}

// DeleteJob removes Job name, which no Trigger may still run: one that
// does is a JobInUseError.
func (h *Host) DeleteJob(ctx context.Context, group, name string) error {
	return h.writeTriggers(group, func(t config.Triggers) error { return t.DeleteJob(name) })
}

// AddHook defines Hook name, whose Jobs must be defined, and clears its
// by, as AddJob does.
func (h *Host) AddHook(ctx context.Context, group, name string, hk config.Hook) error {
	hk.By = ""
	return h.writeTriggers(group, func(t config.Triggers) error { return t.AddHook(name, hk) })
}

// EditHook replaces Hook name's definition with hk, clearing its by.
func (h *Host) EditHook(ctx context.Context, group, name string, hk config.Hook) error {
	hk.By = ""
	return h.writeTriggers(group, func(t config.Triggers) error { return t.EditHook(name, hk) })
}

// DeleteHook removes Hook name.
func (h *Host) DeleteHook(ctx context.Context, group, name string) error {
	return h.writeTriggers(group, func(t config.Triggers) error { return t.DeleteHook(name) })
}

// AddSchedule defines Schedule name, whose Jobs must be defined, and
// clears its by, as AddJob does.
func (h *Host) AddSchedule(ctx context.Context, group, name string, s config.Schedule) error {
	s.By = ""
	return h.writeTriggers(group, func(t config.Triggers) error { return t.AddSchedule(name, s) })
}

// EditSchedule replaces Schedule name's definition with s, clearing its by.
func (h *Host) EditSchedule(ctx context.Context, group, name string, s config.Schedule) error {
	s.By = ""
	return h.writeTriggers(group, func(t config.Triggers) error { return t.EditSchedule(name, s) })
}

// DeleteSchedule removes Schedule name.
func (h *Host) DeleteSchedule(ctx context.Context, group, name string) error {
	return h.writeTriggers(group, func(t config.Triggers) error { return t.DeleteSchedule(name) })
}

// writeTriggers makes write to group's Trigger config, a write the loader
// would refuse being an InvalidError.
func (h *Host) writeTriggers(group string, write func(config.Triggers) error) error {
	if err := h.knownGroup(group); err != nil {
		return err
	}
	err := write(h.triggers(group))
	if errors.Is(err, config.ErrNotWritten) {
		return &InvalidError{Err: err}
	}
	return err
}

// GroupConfigTOML is group's config.toml as text, "" when it has none.
func (h *Host) GroupConfigTOML(ctx context.Context, group string) (string, error) {
	if err := h.knownGroup(group); err != nil {
		return "", err
	}
	b, err := os.ReadFile(h.p.GroupConfigFile(group))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

// SetGroupConfigTOML replaces group's config.toml with text, once it
// loads: a text that does not is an InvalidError and writes nothing.
func (h *Host) SetGroupConfigTOML(ctx context.Context, group, text string) error {
	if err := h.knownGroup(group); err != nil {
		return err
	}
	path := h.p.GroupConfigFile(group)
	if err := config.CheckFile(path, []byte(text)); err != nil {
		return &InvalidError{Err: err}
	}
	if err := mkdirPrivate(filepath.Dir(path)); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text), 0o600)
}

// RunJob runs Job name of the served group now, as no Trigger, on ev or
// on none, and waits for it to end; a Job that fails is reported in the
// result, not as an error. ErrNoJob if config.toml defines no such Job.
func (h *Host) RunJob(ctx context.Context, group, name string, ev *trigger.Event) (JobResult, error) {
	api, err := h.reach(ctx, group)
	if err != nil {
		return JobResult{}, err
	}
	v, err := api.JobRun(ctx, name, ev)
	return v, wrapServed(err)
}
