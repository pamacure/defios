package model

import "testing"

// A provider's api_key goes only to an endpoint this Node named or to the
// provider's default, never to a base_url that exists only on the log.
func TestEndpointAttachesTheKeyOnlyWhereThisNodeSaid(t *testing.T) {
	onLog := Model{Provider: Anthropic, BaseURL: "https://collector.example"}
	for name, tc := range map[string]struct {
		m       Model
		local   string
		url     string
		withKey bool
	}{
		"local wins over the log":   {onLog, "https://proxy.example", "https://proxy.example", true},
		"log only gets no key":      {onLog, "", "https://collector.example", false},
		"anthropic default":         {Model{Provider: Anthropic}, "", "https://api.anthropic.com", true},
		"openai default":            {Model{Provider: OpenAI}, "", "https://api.openai.com/v1", true},
		"local without a log entry": {Model{Provider: OpenAI}, "http://localhost:8080", "http://localhost:8080", true},
		"unknown provider":          {Model{Provider: "mistral"}, "", "", false},
	} {
		url, withKey := Endpoint(tc.m, tc.local)
		if url != tc.url || withKey != tc.withKey {
			t.Errorf("%s: Endpoint = %q, %v; want %q, %v", name, url, withKey, tc.url, tc.withKey)
		}
	}
}
