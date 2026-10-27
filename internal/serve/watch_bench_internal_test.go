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

// One wake of the prune policy, on, while nothing has changed: the covered
// talk is already pruned. Since ADR 0014 an idle serve wakes it only at the
// fallback tick; this is that wake less the stat of config.toml.
func BenchmarkPrunePolicyIdleWake(b *testing.B) {
	for _, shape := range coslbench.Shapes {
		b.Run(shape.Name, func(b *testing.B) {
			r := coslbench.Build(b, shape)
			store, err := cosl.Open(r.Path)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { store.Close() })
			var pruned bool
			s := &server{opts: Options{
				Paths: nodedir.Resolve(nodedir.Lookup{Dir: filepath.Join(b.TempDir(), "node")}),
				Alias: group.ImplicitAlias,
				Logf: func(format string, args ...any) {
					if pruned {
						b.Fatalf(format, args...)
					}
				},
			}, ctx: context.Background()}
			s.opts.Config.Prune = true
			p := s.newPrunePolicy(store)
			p.wake()
			pruned = true
			b.ReportAllocs()
			for b.Loop() {
				p.wake()
			}
		})
	}
}
