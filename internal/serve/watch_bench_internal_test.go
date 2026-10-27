package serve

import (
	"context"
	"path/filepath"
	"testing"

	"defios.com/internal/cosl"
	"defios.com/internal/cosl/coslbench"
	"defios.com/internal/group"
	"defios.com/internal/nodedir"
)

// One wake of the Conversation Workspace watcher while nothing has changed:
// every Workspace exists, as on an idle serve. Since ADR 0014 an idle serve
// wakes only at the fallback tick, and this is what that wake costs.
func BenchmarkWorkspaceIdleWake(b *testing.B) {
	for _, shape := range coslbench.Shapes {
		b.Run(shape.Name, func(b *testing.B) {
			r := coslbench.Build(b, shape)
			store, err := cosl.Open(r.Path)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { store.Close() })
			s := &server{opts: Options{
				Paths: nodedir.Resolve(nodedir.Lookup{Dir: filepath.Join(b.TempDir(), "node")}),
				Alias: group.ImplicitAlias,
				Logf:  func(format string, args ...any) { b.Fatalf(format, args...) },
			}, ctx: context.Background()}
			w := s.newWorkspaceWatcher(store)
			w.wake()
			if len(w.made) != shape.ECDSAKeys+shape.EdDSAKeys {
				b.Fatalf("%d Workspaces made, want %d", len(w.made), shape.ECDSAKeys+shape.EdDSAKeys)
			}
			b.ReportAllocs()
			for b.Loop() {
				w.wake()
			}
		})
	}
}
