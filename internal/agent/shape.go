package agent

import (
	"net/http"
	"strings"
)

// Shape is an API shape an agent speaks: how its request is written, how
// its reply is read, and how its key is sent (ADR 0019). A Provider's
// api_path picks it by suffix; nothing else does.
type Shape struct {
	Name   string // messages, chat_completions or responses
	Suffix string // what a Provider's api_path ends in
	auth   auth
}

func (s Shape) String() string { return s.Name }

// auth is how a shape sends its key.
type auth int

const (
	authAnthropic auth = iota + 1 // x-api-key, with anthropic-version
	authBearer                    // Authorization: Bearer
)

// anthropicVersion is the Messages API version this package speaks.
const anthropicVersion = "2023-06-01"

// set puts a's headers on h; with no key there is no credential header.
func (a auth) set(h http.Header, key string) {
	switch a {
	case authAnthropic:
		h.Set("anthropic-version", anthropicVersion)
		if key != "" {
			h.Set("x-api-key", key)
		}
	case authBearer:
		if key != "" {
			h.Set("Authorization", "Bearer "+key)
		}
	}
}

// The shapes, one row each. A shape is added as a row here plus its request
// writer and reply reader.
var (
	// Messages is Anthropic's Messages API.
	Messages = Shape{Name: "messages", Suffix: "/messages", auth: authAnthropic}
	// ChatCompletions is OpenAI's Chat Completions API.
	ChatCompletions = Shape{Name: "chat_completions", Suffix: "/chat/completions", auth: authBearer}

	// Responses is OpenAI's Responses API.
	Responses = Shape{Name: "responses", Suffix: "/responses", auth: authBearer}

	shapes = []Shape{Messages, ChatCompletions, Responses}
)

// ShapeOf is the shape whose suffix apiPath ends in; ok is false when none
// does, and such a path is invalid for a Provider.
func ShapeOf(apiPath string) (Shape, bool) {
	for _, s := range shapes {
		if strings.HasSuffix(apiPath, s.Suffix) {
			return s, true
		}
	}
	return Shape{}, false
}

// ShapeSuffixes lists the suffixes an api_path may end in, for errors.
func ShapeSuffixes() []string {
	out := make([]string, len(shapes))
	for i, s := range shapes {
		out[i] = s.Suffix
	}
	return out
}
