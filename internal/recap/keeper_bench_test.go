package recap

import (
	"testing"

	"defios.com/internal/cosl"
	"defios.com/internal/cosl/coslbench"
)

// One wake of the prune policy while nothing has changed: the replica's
// covered talk is already pruned.
func BenchmarkKeeperIdleWake(b *testing.B) {
	for _, shape := range coslbench.Shapes {
		b.Run(shape.Name, func(b *testing.B) {
			r := coslbench.Build(b, shape)
			store, err := cosl.Open(r.Path)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { store.Close() })
			k := NewKeeper(store)
			if got, err := k.Step(); err != nil || len(got) == 0 {
				b.Fatalf("first Step = %v, %v; want the Group-level talk pruned", got, err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := k.Step(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
