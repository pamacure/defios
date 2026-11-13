package shell_test

import (
	"context"
	"errors"

	"defios.com/internal/node"
)

// The Agents page is tested over a real Node (agents_host_test.go): the
// fake refuses its changes.

var errNotFaked = errors.New("fake: not faked")

func (n *fakeNode) AgentProfile(context.Context, string) (node.AgentProfile, error) {
	return node.AgentProfile{}, node.ErrUnknownProfile
}
func (n *fakeNode) AddAgentProfile(context.Context, string, node.ProfileEdit) (node.AgentProfile, error) {
	return node.AgentProfile{}, errNotFaked
}
func (n *fakeNode) EditAgentProfile(context.Context, string, node.ProfileEdit) (node.AgentProfile, error) {
	return node.AgentProfile{}, errNotFaked
}
func (n *fakeNode) DeleteAgentProfile(context.Context, string) error { return errNotFaked }
func (n *fakeNode) AgentSystem(context.Context, string) (string, error) {
	return "", node.ErrUnknownProfile
}
func (n *fakeNode) SetAgentSystem(context.Context, string, string) error { return errNotFaked }
func (n *fakeNode) Grant(context.Context, string, string, []string) (node.GrantKind, node.AgentProfile, error) {
	return 0, node.AgentProfile{}, errNotFaked
}
func (n *fakeNode) Revoke(context.Context, string, string) (node.GrantKind, node.AgentProfile, error) {
	return 0, node.AgentProfile{}, errNotFaked
}
func (n *fakeNode) Plugins(context.Context) ([]node.Plugin, error) { return nil, nil }
func (n *fakeNode) Provider(context.Context, string, string, string) (node.Provider, error) {
	return node.Provider{}, errNotFaked
}
func (n *fakeNode) AddProvider(context.Context, string, string, string, node.ProviderEdit) (node.Provider, error) {
	return node.Provider{}, errNotFaked
}
func (n *fakeNode) EditProvider(context.Context, string, string, string, node.ProviderEdit) (node.Provider, error) {
	return node.Provider{}, errNotFaked
}
func (n *fakeNode) RemoveProvider(context.Context, string, string, string) error { return errNotFaked }
func (n *fakeNode) Secrets(context.Context, string, node.Passphrase) ([]node.Secret, error) {
	return nil, nil
}
