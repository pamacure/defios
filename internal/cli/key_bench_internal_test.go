package cli

import (
	"encoding/hex"
	"testing"

	"defios.com/internal/cosl"
	"defios.com/internal/cosl/coslbench"
	"defios.com/internal/key"
)

// One listing of a Group's Keys, as `--key` resolution and `key inspect` do
// it: list every minted Key off the replica and resolve a public-key prefix
// among them. Since ADR 0014 a listing reads Session start, complete and
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
			prefix := hex.EncodeToString(r.SignKey)[:12]
			want := key.ConversationID("ecdsa", r.SignKey)
			got, err := conversationID(store, prefix)
			if err != nil || string(got) != string(want[:]) {
				b.Fatalf("conversationID(%s) = %x, %v; want %x", prefix, got, err, want)
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := conversationID(store, prefix); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
