package cli

import (
	"github.com/spf13/cobra"

	"defios.com/internal/node"
)

// bindWriteGroupFlag lets a writing verb target a Group by alias.
func bindWriteGroupFlag(g *globals, cmd *cobra.Command) {
	cmd.Flags().StringVar(&g.group, "group", "", "write this Group's config.toml instead of the Current Group's")
}

// keyQueryHelp is the ways a --key flag can name a Key.
const keyQueryHelp = "curve:hex, the Conversation ID or the Key alias"

// keyFlagHelp is --key on a read verb of a Workspace section.
const keyFlagHelp = "read with this Key's Conversation config.toml merged over the Group's: " + keyQueryHelp

// keyEditFlagHelp is --key on a writing verb of a Workspace section.
const keyEditFlagHelp = "edit this Key's Conversation config.toml instead of the Group's: " + keyQueryHelp

// workspaceTarget is the Node, Group alias and Conversation a provider or
// network verb acts on.
func workspaceTarget(g *globals, keyQuery string) (*node.Host, string, string, error) {
	h, err := openNode(g)
	if err != nil {
		return nil, "", "", err
	}
	alias, err := g.resolveAlias()
	if err != nil {
		return nil, "", "", err
	}
	conv, err := queriedConversation(g, h, keyQuery)
	if err != nil {
		return nil, "", "", err
	}
	return h, alias, conv, nil
}
