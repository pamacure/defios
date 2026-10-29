package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"defios.com/internal/agent"
	"defios.com/internal/model"
)

// sse writes server-sent events, flushing after each.
func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		fmt.Fprint(w, e+"\n\n")
		w.(http.Flusher).Flush()
	}
}

func anthropicText(parts ...string) []string {
	out := []string{`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"m"}}`}
	for _, p := range parts {
		raw, _ := json.Marshal(p)
		out = append(out, `event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(raw)+`}}`)
	}
	return append(out, `event: message_stop`+"\n"+`data: {"type":"message_stop"}`)
}

func openaiText(parts ...string) []string {
	var out []string
	for _, p := range parts {
		raw, _ := json.Marshal(p)
		out = append(out, `data: {"choices":[{"index":0,"delta":{"content":`+string(raw)+`}}]}`)
	}
	return append(out, `data: [DONE]`)
}

// recorder collects what Generate reports as it goes.
type recorder struct{ log []string }

func (r *recorder) events() agent.Events {
	return agent.Events{
		Delta: func(s string) { r.log = append(r.log, "delta:"+s) },
		Reset: func() { r.log = append(r.log, "reset") },
	}
}

func (r *recorder) String() string { return strings.Join(r.log, " ") }

// talk is a Request's turns as the serve builds them (ADR 0018), and
// wantTalk how they read as decoded messages.
var talk = []agent.Turn{
	{Role: agent.User, Text: "alice: @auditor hi"},
	{Role: agent.Assistant, Text: "hello"},
	{Role: agent.User, Text: "alice: and?"},
}

const wantTalk = "[{user alice: @auditor hi} {assistant hello} {user alice: and?}]"

// An Anthropic Model streams its text deltas, and is called with the system
// prompt, the turns as messages in order, and the key in x-api-key.
func TestAnthropicStreamsDeltas(t *testing.T) {
	var got struct {
		Model    string                           `json:"model"`
		System   string                           `json:"system"`
		Stream   bool                             `json:"stream"`
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	var key, version, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key, version = r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		json.NewDecoder(r.Body).Decode(&got)
		sse(w, anthropicText("Hel", "lo")...)
	}))
	defer srv.Close()

	var rec recorder
	text, err := agent.Client{}.Generate(context.Background(), agent.Request{
		Provider: model.Anthropic, URL: srv.URL + "/v1/messages", APIKey: "sk-test",
		Model: "claude-x", System: "be brief", Turns: talk,
	}, rec.events())
	if err != nil {
		t.Fatal(err)
	}
	if text != "Hello" || rec.String() != "delta:Hel delta:lo" {
		t.Fatalf("text %q, events %s", text, &rec)
	}
	if path != "/v1/messages" || key != "sk-test" || version == "" {
		t.Fatalf("path %q key %q version %q", path, key, version)
	}
	if got.Model != "claude-x" || got.System != "be brief" || !got.Stream || fmt.Sprint(got.Messages) != wantTalk {
		t.Fatalf("request = %+v", got)
	}
}

// An OpenAI Model streams its content deltas, with the system prompt as the
// first message before the turns and the key as a bearer token.
func TestOpenAIStreamsDeltas(t *testing.T) {
	var got struct {
		Model    string                           `json:"model"`
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		sse(w, openaiText("Hi", " there")...)
	}))
	defer srv.Close()

	var rec recorder
	text, err := agent.Client{}.Generate(context.Background(), agent.Request{
		Provider: model.OpenAI, URL: srv.URL + "/chat/completions", APIKey: "sk-o",
		Model: "gpt-x", System: "be brief", Turns: talk,
	}, rec.events())
	if err != nil {
		t.Fatal(err)
	}
	if text != "Hi there" || rec.String() != "delta:Hi delta: there" || auth != "Bearer sk-o" {
		t.Fatalf("text %q, events %s, auth %q", text, &rec, auth)
	}
	if got.Model != "gpt-x" || len(got.Messages) != 4 || got.Messages[0] != (struct{ Role, Content string }{"system", "be brief"}) ||
		fmt.Sprint(got.Messages[1:]) != wantTalk {
		t.Fatalf("request = %+v", got)
	}
}

// With no key, no credential header is sent at all.
func TestNoKeySendsNoCredential(t *testing.T) {
	for _, p := range []string{model.Anthropic, model.OpenAI} {
		var seen []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, r.Header.Get("x-api-key"), r.Header.Get("Authorization"))
			if p == model.Anthropic {
				sse(w, anthropicText("ok")...)
			} else {
				sse(w, openaiText("ok")...)
			}
		}))
		if _, err := (agent.Client{}).Generate(context.Background(), agent.Request{Provider: p, URL: srv.URL, Model: "m", Turns: []agent.Turn{{Role: agent.User, Text: "p"}}}, agent.Events{}); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		if strings.Join(seen, "") != "" {
			t.Errorf("%s: credential headers %q", p, seen)
		}
	}
}

// failing serves fail(n) for the nth call while it returns true, then the
// reply.
func failing(t *testing.T, fail func(n int, w http.ResponseWriter) bool) (*httptest.Server, *int) {
	t.Helper()
	calls := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if fail(*calls, w) {
			return
		}
		sse(w, anthropicText("fine")...)
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

func generate(srv *httptest.Server, ev agent.Events) (string, error) {
	c := agent.Client{Backoff: time.Millisecond}
	return c.Generate(context.Background(), agent.Request{Provider: model.Anthropic, URL: srv.URL, Model: "m", Turns: []agent.Turn{{Role: agent.User, Text: "p"}}}, ev)
}

// Rate limits, overload, 5xx and dropped connections are retried; a retry
// after the stream began resets it, and one before does not.
func TestRetryableFailuresRetry(t *testing.T) {
	for name, fail := range map[string]func(w http.ResponseWriter){
		"429": func(w http.ResponseWriter) { w.Header().Set("Retry-After", "0"); w.WriteHeader(429) },
		"529": func(w http.ResponseWriter) { w.WriteHeader(529) },
		"503": func(w http.ResponseWriter) { w.WriteHeader(503) },
		"overload": func(w http.ResponseWriter) {
			sse(w, `data: {"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
		},
	} {
		srv, calls := failing(t, func(n int, w http.ResponseWriter) bool {
			if n <= 3 {
				fail(w)
				return true
			}
			return false
		})
		var rec recorder
		text, err := generate(srv, rec.events())
		if err != nil || text != "fine" || *calls != 4 || rec.String() != "delta:fine" {
			t.Errorf("%s: text %q err %v after %d calls, events %s", name, text, err, *calls, &rec)
		}
	}

	// Cut off mid-reply, twice: each retry starts over.
	srv, calls := failing(t, func(n int, w http.ResponseWriter) bool {
		if n <= 2 {
			sse(w, anthropicText("partial")[:2]...)
			return true
		}
		return false
	})
	var rec recorder
	text, err := generate(srv, rec.events())
	if err != nil || text != "fine" || *calls != 3 {
		t.Fatalf("dropped: text %q err %v after %d calls", text, err, *calls)
	}
	if rec.String() != "delta:partial reset delta:partial reset delta:fine" {
		t.Fatalf("dropped: events %s", &rec)
	}
}

// After three retries the last failure is returned. Anything else is not
// retried at all.
func TestRetriesRunOutAndOtherErrorsDoNotRetry(t *testing.T) {
	srv, calls := failing(t, func(_ int, w http.ResponseWriter) bool { w.WriteHeader(500); return true })
	if _, err := generate(srv, agent.Events{}); err == nil || *calls != 4 || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err %v after %d calls, want the 500 after 4", err, *calls)
	}
	srv, calls = failing(t, func(_ int, w http.ResponseWriter) bool {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"bad key"}`))
		return true
	})
	if _, err := generate(srv, agent.Events{}); err == nil || *calls != 1 || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("err %v after %d calls, want the 401 once", err, *calls)
	}
}

// Retry-After says how long to wait, over the backoff.
func TestRetryAfterIsHonoured(t *testing.T) {
	srv, _ := failing(t, func(n int, w http.ResponseWriter) bool {
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return true
		}
		return false
	})
	start := time.Now()
	if _, err := generate(srv, agent.Events{}); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < time.Second {
		t.Fatalf("retried after %v, want Retry-After's 1s", waited)
	}
}

// Cancelling the context ends the provider request and is not retried.
func TestCancelEndsTheRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		sse(w, anthropicText("start")[:2]...)
		cancel()
		<-r.Context().Done()
	}))
	defer srv.Close()
	_, err := agent.Client{Backoff: time.Millisecond}.Generate(ctx, agent.Request{Provider: model.Anthropic, URL: srv.URL, Model: "m", Turns: []agent.Turn{{Role: agent.User, Text: "p"}}}, agent.Events{})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err %v after %d calls, want context.Canceled once", err, calls)
	}
}
