package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// A Key is named by curve:hex, its Conversation ID or its alias, and no longer
// by a public-key prefix, so every --key flag offers the alias and none a
// prefix (ADR 0033).
func TestEveryKeyFlagOffersAnAliasAndNoPrefix(t *testing.T) {
	g := &globals{}
	seen := 0
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if f := c.Flags().Lookup("key"); f != nil {
			seen++
			if strings.Contains(f.Usage, "prefix") {
				t.Errorf("%s --key offers a prefix: %q", c.CommandPath(), f.Usage)
			}
			if !strings.Contains(f.Usage, "alias") {
				t.Errorf("%s --key does not offer an alias: %q", c.CommandPath(), f.Usage)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(App{}.rootCommand(g))
	if seen == 0 {
		t.Fatal("no --key flags found")
	}
}
