package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func anthropicEvent(typ, data string) string { return "event: " + typ + "\ndata: " + data }

// anthropicStart opens an Anthropic stream.
func anthropicStart() string {
	return anthropicEvent("message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":10,"output_tokens":1}}}`)
}

// anthropicEnd closes it with stop.
func anthropicEnd(stop string) []string {
	return []string{
		anthropicEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stop+`"},"usage":{"output_tokens":5}}`),
		anthropicEvent("message_stop", `{"type":"message_stop"}`),
	}
}

// anthropicTextBlock is a text block at index streamed in parts.
func anthropicTextBlock(index int, parts ...string) []string {
	out := []string{anthropicEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, index))}
	for _, p := range parts {
		out = append(out, anthropicEvent("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, index, jsonString(p))))
	}
	return append(out, anthropicEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)))
}

// anthropicToolBlock is a tool_use block at index whose input JSON streams
// in two halves.
func anthropicToolBlock(index int, id, name, input string) []string {
	half := len(input) / 2
	return []string{
		anthropicEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, index, id, name)),
		anthropicEvent("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, index, jsonString(input[:half]))),
		anthropicEvent("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, index, jsonString(input[half:]))),
		anthropicEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)),
	}
}

func sayInput(text string) string {
	raw, _ := json.Marshal(map[string]string{"text": text})
	return string(raw)
}

// anthropicSays streams thought as text, then a say of text.
func anthropicSays(thought, text string) []string {
	out := []string{anthropicStart()}
	out = append(out, anthropicTextBlock(0, thought)...)
	out = append(out, anthropicToolBlock(1, "toolu_1", "say", sayInput(text))...)
	return append(out, anthropicEnd("tool_use")...)
}

// anthropicText streams parts as one text block and ends the turn.
func anthropicText(parts ...string) []string {
	out := append([]string{anthropicStart()}, anthropicTextBlock(0, parts...)...)
	return append(out, anthropicEnd("end_turn")...)
}

func openaiChunk(delta string, finish string) string {
	f := "null"
	if finish != "" {
		f = `"` + finish + `"`
	}
	return `data: {"choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + f + `}]}`
}

const openaiUsage = `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`

// openaiSays streams thought as content, then a say of text.
func openaiSays(thought, text string) []string {
	in := sayInput(text)
	half := len(in) / 2
	return []string{
		openaiChunk(`{"role":"assistant","content":`+jsonString(thought)+`}`, ""),
		openaiChunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"say","arguments":`+jsonString(in[:half])+`}}]}`, ""),
		openaiChunk(`{"tool_calls":[{"index":0,"function":{"arguments":`+jsonString(in[half:])+`}}]}`, ""),
		openaiChunk(`{}`, "tool_calls"),
		openaiUsage,
		`data: [DONE]`,
	}
}

func openaiText(parts ...string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, openaiChunk(`{"content":`+jsonString(p)+`}`, ""))
	}
	return append(out, openaiChunk(`{}`, "stop"), openaiUsage, `data: [DONE]`)
}

// recorder collects what Run reports as it goes.
type recorder struct {
	mu      sync.Mutex
	log     []string
	records []agent.Record
}

func (r *recorder) events() agent.Events {
	return agent.Events{
		Delta: func(s string) { r.mu.Lock(); r.log = append(r.log, "delta:"+s); r.mu.Unlock() },
		Reset: func() { r.mu.Lock(); r.log = append(r.log, "reset"); r.mu.Unlock() },
		Record: func(rec agent.Record) {
			r.mu.Lock()
			r.records = append(r.records, rec)
			r.mu.Unlock()
		},
	}
}

func (r *recorder) String() string { return strings.Join(r.log, " ") }

// kinds is the records' kinds in order.
func (r *recorder) kinds() string {
	var out []string
	for _, rec := range r.records {
		out = append(out, rec.Kind)
	}
	return strings.Join(out, " ")
}

// script is a fake provider answering the nth call with replies[n-1] (the
// last one again past the end), recording each request body and its
// headers.
type script struct {
	*httptest.Server
	mu      sync.Mutex
	bodies  []map[string]any
	headers []http.Header
}

func newScript(t *testing.T, replies ...[]string) *script {
	t.Helper()
	s := &script{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.headers = append(s.headers, r.Header.Clone())
		n := len(s.bodies)
		s.mu.Unlock()
		sse(w, replies[min(n, len(replies))-1]...)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *script) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *script) body(n int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[n-1]
}

func run(s *script, provider string, ev agent.Events) (agent.Result, error) {
	return agent.Client{Backoff: time.Millisecond}.Run(context.Background(), agent.Request{
		Provider: provider, URL: s.URL, APIKey: "sk-secret", Model: "m", System: "be brief\n", SystemDepth: "at depth 1\n", Turns: talk,
	}, ev)
}

func messages(body map[string]any) []any { m, _ := body["messages"].([]any); return m }

// talk is a Request's turns as the serve builds them (ADR 0018), and
// wantTalk how they read as sent.
var talk = []agent.Turn{
	{Role: agent.User, Text: "alice: @auditor hi"},
	{Role: agent.Assistant, Text: "hello"},
	{Role: agent.User, Text: "alice: and?"},
}

const wantTalk = "[map[content:alice: @auditor hi role:user] map[content:hello role:assistant] map[content:alice: and? role:user]]"

// wantAnthropicTalk is talk as an Anthropic request sends it: the latest
// turn as a block carrying a cache breakpoint.
const wantAnthropicTalk = "[map[content:alice: @auditor hi role:user] map[content:hello role:assistant] " +
	"map[content:[map[cache_control:map[type:ephemeral] text:alice: and? type:text]] role:user]]"

// An Anthropic run streams its text, sends the turns in order, offers say
// as its only tool with max_tokens 8192, and a say commits exactly its
// text: the text outside it is never the result.
func TestAnthropicSayIsTheResult(t *testing.T) {
	s := newScript(t, anthropicSays("let me think", "all good"))
	var rec recorder
	res, err := run(s, model.Anthropic, rec.events())
	if err != nil {
		t.Fatal(err)
	}
	if res.Silent || res.Said != "all good" {
		t.Fatalf("result = %+v", res)
	}
	if rec.String() != "delta:let me think" {
		t.Fatalf("events %s", &rec)
	}
	h := s.headers[0]
	if h.Get("x-api-key") != "sk-secret" || h.Get("anthropic-version") == "" {
		t.Fatalf("headers %v", h)
	}
	body := s.body(1)
	if body["model"] != "m" || body["stream"] != true || body["max_tokens"] != float64(8192) {
		t.Fatalf("body = %v", body)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "say" {
		t.Fatalf("tools = %v", body["tools"])
	}
	if msgs := messages(body); fmt.Sprint(msgs) != wantAnthropicTalk {
		t.Fatalf("messages = %v", msgs)
	}
	if s.calls() != 1 {
		t.Fatalf("%d calls after the say, want 1", s.calls())
	}
}

// An OpenAI run does the same over Chat Completions: the system prompt as
// the first message before the turns, the key as a bearer token, say as a function tool and
// max_completion_tokens 8192.
func TestOpenAISayIsTheResult(t *testing.T) {
	s := newScript(t, openaiSays("hmm", "all good"))
	var rec recorder
	res, err := run(s, model.OpenAI, rec.events())
	if err != nil {
		t.Fatal(err)
	}
	if res.Silent || res.Said != "all good" || rec.String() != "delta:hmm" {
		t.Fatalf("result = %+v, events %s", res, &rec)
	}
	if s.headers[0].Get("Authorization") != "Bearer sk-secret" {
		t.Fatalf("headers %v", s.headers[0])
	}
	body := s.body(1)
	if body["max_completion_tokens"] != float64(8192) {
		t.Fatalf("body = %v", body)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "say" {
		t.Fatalf("tools = %v", body["tools"])
	}
	msgs := messages(body)
	if len(msgs) != 4 || fmt.Sprint(msgs[0]) != "map[content:be brief\nat depth 1\n role:system]" || fmt.Sprint(msgs[1:]) != wantTalk {
		t.Fatalf("messages = %v", msgs)
	}
}

// A say over 4096 bytes, or an empty one, is refused with a tool error
// saying why; the next call carries everything so far, and a shorter say
// is the result. Both providers.
func TestARefusedSayIsRewritten(t *testing.T) {
	long := strings.Repeat("a", 4097)
	for _, p := range []string{model.Anthropic, model.OpenAI} {
		says := anthropicSays
		if p == model.OpenAI {
			says = openaiSays
		}
		s := newScript(t, says("first", long), says("second", ""), says("third", "short"))
		res, err := run(s, p, agent.Events{})
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if res.Said != "short" || s.calls() != 3 {
			t.Fatalf("%s: result %+v after %d calls", p, res, s.calls())
		}
		second, _ := json.Marshal(messages(s.body(2)))
		for _, want := range []string{"first", "4097 bytes", "4096", `"is_error":true`} {
			if p == model.OpenAI && want == `"is_error":true` {
				want = `"role":"tool"`
			}
			if !strings.Contains(string(second), want) {
				t.Errorf("%s: second call lacks %q: %s", p, want, second)
			}
		}
		third, _ := json.Marshal(messages(s.body(3)))
		if !strings.Contains(string(third), "first") || !strings.Contains(string(third), "empty") {
			t.Errorf("%s: third call lacks the run so far: %s", p, third)
		}
	}
}

// An Anthropic run sends the system prompt as blocks, the stable part first
// with a cache breakpoint after it and the depth part last, and a breakpoint
// on the latest turn: the talk's last turn, then the latest tool result,
// which takes the breakpoint from the one before it (ADR 0018).
func TestAnthropicMarksCacheBreakpoints(t *testing.T) {
	s := newScript(t, anthropicSays("first", ""), anthropicSays("second", ""), anthropicSays("third", "done"))
	if _, err := run(s, model.Anthropic, agent.Events{}); err != nil {
		t.Fatal(err)
	}
	const wantSystem = "[map[cache_control:map[type:ephemeral] text:be brief\n type:text] map[text:at depth 1\n type:text]]"
	for n := 1; n <= 3; n++ {
		if got := fmt.Sprint(s.body(n)["system"]); got != wantSystem {
			t.Fatalf("call %d system = %s", n, got)
		}
	}
	marked := func(n int) []int {
		var at []int
		for i, m := range messages(s.body(n)) {
			raw, _ := json.Marshal(m)
			if strings.Contains(string(raw), "cache_control") {
				at = append(at, i)
			}
		}
		return at
	}
	for n, want := range map[int]string{1: "[2]", 2: "[2 4]", 3: "[2 6]"} {
		if got := fmt.Sprint(marked(n)); got != want {
			t.Errorf("call %d marks messages %s, want %s", n, got, want)
		}
	}
	last, _ := json.Marshal(messages(s.body(3))[6])
	if !strings.Contains(string(last), `"type":"tool_result"`) {
		t.Fatalf("latest turn = %s", last)
	}
}

// Each call of a run sends the call before it as an unchanged prefix: the
// same system prompt and tools, and its messages first. Both providers.
func TestASecondCallExtendsTheFirst(t *testing.T) {
	for _, p := range []string{model.Anthropic, model.OpenAI} {
		says := anthropicSays
		if p == model.OpenAI {
			says = openaiSays
		}
		s := newScript(t, says("first", ""), says("second", "done"))
		if _, err := run(s, p, agent.Events{}); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		first, second := s.body(1), s.body(2)
		m1, m2 := messages(first), messages(second)
		if len(m2) <= len(m1) || fmt.Sprint(m2[:len(m1)]) != fmt.Sprint(m1) {
			t.Errorf("%s: second call's messages do not begin with the first's:\n%v\n%v", p, m1, m2)
		}
		for k, v := range first {
			if k != "messages" && fmt.Sprint(second[k]) != fmt.Sprint(v) {
				t.Errorf("%s: %s changed: %v, then %v", p, k, v, second[k])
			}
		}
	}
}

// Ending the turn without say is silence. Both providers.
func TestEndingWithoutSayIsSilent(t *testing.T) {
	for p, reply := range map[string][]string{model.Anthropic: anthropicText("nothing to add"), model.OpenAI: openaiText("nothing to add")} {
		s := newScript(t, reply)
		res, err := run(s, p, agent.Events{})
		if err != nil || !res.Silent || res.Said != "" || s.calls() != 1 {
			t.Fatalf("%s: result %+v, err %v, after %d calls", p, res, err, s.calls())
		}
	}
}

// A call that stops at max_tokens is an error.
func TestMaxTokensIsAnError(t *testing.T) {
	anth := append([]string{anthropicStart()}, anthropicTextBlock(0, "on and on")...)
	anth = append(anth, anthropicEnd("max_tokens")...)
	oai := []string{openaiChunk(`{"content":"on and on"}`, ""), openaiChunk(`{}`, "length"), `data: [DONE]`}
	for p, reply := range map[string][]string{model.Anthropic: anth, model.OpenAI: oai} {
		s := newScript(t, reply)
		if _, err := run(s, p, agent.Events{}); err == nil || !strings.Contains(err.Error(), "max_tokens") || s.calls() != 1 {
			t.Fatalf("%s: err %v after %d calls", p, err, s.calls())
		}
	}
}

// A run that reaches 8 calls without an accepted say is an error.
func TestEightCallsWithoutSayIsAnError(t *testing.T) {
	s := newScript(t, anthropicSays("again", ""))
	_, err := run(s, model.Anthropic, agent.Events{})
	if err == nil || !strings.Contains(err.Error(), "8 model calls") || s.calls() != 8 {
		t.Fatalf("err %v after %d calls", err, s.calls())
	}
}

// A run records each request body as sent, each output block and each
// call's usage, and never the key.
func TestRunRecordsRequestsBlocksAndUsage(t *testing.T) {
	s := newScript(t, anthropicSays("first", ""), anthropicSays("second", "done"))
	var rec recorder
	if _, err := run(s, model.Anthropic, rec.events()); err != nil {
		t.Fatal(err)
	}
	if got := rec.kinds(); got != "request block block usage request block block usage" {
		t.Fatalf("records = %s", got)
	}
	var sent map[string]any
	if err := json.Unmarshal(rec.records[4].Data, &sent); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sent) != fmt.Sprint(s.body(2)) {
		t.Fatalf("recorded request %v, sent %v", sent, s.body(2))
	}
	if !strings.Contains(string(rec.records[2].Data), `"say"`) || !strings.Contains(string(rec.records[3].Data), "output_tokens") {
		t.Fatalf("records = %s, %s", rec.records[2].Data, rec.records[3].Data)
	}
	for _, r := range rec.records {
		if strings.Contains(string(r.Data), "sk-secret") {
			t.Fatalf("a record holds the key: %s", r.Data)
		}
	}
}

// With no key, no credential header is sent at all.
func TestNoKeySendsNoCredential(t *testing.T) {
	for p, reply := range map[string][]string{model.Anthropic: anthropicText("ok"), model.OpenAI: openaiText("ok")} {
		s := newScript(t, reply)
		if _, err := (agent.Client{}).Run(context.Background(), agent.Request{Provider: p, URL: s.URL, Model: "m", Turns: []agent.Turn{{Role: agent.User, Text: "p"}}}, agent.Events{}); err != nil {
			t.Fatal(err)
		}
		if h := s.headers[0]; h.Get("x-api-key")+h.Get("Authorization") != "" {
			t.Errorf("%s: credential headers %v", p, h)
		}
	}
}

// failing serves fail(n) for the nth call while it returns true, then a say
// of "fine".
func failing(t *testing.T, fail func(n int, w http.ResponseWriter) bool) (*httptest.Server, *int) {
	t.Helper()
	calls := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if fail(*calls, w) {
			return
		}
		sse(w, anthropicSays("ok", "fine")...)
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

func generate(srv *httptest.Server, ev agent.Events) (agent.Result, error) {
	c := agent.Client{Backoff: time.Millisecond}
	return c.Run(context.Background(), agent.Request{Provider: model.Anthropic, URL: srv.URL, Model: "m", Turns: []agent.Turn{{Role: agent.User, Text: "p"}}}, ev)
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
		res, err := generate(srv, rec.events())
		if err != nil || res.Said != "fine" || *calls != 4 || rec.String() != "delta:ok" {
			t.Errorf("%s: result %+v err %v after %d calls, events %s", name, res, err, *calls, &rec)
		}
	}

	// Cut off mid-reply, twice: each retry starts over.
	srv, calls := failing(t, func(n int, w http.ResponseWriter) bool {
		if n <= 2 {
			sse(w, anthropicText("partial")[:3]...)
			return true
		}
		return false
	})
	var rec recorder
	res, err := generate(srv, rec.events())
	if err != nil || res.Said != "fine" || *calls != 3 {
		t.Fatalf("dropped: result %+v err %v after %d calls", res, err, *calls)
	}
	if rec.String() != "delta:partial reset delta:partial reset delta:ok" {
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
		sse(w, anthropicText("start")[:3]...)
		cancel()
		<-r.Context().Done()
	}))
	defer srv.Close()
	_, err := agent.Client{Backoff: time.Millisecond}.Run(ctx, agent.Request{Provider: model.Anthropic, URL: srv.URL, Model: "m", Turns: []agent.Turn{{Role: agent.User, Text: "p"}}}, agent.Events{})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err %v after %d calls, want context.Canceled once", err, calls)
	}
}
