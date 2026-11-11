package node

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"defios.com/internal/config"
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
	ErrAlreadyDefined = errors.New("node: already defined in this Group's config.toml")
	// ErrNotDefined: no Job, Hook or Schedule by that name is defined.
	ErrNotDefined = errors.New("node: not defined in this Group's config.toml")
	// ErrNoJob: RunJob named a Job the served Group's config.toml does
	// not define.
	ErrNoJob = serve.ErrNoJob
)

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
	c, err := config.ReadFile(h.p.GroupConfigFile(group))
	if errors.Is(err, os.ErrNotExist) {
		return config.Config{}, nil
	}
	return c, err
}

// AddJob defines Job name in group's config.toml. An agent Job's Handle
// is checked as the config loader checks it: one answering in Group-level
// talk must be in the Group's [agent] profiles. That it names an Agent
// Profile of this Node's is a front end's to offer, as the loader checks
// it only where the Job runs.
func (h *Host) AddJob(ctx context.Context, group, name string, j config.Job) error {
	return h.setEntry(group, config.TableJobs, name, j, false)
}

// EditJob replaces Job name's definition with j.
func (h *Host) EditJob(ctx context.Context, group, name string, j config.Job) error {
	return h.setEntry(group, config.TableJobs, name, j, true)
}

// DeleteJob removes Job name, which no Trigger may still run.
func (h *Host) DeleteJob(ctx context.Context, group, name string) error {
	return h.setEntry(group, config.TableJobs, name, nil, true)
}

// AddHook defines Hook name, whose Jobs must be defined.
func (h *Host) AddHook(ctx context.Context, group, name string, hk config.Hook) error {
	return h.setEntry(group, config.TableHooks, name, hk, false)
}

// EditHook replaces Hook name's definition with hk.
func (h *Host) EditHook(ctx context.Context, group, name string, hk config.Hook) error {
	return h.setEntry(group, config.TableHooks, name, hk, true)
}

// DeleteHook removes Hook name.
func (h *Host) DeleteHook(ctx context.Context, group, name string) error {
	return h.setEntry(group, config.TableHooks, name, nil, true)
}

// AddSchedule defines Schedule name, whose Jobs must be defined.
func (h *Host) AddSchedule(ctx context.Context, group, name string, s config.Schedule) error {
	return h.setEntry(group, config.TableSchedules, name, s, false)
}

// EditSchedule replaces Schedule name's definition with s.
func (h *Host) EditSchedule(ctx context.Context, group, name string, s config.Schedule) error {
	return h.setEntry(group, config.TableSchedules, name, s, true)
}

// DeleteSchedule removes Schedule name.
func (h *Host) DeleteSchedule(ctx context.Context, group, name string) error {
	return h.setEntry(group, config.TableSchedules, name, nil, true)
}

// setEntry sets [<table>.<name>] of group's config.toml to v, nil to
// remove it; exists says whether it must be defined already or must not.
func (h *Host) setEntry(group, table, name string, v any, exists bool) error {
	c, err := h.groupConfig(group)
	if err != nil {
		return err
	}
	var defined bool
	switch table {
	case config.TableJobs:
		_, defined = c.Jobs[name]
	case config.TableHooks:
		_, defined = c.Hooks[name]
	case config.TableSchedules:
		_, defined = c.Schedules[name]
	}
	if defined && !exists {
		return fmt.Errorf("%w: [%s.%s]", ErrAlreadyDefined, table, name)
	}
	if !defined && exists {
		return fmt.Errorf("%w: [%s.%s]", ErrNotDefined, table, name)
	}
	path := h.p.GroupConfigFile(group)
	if err := mkdirPrivate(filepath.Dir(path)); err != nil {
		return err
	}
	err = config.SetEntry(path, table, name, v)
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
