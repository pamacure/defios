package agent_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"defios.com/internal/agent"
)

// A shape's model list is at its api_path with the shape's suffix
// replaced by /models, whatever comes before it.
func TestAShapesModelsPathReplacesItsSuffix(t *testing.T) {
	for _, c := range []struct {
		shape         agent.Shape
		apiPath, want string
	}{
		{agent.Messages, "/v1/messages", "/v1/models"},
		{agent.Responses, "/v1/responses", "/v1/models"},
		{agent.ChatCompletions, "/api/v1/chat/completions", "/api/v1/models"},
		{agent.ChatCompletions, "/chat/completions", "/models"},
	} {
		if got := c.shape.ModelsPath(c.apiPath); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.shape, c.apiPath, got, c.want)
		}
	}
}

// Messages lists its models with its own auth headers, page by page,
// sorted by ID, each with the display name the list gives.
func TestMessagesListsEveryPageOfModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" || r.Header.Get("Authorization") != "" {
			t.Errorf("headers %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("after_id") {
		case "":
			fmt.Fprint(w, `{"data":[{"id":"claude-opus-5-5","display_name":"Claude Opus 5.5"}],"has_more":true,"last_id":"claude-opus-5-5"}`)
		case "claude-opus-5-5":
			fmt.Fprint(w, `{"data":[{"id":"claude-haiku-5-5","display_name":"Claude Haiku 5.5"}],"has_more":false,"last_id":"claude-haiku-5-5"}`)
		default:
			t.Errorf("after_id %q", r.URL.Query().Get("after_id"))
		}
	}))
	defer srv.Close()

	got, err := agent.Client{}.Models(context.Background(), agent.Messages, srv.URL+"/v1/models", "k")
	want := []agent.Model{{ID: "claude-haiku-5-5", Name: "Claude Haiku 5.5"}, {ID: "claude-opus-5-5", Name: "Claude Opus 5.5"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v, %v; want %#v", got, err, want)
	}
}

// The OpenAI shapes send a bearer key, and none when there is no key.
func TestOpenAIShapesListModelsWithABearerKey(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-b","object":"model"},{"id":"gpt-a","object":"model"}]}`)
	}))
	defer srv.Close()

	for _, key := range []string{"sk", ""} {
		got, err := agent.Client{}.Models(context.Background(), agent.Responses, srv.URL+"/v1/models", key)
		if want := []agent.Model{{ID: "gpt-a"}, {ID: "gpt-b"}}; err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("key %q: %#v, %v; want %#v", key, got, err, want)
		}
	}
	if want := []string{"Bearer sk", ""}; !reflect.DeepEqual(auth, want) {
		t.Fatalf("Authorization %q, want %q", auth, want)
	}
}

// A list the API refuses is an error carrying its status and message.
func TestARefusedModelListIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such route", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := agent.Client{}.Models(context.Background(), agent.ChatCompletions, srv.URL+"/v1/models", "")
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "no such route") {
		t.Fatalf("%v; want a 404 naming its message", err)
	}
}
