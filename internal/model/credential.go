package model

// Where a Model's credential came from.
const (
	SourceEnv   = "env"   // the provider's environment variable
	SourceVault = "vault" // the Group's credentials file in the Vault
	SourceNone  = "none"
)

// CredentialEnv is the environment variable that, when set, supplies the
// credential for every Model of provider, ahead of the Vault: the TS_AUTHKEY
// precedent (ADR 0012).
func CredentialEnv(provider string) string {
	switch provider {
	case Anthropic:
		return "ANTHROPIC_API_KEY"
	case OpenAI:
		return "OPENAI_API_KEY"
	}
	return ""
}

// Credential resolves m's credential: the provider's environment variable
// when set, else the one stored in the Vault for m's alias.
func Credential(m Model, getenv func(string) string, stored map[string]string) (value, source string) {
	if name := CredentialEnv(m.Provider); name != "" {
		if v := getenv(name); v != "" {
			return v, SourceEnv
		}
	}
	if v := stored[m.Alias]; v != "" {
		return v, SourceVault
	}
	return "", SourceNone
}
