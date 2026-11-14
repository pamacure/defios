package shell_test

import (
	"context"

	"defios.com/internal/node"
)

// The Plugins and Networks pages are tested over a real Node
// (plugins_host_test.go, networks_host_test.go): the fake has none and
// refuses their changes.

func (n *fakeNode) InstalledPlugins(context.Context) ([]string, error) { return nil, nil }
func (n *fakeNode) StagePluginInstall(context.Context, string) (*node.PluginStage, error) {
	return nil, errNotFaked
}
func (n *fakeNode) StagePluginUpdate(context.Context, string) (*node.PluginStage, error) {
	return nil, errNotFaked
}
func (n *fakeNode) RemovePlugin(context.Context, string) error { return errNotFaked }
func (n *fakeNode) AddNetwork(context.Context, string, string, string, node.NetworkEdit) (node.Network, error) {
	return node.Network{}, errNotFaked
}
func (n *fakeNode) EditNetwork(context.Context, string, string, string, node.NetworkEdit) (node.Network, error) {
	return node.Network{}, errNotFaked
}
func (n *fakeNode) RemoveNetwork(context.Context, string, string, string) error { return errNotFaked }
