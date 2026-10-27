package model

// DefaultBaseURL is the provider's own endpoint.
func DefaultBaseURL(provider string) string {
	switch provider {
	case Anthropic:
		return "https://api.anthropic.com"
	case OpenAI:
		return "https://api.openai.com/v1"
	}
	return ""
}

// Endpoint is where this Node calls m, given its local [providers.<p>]
// base_url (empty when unset), and whether the provider's api_key may go
// with the call. The key goes only to a local base_url or the provider's
// default, never to a base_url only the log names, because any Node in the
// Group can set that one (ADR 0013).
func Endpoint(m Model, localBaseURL string) (url string, withKey bool) {
	switch {
	case localBaseURL != "":
		return localBaseURL, true
	case m.BaseURL != "":
		return m.BaseURL, false
	}
	url = DefaultBaseURL(m.Provider)
	return url, url != ""
}
