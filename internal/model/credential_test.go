package model

import "testing"

// The provider's variable wins over the Vault, for every alias of that
// provider; an unset or empty variable falls through to the stored one.
func TestCredentialPrefersTheProvidersEnvironmentVariable(t *testing.T) {
	stored := map[string]string{"claude": "vault-ant", "gpt": "vault-oai"}
	env := map[string]string{"ANTHROPIC_API_KEY": "env-ant", "OPENAI_API_KEY": ""}
	getenv := func(k string) string { return env[k] }

	for _, tc := range []struct {
		m           Model
		value, from string
	}{
		{Model{Alias: "claude", Provider: Anthropic}, "env-ant", SourceEnv},
		{Model{Alias: "gpt", Provider: OpenAI}, "vault-oai", SourceVault},
		{Model{Alias: "other", Provider: OpenAI}, "", SourceNone},
	} {
		value, from := Credential(tc.m, getenv, stored)
		if value != tc.value || from != tc.from {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.m.Alias, value, from, tc.value, tc.from)
		}
	}
}

func TestCredentialEnvNamesTheProvidersConventionalVariable(t *testing.T) {
	if got := CredentialEnv(Anthropic); got != "ANTHROPIC_API_KEY" {
		t.Errorf("anthropic: %q", got)
	}
	if got := CredentialEnv(OpenAI); got != "OPENAI_API_KEY" {
		t.Errorf("openai: %q", got)
	}
}
