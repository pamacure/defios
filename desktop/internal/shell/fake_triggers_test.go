package shell_test

import (
	"context"

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
