package cli

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"defios.com/internal/cosl"
	"defios.com/internal/cosl/coslbench"
	"defios.com/internal/group"
	"defios.com/internal/key"
)

// One listing of a Group's Keys, as `--key` resolution and `key inspect` do
// it: list every minted Key off the replica and resolve one curve:hex among
// them. Since ADR 0014 a listing reads Session start, complete and
// abort rows only, so talk and round traffic cost it nothing.
func BenchmarkKeyListing(b *testing.B) {
	for _, shape := range coslbench.Shapes {
		b.Run(shape.Name, func(b *testing.B) {
			r := coslbench.Build(b, shape)
			store, err := cosl.Open(r.Path)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { store.Close() })
			g := &globals{dir: b.TempDir()}
			if err := os.MkdirAll(filepath.Join(g.paths().GroupsDir(), group.ImplicitAlias), 0o700); err != nil {
				b.Fatal(err)
			}
			ref := "ecdsa:" + hex.EncodeToString(r.SignKey)
			want := key.ConversationID("ecdsa", r.SignKey)
			got, err := conversationID(g, store, ref)
			if err != nil || string(got) != string(want[:]) {
				b.Fatalf("conversationID(%s) = %x, %v; want %x", ref, got, err, want)
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := conversationID(g, store, ref); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
