package shell_test

import (
	"context"
	"slices"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/node"
	"defios.com/internal/trigger"
)

// The Triggers page is tested over a real Node (triggers_host_test.go):
// the fake lists none and refuses its changes.

func (n *fakeNode) Jobs(context.Context, string) ([]node.Job, error)            { return nil, nil }
func (n *fakeNode) AddJob(context.Context, string, string, config.Job) error    { return errNotFaked }
func (n *fakeNode) EditJob(context.Context, string, string, config.Job) error   { return errNotFaked }
func (n *fakeNode) DeleteJob(context.Context, string, string) error             { return errNotFaked }
func (n *fakeNode) Hooks(context.Context, string) ([]node.Hook, error)          { return nil, nil }
func (n *fakeNode) AddHook(context.Context, string, string, config.Hook) error  { return errNotFaked }
func (n *fakeNode) EditHook(context.Context, string, string, config.Hook) error { return errNotFaked }
func (n *fakeNode) DeleteHook(context.Context, string, string) error            { return errNotFaked }
func (n *fakeNode) Schedules(context.Context, string) ([]node.Schedule, error)  { return nil, nil }
func (n *fakeNode) AddSchedule(context.Context, string, string, config.Schedule) error {
	return errNotFaked
}
func (n *fakeNode) EditSchedule(context.Context, string, string, config.Schedule) error {
	return errNotFaked
}
func (n *fakeNode) DeleteSchedule(context.Context, string, string) error    { return errNotFaked }
func (n *fakeNode) GroupConfigTOML(context.Context, string) (string, error) { return "", nil }
func (n *fakeNode) SetGroupConfigTOML(context.Context, string, string) error {
	return errNotFaked
}
func (n *fakeNode) RunJob(context.Context, string, string, *trigger.Event) (node.JobResult, error) {
	return node.JobResult{}, errNotFaked
}

// addJobRuns keeps runs, one firing's Job Runs in the order they run, as
// a Trigger firing does: each given its ID and Firing.
func (n *fakeNode) addJobRuns(runs ...node.JobRun) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.tk()
	first := uint64(len(t.jobRuns)) + 1
	for i := range runs {
		runs[i].ID, runs[i].Firing = first+uint64(i), first
		t.jobRuns = append(t.jobRuns, runs[i])
	}
}

// endJobRun ends Job Run id in state, with err.
func (n *fakeNode) endJobRun(id uint64, state, err string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	r := &n.tk().jobRuns[id-1]
	r.State, r.Error, r.Ended = state, err, time.Now()
}

// JobRuns are the Job Runs kept, newest first.
func (n *fakeNode) JobRuns(_ context.Context, group, job string, last int) ([]node.JobRun, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := slices.Clone(n.tk().jobRuns)
	slices.Reverse(out)
	return out, nil
}

// JobRun is the Job Run kept by id, or node.ErrNoJobRun.
func (n *fakeNode) JobRun(_ context.Context, group string, id uint64) (node.JobRun, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if id == 0 || id > uint64(len(n.tk().jobRuns)) {
		return node.JobRun{}, node.ErrNoJobRun
	}
	return n.tk().jobRuns[id-1], nil
}
