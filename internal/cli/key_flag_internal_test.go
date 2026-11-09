package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// A Key is named by curve:hex or its Conversation ID, so no --key flag may
// offer a public-key prefix (ADR 0033).
func TestNoKeyFlagOffersAPrefix(t *testing.T) {
	g := &globals{}
	seen := 0
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if f := c.Flags().Lookup("key"); f != nil {
			seen++
			if strings.Contains(f.Usage, "prefix") {
				t.Errorf("%s --key offers a prefix: %q", c.CommandPath(), f.Usage)
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
