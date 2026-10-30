package cosl_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	coslv1 "defios.com/proto/cosl/v1"
)

// Provider rows reuse ids 14–16 under their new names, and cosl log prints
// their endpoint (ADR 0019).
func TestProviderRowsDecodeAndSummarise(t *testing.T) {
	cfg, _ := proto.Marshal(&coslv1.ProviderConfig{Alias: "local", BaseUrl: "http://127.0.0.1:8080", ApiPath: "/v1/chat/completions"})
	rm, _ := proto.Marshal(&coslv1.ProviderRemove{Alias: "local"})
	for _, c := range []struct {
		typ     uint32
		id      uint32
		name    string
		payload []byte
		summary string
	}{
		{cosl.TypeProviderCreate, 14, "provider.create", cfg, "alias=local base_url=http://127.0.0.1:8080 api_path=/v1/chat/completions"},
		{cosl.TypeProviderUpdate, 15, "provider.update", cfg, "alias=local base_url=http://127.0.0.1:8080 api_path=/v1/chat/completions"},
		{cosl.TypeProviderRemove, 16, "provider.remove", rm, "alias=local"},
	} {
		if c.typ != c.id || cosl.TypeName(c.typ) != c.name {
			t.Errorf("%d: name %q", c.typ, cosl.TypeName(c.typ))
		}
		if got := cosl.Summary(c.typ, c.payload); got != c.summary {
			t.Errorf("%s summary %q, want %q", c.name, got, c.summary)
		}
	}
}
