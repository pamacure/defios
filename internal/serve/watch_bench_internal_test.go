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
// every Workspace exists, as on an idle serve.
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
			made := map[[32]byte]bool{}
			s.ensureWorkspaces(store, made)
			if len(made) != shape.ECDSAKeys+shape.EdDSAKeys {
				b.Fatalf("%d Workspaces made, want %d", len(made), shape.ECDSAKeys+shape.EdDSAKeys)
			}
			b.ReportAllocs()
			for b.Loop() {
				s.ensureWorkspaces(store, made)
			}
		})
	}
}
