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

func talkWriteInput(text string) string {
	raw, _ := json.Marshal(map[string]string{"text": text})
	return string(raw)
}

// anthropicSays streams thought as text, then a talk_write of text.
func anthropicSays(thought, text string) []string {
	out := []string{anthropicStart()}
	out = append(out, anthropicTextBlock(0, thought)...)
	out = append(out, anthropicToolBlock(1, "toolu_1", "talk_write", talkWriteInput(text))...)
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

// openaiSays streams thought as content, then a talk_write of text.
func openaiSays(thought, text string) []string {
	in := talkWriteInput(text)
	half := len(in) / 2
	return []string{
		openaiChunk(`{"role":"assistant","content":`+jsonString(thought)+`}`, ""),
		openaiChunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"talk_write","arguments":`+jsonString(in[:half])+`}}]}`, ""),
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

// responsesEvent is a Responses stream event of typ with fields, a JSON
// object's members.
func responsesEvent(typ, fields string) string {
	if fields != "" {
		fields = "," + fields
	}
	return "event: " + typ + "\ndata: {\"type\":\"" + typ + "\"" + fields + "}"
}

const responsesUsage = `"usage":{"input_tokens":10,"output_tokens":5,"output_tokens_details":{"reasoning_tokens":2}}`

// responsesEnd closes a Responses stream as completed.
func responsesEnd() string {
	return responsesEvent("response.completed", `"response":{"id":"resp_1","status":"completed",`+responsesUsage+`}`)
}

// responsesMessage is an output message item at index streaming parts.
func responsesMessage(index int, parts ...string) []string {
	out := []string{responsesEvent("response.output_item.added", fmt.Sprintf(`"output_index":%d,"item":{"id":"msg_%d","type":"message","role":"assistant","status":"in_progress","content":[]}`, index, index))}
	for _, p := range parts {
		out = append(out, responsesEvent("response.output_text.delta", fmt.Sprintf(`"output_index":%d,"item_id":"msg_%d","content_index":0,"delta":%s`, index, index, jsonString(p))))
	}
	text := jsonString(strings.Join(parts, ""))
	return append(out, responsesEvent("response.output_item.done", fmt.Sprintf(`"output_index":%d,"item":{"id":"msg_%d","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":%s,"annotations":[]}]}`, index, index, text)))
}

// responsesCall is a function_call item at index whose arguments stream in
// two halves.
func responsesCall(index int, callID, name, args string) []string {
	half := len(args) / 2
	item := func(status, a string) string {
		return fmt.Sprintf(`{"id":"fc_%d","type":"function_call","status":%q,"call_id":%q,"name":%q,"arguments":%s}`, index, status, callID, name, jsonString(a))
	}
	return []string{
		responsesEvent("response.output_item.added", fmt.Sprintf(`"output_index":%d,"item":%s`, index, item("in_progress", ""))),
		responsesEvent("response.function_call_arguments.delta", fmt.Sprintf(`"output_index":%d,"item_id":"fc_%d","delta":%s`, index, index, jsonString(args[:half]))),
		responsesEvent("response.function_call_arguments.delta", fmt.Sprintf(`"output_index":%d,"item_id":"fc_%d","delta":%s`, index, index, jsonString(args[half:]))),
		responsesEvent("response.output_item.done", fmt.Sprintf(`"output_index":%d,"item":%s`, index, item("completed", args))),
	}
}

// responsesReasoning is a reasoning item at index whose summary streams in
// parts, carrying encrypted content.
func responsesReasoning(index int, encrypted string, parts ...string) []string {
	out := []string{responsesEvent("response.output_item.added", fmt.Sprintf(`"output_index":%d,"item":{"id":"rs_%d","type":"reasoning","summary":[]}`, index, index))}
	for _, p := range parts {
		out = append(out, responsesEvent("response.reasoning_summary_text.delta", fmt.Sprintf(`"output_index":%d,"item_id":"rs_%d","summary_index":0,"delta":%s`, index, index, jsonString(p))))
	}
	summary := jsonString(strings.Join(parts, ""))
	return append(out, responsesEvent("response.output_item.done", fmt.Sprintf(`"output_index":%d,"item":{"id":"rs_%d","type":"reasoning","summary":[{"type":"summary_text","text":%s}],"encrypted_content":%q}`, index, index, summary, encrypted)))
}

// responsesSays streams thought as a message, then a talk_write of text.
func responsesSays(thought, text string) []string {
	out := []string{responsesEvent("response.created", `"response":{"id":"resp_1","status":"in_progress"}`)}
	out = append(out, responsesMessage(0, thought)...)
	out = append(out, responsesCall(1, "call_1", "talk_write", talkWriteInput(text))...)
	return append(out, responsesEnd())
}

// responsesText streams parts as one message and ends the turn.
func responsesText(parts ...string) []string {
	return append(responsesMessage(0, parts...), responsesEnd())
}

// jsonMark marks a script reply as one JSON body, not an event stream.
const jsonMark = "json:"

// asJSON is a script reply of body as a JSON reply that is not streamed.
func asJSON(body string) []string { return []string{jsonMark + body} }

// jsonSays is each shape's JSON reply that thinks thought, writes text,
// then says said: the same blocks as the streamed fakes, all at once.
var jsonSays = map[agent.Shape]func(thought, text, said string) []string{
	agent.Messages: func(thought, text, said string) []string {
		return asJSON(`{"id":"m","type":"message","role":"assistant","content":[` +
			`{"type":"thinking","thinking":` + jsonString(thought) + `,"signature":"sig"},` +
			`{"type":"text","text":` + jsonString(text) + `},` +
			`{"type":"tool_use","id":"toolu_1","name":"talk_write","input":` + talkWriteInput(said) + `}],` +
			`"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`)
	},
	agent.ChatCompletions: func(thought, text, said string) []string {
		return asJSON(`{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":` + jsonString(text) + `,` +
			`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"talk_write","arguments":` + jsonString(talkWriteInput(said)) + `}}]},` +
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	},
	agent.Responses: func(thought, text, said string) []string {
		return asJSON(`{"id":"resp_1","object":"response","status":"completed","output":[` +
			`{"id":"rs_0","type":"reasoning","summary":[{"type":"summary_text","text":` + jsonString(thought) + `}],"encrypted_content":"gAAAA-enc"},` +
			`{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":` + jsonString(text) + `,"annotations":[]}]},` +
			`{"id":"fc_2","type":"function_call","status":"completed","call_id":"call_1","name":"talk_write","arguments":` + jsonString(talkWriteInput(said)) + `}],` +
			responsesUsage + `}`)
	},
}

// says is each shape's fake for a run that thinks thought and says text.
var says = map[agent.Shape]func(thought, text string) []string{
	agent.Messages: anthropicSays, agent.ChatCompletions: openaiSays, agent.Responses: responsesSays,
}

// texts is each shape's fake for a turn that ends without a talk_write.
var texts = map[agent.Shape]func(parts ...string) []string{
	agent.Messages: anthropicText, agent.ChatCompletions: openaiText, agent.Responses: responsesText,
}

// callsTool is each shape's fake for a turn that calls the tool name with
// input, a JSON object, and nothing else.
var callsTool = map[agent.Shape]func(name, input string) []string{
	agent.Messages: func(name, input string) []string {
		out := append([]string{anthropicStart()}, anthropicToolBlock(0, "toolu_7", name, input)...)
		return append(out, anthropicEnd("tool_use")...)
	},
	agent.ChatCompletions: func(name, input string) []string {
		return []string{
			openaiChunk(`{"role":"assistant","tool_calls":[{"index":0,"id":"call_7","type":"function","function":{"name":`+jsonString(name)+`,"arguments":`+jsonString(input)+`}}]}`, ""),
			openaiChunk(`{}`, "tool_calls"), openaiUsage, `data: [DONE]`,
		}
	},
	agent.Responses: func(name, input string) []string {
		return append(responsesCall(0, "call_7", name, input), responsesEnd())
	},
}

// recorder collects what Run reports as it goes, consecutive deltas of a
// kind as one "kind:text" entry.
type recorder struct {
	mu      sync.Mutex
	log     []string
	kind    string // of the last entry, while it is a delta
	records []agent.Record
}

func (r *recorder) delta(kind, s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.kind == kind {
		r.log[len(r.log)-1] += s
		return
	}
	r.log, r.kind = append(r.log, kind+":"+s), kind
}

func (r *recorder) events() agent.Events {
	return agent.Events{
		Delta: r.delta,
		Reset: func() { r.mu.Lock(); r.log, r.kind = append(r.log, "reset"), ""; r.mu.Unlock() },
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
		reply := replies[min(n, len(replies))-1]
		if len(reply) == 1 && strings.HasPrefix(reply[0], jsonMark) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, strings.TrimPrefix(reply[0], jsonMark))
			return
		}
		sse(w, reply...)
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

// run runs a Request offering talk_write alone against s.
func run(s *script, shape agent.Shape, ev agent.Events) (agent.Result, error) {
	return runWith(s, shape, ev, agent.TalkWrite)
}

// runWith runs a Request offering tools against s.
func runWith(s *script, shape agent.Shape, ev agent.Events, tools ...agent.Tool) (agent.Result, error) {
	return agent.Client{Backoff: time.Millisecond}.Run(context.Background(), agent.Request{
		Shape: shape, URL: s.URL, APIKey: "sk-secret", Body: map[string]any{"model": "m"}, System: "be brief\n", Earlier: earlier, Turn: turn,
		Tools: tools,
	}, ev)
}

// toolNames is the names of the tools body offers, in order.
func toolNames(body map[string]any) string {
	var names []string
	tools, _ := body["tools"].([]any)
	for _, raw := range tools {
		t, _ := raw.(map[string]any)
		if f, ok := t["function"].(map[string]any); ok {
			t = f
		}
		names = append(names, fmt.Sprint(t["name"]))
	}
	return strings.Join(names, " ")
}

// resultBlocks is the tool results among rec's blocks.
func (r *recorder) resultBlocks() []agent.ToolResult {
	var out []agent.ToolResult
	for _, rec := range r.records {
		var res agent.ToolResult
		if rec.Kind == agent.RecordBlock && json.Unmarshal(rec.Data, &res) == nil && res.Type == "tool_result" {
			out = append(out, res)
		}
	}
	return out
}

// messages is a request's conversation: its messages, or its input items.
func messages(body map[string]any) []any {
	if in, ok := body["input"].([]any); ok {
		return in
	}
	m, _ := body["messages"].([]any)
	return m
}

// earlier is a Request's replay as the serve builds it (ADR 0036): one
// earlier run that said hello, then turn, this run's; wantTalk is how they
// read as sent.
var earlier = []agent.Earlier{{
	Turn:   "alice: @auditor hi",
	Blocks: []json.RawMessage{json.RawMessage(`{"type":"tool_use","id":"toolu_0","name":"talk_write","input":{"text":"hello"}}`)},
	Posted: true,
}}

const turn = "alice: and?"

// wantAnthropicTalk is the replay and the turn as an Anthropic request
// sends them: the earlier run's turn and its talk_write, then one user
// message answering that call and carrying the turn, with a cache
// breakpoint where the replay ends and one on the turn.
const wantAnthropicTalk = "[map[content:[map[cache_control:map[type:ephemeral] text:alice: @auditor hi type:text]] role:user] " +
	"map[content:[map[id:toolu_0 input:map[text:hello] name:talk_write type:tool_use]] role:assistant] " +
	"map[content:[map[content:Posted. Your run ended there. is_error:false tool_use_id:toolu_0 type:tool_result] " +
	"map[cache_control:map[type:ephemeral] text:alice: and? type:text]] role:user]]"

// A Messages run streams its text, sends the turns in order, offers talk_write
// as its only tool, and a talk_write commits exactly its text: the text outside it
// is never the result. DeFiOS writes no model, stream or max-token field:
// those are the profile's body (ADR 0019).
func TestAnthropicSayIsTheResult(t *testing.T) {
	s := newScript(t, anthropicSays("let me think", "all good"))
	var rec recorder
	res, err := run(s, agent.Messages, rec.events())
	if err != nil {
		t.Fatal(err)
	}
	if res.Silent || res.Said != "all good" {
		t.Fatalf("result = %+v", res)
	}
	if rec.String() != `text:let me think tool:talk_write {"text":"all good"}` {
		t.Fatalf("events %s", &rec)
	}
	h := s.headers[0]
	if h.Get("x-api-key") != "sk-secret" || h.Get("anthropic-version") == "" {
		t.Fatalf("headers %v", h)
	}
	body := s.body(1)
	if body["model"] != "m" || len(body) != 4 {
		t.Fatalf("body = %v, want model, system, messages and tools only", body)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "talk_write" {
		t.Fatalf("tools = %v", body["tools"])
	}
	if msgs := messages(body); fmt.Sprint(msgs) != wantAnthropicTalk {
		t.Fatalf("messages = %v", msgs)
	}
	if s.calls() != 1 {
		t.Fatalf("%d calls after the talk_write, want 1", s.calls())
	}
}

// Every block streams with its kind as it is generated: thinking, text,
// and each tool call (a talk_write like any other) as its name and its input.
// Consecutive tool calls start on a new line; redacted thinking streams
// nothing.
func TestAnthropicStreamsEveryBlockWithItsKind(t *testing.T) {
	reply := []string{anthropicStart(),
		anthropicEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm, "}}`),
		anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"ok"}}`),
		anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`),
		anthropicEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		anthropicEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"xyz"}}`),
		anthropicEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
	}
	reply = append(reply, anthropicTextBlock(2, "so")...)
	reply = append(reply, anthropicToolBlock(3, "toolu_1", "talk_write", "{}")...)
	reply = append(reply, anthropicToolBlock(4, "toolu_2", "talk_write", talkWriteInput("done"))...)
	reply = append(reply, anthropicEnd("tool_use")...)
	s := newScript(t, reply)
	var rec recorder
	if res, err := run(s, agent.Messages, rec.events()); err != nil || res.Said != "done" {
		t.Fatalf("result %+v err %v", res, err)
	}
	if want := `thinking:hmm, ok text:so tool:talk_write {}` + "\n" + `talk_write {"text":"done"}`; rec.String() != want {
		t.Fatalf("events %s\nwant   %s", &rec, want)
	}
}

// A Chat Completions run does the same: the system prompt as the first
// message before the turns, the key as a bearer token, talk_write as a function
// tool, and nothing else but the profile's body.
func TestOpenAISayIsTheResult(t *testing.T) {
	s := newScript(t, openaiSays("hmm", "all good"))
	var rec recorder
	res, err := run(s, agent.ChatCompletions, rec.events())
	if err != nil {
		t.Fatal(err)
	}
	if res.Silent || res.Said != "all good" || rec.String() != `text:hmm tool:talk_write {"text":"all good"}` {
		t.Fatalf("result = %+v, events %s", res, &rec)
	}
	if s.headers[0].Get("Authorization") != "Bearer sk-secret" {
		t.Fatalf("headers %v", s.headers[0])
	}
	body := s.body(1)
	if body["model"] != "m" || len(body) != 3 {
		t.Fatalf("body = %v, want model, messages and tools only", body)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "talk_write" {
		t.Fatalf("tools = %v", body["tools"])
	}
	msgs := messages(body)
	const wantTalk = "[map[content:alice: @auditor hi role:user] " +
		"map[role:assistant tool_calls:[map[function:map[arguments:{\"text\":\"hello\"} name:talk_write] id:toolu_0 type:function]]] " +
		"map[content:Posted. Your run ended there. role:tool tool_call_id:toolu_0] map[content:alice: and? role:user]]"
	if len(msgs) != 5 || fmt.Sprint(msgs[0]) != "map[content:be brief\n role:system]" || fmt.Sprint(msgs[1:]) != wantTalk {
		t.Fatalf("messages = %v", msgs)
	}
}

// A Responses run sends the system prompt and the turns as input items,
// the key as a bearer token, talk_write as a flat function tool, and nothing else
// but the profile's body; text and the talk_write stream as they come.
func TestResponsesSayIsTheResult(t *testing.T) {
	s := newScript(t, responsesSays("hmm", "all good"))
	var rec recorder
	res, err := run(s, agent.Responses, rec.events())
	if err != nil {
		t.Fatal(err)
	}
	if res.Silent || res.Said != "all good" || rec.String() != `text:hmm tool:talk_write {"text":"all good"}` {
		t.Fatalf("result = %+v, events %s", res, &rec)
	}
	if s.headers[0].Get("Authorization") != "Bearer sk-secret" || s.headers[0].Get("x-api-key") != "" {
		t.Fatalf("headers %v", s.headers[0])
	}
	body := s.body(1)
	if body["model"] != "m" || len(body) != 3 {
		t.Fatalf("body = %v, want model, input and tools only", body)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", body["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "talk_write" || tool["description"] == nil || tool["parameters"] == nil || tool["function"] != nil {
		t.Fatalf("tool = %v, want a flat function tool", tool)
	}
	in := messages(body)
	const wantTalk = "[map[content:alice: @auditor hi role:user] " +
		"map[arguments:{\"text\":\"hello\"} call_id:toolu_0 name:talk_write type:function_call] " +
		"map[call_id:toolu_0 output:Posted. Your run ended there. type:function_call_output] map[content:alice: and? role:user]]"
	if len(in) != 5 || fmt.Sprint(in[0]) != "map[content:be brief\n role:system]" || fmt.Sprint(in[1:]) != wantTalk {
		t.Fatalf("input = %v", in)
	}
}

// Reasoning streams as thinking and is kept on the run's blocks; the next
// call of the run replays every output item as it came back, the reasoning
// with its encrypted content, followed by the answer to the refused talk_write as
// a function_call_output.
func TestResponsesReplaysItsOutputItems(t *testing.T) {
	first := append([]string{}, responsesReasoning(0, "gAAAA-secret-thoughts", "weighing ", "it")...)
	first = append(first, responsesCall(1, "call_9", "talk_write", talkWriteInput(""))...)
	first = append(first, responsesEnd())
	s := newScript(t, first, responsesSays("again", "done"))
	var rec recorder
	res, err := run(s, agent.Responses, rec.events())
	if err != nil || res.Said != "done" {
		t.Fatalf("result %+v err %v", res, err)
	}
	if want := `thinking:weighing it tool:talk_write {"text":""} text:again tool:talk_write {"text":"done"}`; rec.String() != want {
		t.Fatalf("events %s\nwant   %s", &rec, want)
	}
	in := messages(s.body(2))
	if len(in) != 8 {
		t.Fatalf("second call's input = %v", in)
	}
	if fmt.Sprint(in[:5]) != fmt.Sprint(messages(s.body(1))) {
		t.Fatalf("second call's input does not begin with the first's: %v", in)
	}
	reasoning, call, output := in[5].(map[string]any), in[6].(map[string]any), in[7].(map[string]any)
	if reasoning["type"] != "reasoning" || reasoning["id"] != "rs_0" || reasoning["encrypted_content"] != "gAAAA-secret-thoughts" ||
		fmt.Sprint(reasoning["summary"]) != "[map[text:weighing it type:summary_text]]" {
		t.Fatalf("reasoning replayed as %v", reasoning)
	}
	if call["type"] != "function_call" || call["call_id"] != "call_9" || call["name"] != "talk_write" || call["arguments"] != talkWriteInput("") {
		t.Fatalf("function call replayed as %v", call)
	}
	if output["type"] != "function_call_output" || output["call_id"] != "call_9" || !strings.Contains(fmt.Sprint(output["output"]), "empty") {
		t.Fatalf("refusal = %v", output)
	}
	if got := rec.kinds(); got != "request block block usage block request block block usage" {
		t.Fatalf("records = %s", got)
	}
	if b := string(rec.records[1].Data); !strings.Contains(b, `"type":"reasoning"`) || !strings.Contains(b, "weighing it") {
		t.Fatalf("reasoning block recorded as %s", b)
	}
	if u := string(rec.records[3].Data); !strings.Contains(u, `"input_tokens":10`) || !strings.Contains(u, "reasoning_tokens") {
		t.Fatalf("usage recorded as %s", u)
	}
}

// A Responses reply that comes back incomplete is an error: at its token
// bound like the other shapes, and for any other reason too.
func TestAnIncompleteResponseIsAnError(t *testing.T) {
	for reason, want := range map[string]string{"max_output_tokens": "token bound", "content_filter": "content_filter"} {
		reply := append(responsesMessage(0, "on and on"), responsesEvent("response.incomplete",
			`"response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"`+reason+`"},`+responsesUsage+`}`))
		s := newScript(t, reply)
		var rec recorder
		_, err := run(s, agent.Responses, rec.events())
		if err == nil || !strings.Contains(err.Error(), want) || s.calls() != 1 {
			t.Fatalf("%s: err %v after %d calls", reason, err, s.calls())
		}
		if !strings.Contains(rec.kinds(), "usage") {
			t.Errorf("%s: records = %s, want the usage", reason, rec.kinds())
		}
	}
}

// A Responses stream that fails says why; a server error is retried, and a
// stream that ends without completing was cut off.
func TestAFailedResponseIsAnError(t *testing.T) {
	failed := []string{responsesEvent("response.failed", `"response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"boom"}}`)}
	s := newScript(t, failed, failed, failed, responsesSays("ok", "fine"))
	if res, err := run(s, agent.Responses, agent.Events{}); err != nil || res.Said != "fine" || s.calls() != 4 {
		t.Fatalf("server_error: result %+v err %v after %d calls", res, err, s.calls())
	}
	bad := []string{responsesEvent("error", `"code":"invalid_prompt","message":"no way"`)}
	s = newScript(t, bad)
	if _, err := run(s, agent.Responses, agent.Events{}); err == nil || !strings.Contains(err.Error(), "no way") || s.calls() != 1 {
		t.Fatalf("error event: err %v after %d calls", err, s.calls())
	}
	s = newScript(t, responsesMessage(0, "partial"))
	if _, err := run(s, agent.Responses, agent.Events{}); err == nil || !strings.Contains(err.Error(), "cut off") || s.calls() != 4 {
		t.Fatalf("cut off: err %v after %d calls", err, s.calls())
	}
}

// A talk_write over 4096 bytes, or an empty one, is refused with a tool error
// saying why; the next call carries everything so far, and a shorter talk_write
// is the result. Every shape.
func TestARefusedSayIsRewritten(t *testing.T) {
	long := strings.Repeat("a", 4097)
	for p, says := range says {
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
			if want == `"is_error":true` {
				want = map[agent.Shape]string{agent.Messages: want, agent.ChatCompletions: `"role":"tool"`, agent.Responses: `"type":"function_call_output"`}[p]
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

// A Tool other than talk_write is offered in the shape's own form after talk_write, called
// with its input, and its result fed back to the model as a result, not an
// error, and recorded as a block; the run goes on until a talk_write. Every shape.
func TestAToolResultIsFedBackAndTheRunGoesOn(t *testing.T) {
	for p, says := range says {
		var got []string
		lookup := agent.Tool{Name: "lookup", Description: "Look it up.",
			Schema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":false}`),
			Call: func(_ context.Context, in json.RawMessage) (string, error) {
				got = append(got, string(in))
				return "found 42", nil
			}}
		s := newScript(t, callsTool[p]("lookup", `{"q":"x"}`), says("so", "it is 42"))
		var rec recorder
		res, err := runWith(s, p, rec.events(), agent.TalkWrite, lookup)
		if err != nil || res.Said != "it is 42" || s.calls() != 2 {
			t.Fatalf("%s: result %+v err %v after %d calls", p, res, err, s.calls())
		}
		if fmt.Sprint(got) != `[{"q":"x"}]` {
			t.Errorf("%s: lookup called with %v", p, got)
		}
		if names := toolNames(s.body(1)); names != "talk_write lookup" {
			t.Errorf("%s: tools offered %q", p, names)
		}
		second, _ := json.Marshal(messages(s.body(2)))
		if !strings.Contains(string(second), "found 42") || strings.Contains(string(second), `"is_error":true`) {
			t.Errorf("%s: second call does not carry the result: %s", p, second)
		}
		if r := rec.resultBlocks(); len(r) != 1 || r[0].Content != "found 42" || r[0].IsError || r[0].ToolUseID == "" {
			t.Errorf("%s: results recorded %+v", p, r)
		}
	}
}

// A call of a Tool the run does not offer, or one whose Call fails, is
// answered with a tool error saying why, and the run goes on. Every shape.
func TestAnUnofferedOrFailingToolIsAToolError(t *testing.T) {
	for p, says := range says {
		broken := agent.Tool{Name: "broken", Schema: json.RawMessage(`{"type":"object"}`),
			Call: func(context.Context, json.RawMessage) (string, error) {
				return "", errors.New("the log is unreachable")
			}}
		s := newScript(t, callsTool[p]("sign_list", `{}`), callsTool[p]("broken", `{}`), says("ok", "done"))
		var rec recorder
		res, err := runWith(s, p, rec.events(), agent.TalkWrite, broken)
		if err != nil || res.Said != "done" || s.calls() != 3 {
			t.Fatalf("%s: result %+v err %v after %d calls", p, res, err, s.calls())
		}
		second, _ := json.Marshal(messages(s.body(2)))
		if !strings.Contains(string(second), `no tool named \"sign_list\"`) || !strings.Contains(string(second), "talk_write, broken") {
			t.Errorf("%s: second call lacks the refusal: %s", p, second)
		}
		third, _ := json.Marshal(messages(s.body(3)))
		if !strings.Contains(string(third), "the log is unreachable") {
			t.Errorf("%s: third call lacks the failure: %s", p, third)
		}
		r := rec.resultBlocks()
		if len(r) != 2 || !r[0].IsError || !r[1].IsError {
			t.Errorf("%s: results recorded %+v", p, r)
		}
	}
}

// A run offered no Tools sends no tools field at all, and ends silent when
// the model ends its turn; a talk_write it calls is refused like any Tool not
// offered, so nothing is said. Every shape.
func TestARunWithNoToolsOffersNone(t *testing.T) {
	for p, says := range says {
		s := newScript(t, says("hmm", "let me speak"), texts[p]("nothing then"))
		res, err := runWith(s, p, agent.Events{})
		if err != nil || !res.Silent || res.Said != "" || s.calls() != 2 {
			t.Fatalf("%s: result %+v err %v after %d calls", p, res, err, s.calls())
		}
		if tools, ok := s.body(1)["tools"]; ok {
			t.Errorf("%s: tools = %v, want none sent", p, tools)
		}
		second, _ := json.Marshal(messages(s.body(2)))
		if !strings.Contains(string(second), `no tool named \"talk_write\"`) {
			t.Errorf("%s: second call lacks the refusal: %s", p, second)
		}
	}
}

// An Anthropic run sends the system prompt as one block with a cache
// breakpoint after it, a breakpoint where the replay ends, on the last user
// message the run before sent, and one on this run's turn, which both stay;
// and one on the latest tool result, which takes it from the one before it
// (ADR 0036).
func TestAnthropicMarksCacheBreakpoints(t *testing.T) {
	s := newScript(t, anthropicSays("first", ""), anthropicSays("second", ""), anthropicSays("third", "done"))
	if _, err := run(s, agent.Messages, agent.Events{}); err != nil {
		t.Fatal(err)
	}
	const wantSystem = "[map[cache_control:map[type:ephemeral] text:be brief\n type:text]]"
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
	for n, want := range map[int]string{1: "[0 2]", 2: "[0 2 4]", 3: "[0 2 6]"} {
		if got := fmt.Sprint(marked(n)); got != want {
			t.Errorf("call %d marks messages %s, want %s", n, got, want)
		}
	}
	last, _ := json.Marshal(messages(s.body(3))[6])
	if !strings.Contains(string(last), `"type":"tool_result"`) {
		t.Fatalf("latest turn = %s", last)
	}
}

// unmarked is v, part of a request body, as JSON without Anthropic's cache
// breakpoints, which move.
func unmarked(v any) string {
	var strip func(v any)
	strip = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			delete(v, "cache_control")
			for _, e := range v {
				strip(e)
			}
		case []any:
			for _, e := range v {
				strip(e)
			}
		}
	}
	strip(v)
	raw, _ := json.Marshal(v)
	return string(raw)
}

// stored is rec's blocks as a Transcript gives them back: each written as
// part of a JSON line, and read from it.
func stored(t *testing.T, rec *recorder) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	for _, r := range rec.records {
		if r.Kind != agent.RecordBlock {
			continue
		}
		line, err := json.Marshal(struct{ Block json.RawMessage }{r.Data})
		if err != nil {
			t.Fatal(err)
		}
		var l struct{ Block json.RawMessage }
		if err := json.Unmarshal(line, &l); err != nil {
			t.Fatal(err)
		}
		out = append(out, l.Block)
	}
	return out
}

// A run replays the agent's earlier runs as they were sent and generated
// (ADR 0036): its request begins with the last request of the run before
// it, whatever its tool calls carried, then holds that run's last output,
// the answer to its accepted talk_write, and this run's turn. A run that
// ended silent, and one that generated nothing, replay too. Every shape.
func TestARunReplaysItsEarlierRunsAsTheyWereSent(t *testing.T) {
	echo := agent.Tool{Name: "echo", Description: "Echo.", Schema: json.RawMessage(`{"type":"object"}`),
		Call: func(context.Context, json.RawMessage) (string, error) { return "pong <&>", nil }}
	for p, says := range says {
		s := newScript(t, callsTool[p]("echo", `{"q": "a<b & c"}`), says("hmm", "first"), texts[p]("nothing to add"), says("", "third"))
		var earlier []agent.Earlier
		summon := func(turn string) []any {
			t.Helper()
			var rec recorder
			res, err := agent.Client{}.Run(context.Background(), agent.Request{
				Shape: p, URL: s.URL, Body: map[string]any{"model": "m"}, System: "be brief", Earlier: earlier, Turn: turn,
				Tools: []agent.Tool{agent.TalkWrite, echo},
			}, rec.events())
			if err != nil {
				t.Fatalf("%s: %q: %v", p, turn, err)
			}
			earlier = append(earlier, agent.Earlier{Turn: turn, Blocks: stored(t, &rec), Posted: !res.Silent})
			return messages(s.body(s.calls()))
		}
		sent := summon("one") // calls echo, then says
		earlier = append(earlier, agent.Earlier{Turn: "a run that failed at its first call"})
		for _, turn := range []string{"two", "three"} { // silent, then says
			before, n := unmarked(sent), len(sent)
			sent = summon(turn)
			if len(sent) < n+2 || unmarked(sent[:n]) != before {
				t.Fatalf("%s: %q does not begin with the run before it:\n%s\n%s", p, turn, before, unmarked(sent))
			}
			if last := unmarked(sent[len(sent)-1]); !strings.Contains(last, turn) {
				t.Errorf("%s: %q ends with %s", p, turn, last)
			}
		}
		all := unmarked(sent)
		for _, want := range []string{"Posted. Your run ended there.", "nothing to add", "pong", "a run that failed at its first call"} {
			if strings.Count(all, want) != 1 {
				t.Errorf("%s: the third run's request holds %q %d times, want once:\n%s", p, want, strings.Count(all, want), all)
			}
		}
	}
}

// A tool call an earlier run ended without answering is answered in the
// replay, so every call has its answer: the talk_write of a run that
// posted nothing, and a call cut off before its Tool returned.
func TestAReplayAnswersCallsItsRunLeftUnanswered(t *testing.T) {
	cut := []agent.Earlier{{Turn: "one", Blocks: []json.RawMessage{
		json.RawMessage(`{"type":"tool_use","id":"t1","name":"echo","input":{}}`),
		json.RawMessage(`{"type":"tool_use","id":"t2","name":"talk_write","input":{"text":"hi"}}`),
		json.RawMessage(`{"type":"tool_result","tool_use_id":"t1","content":"pong","is_error":false}`),
	}}}
	for p, reply := range texts {
		s := newScript(t, reply("ok"))
		if _, err := (agent.Client{}).Run(context.Background(), agent.Request{Shape: p, URL: s.URL, Earlier: cut, Turn: "two"}, agent.Events{}); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		sent := unmarked(messages(s.body(1)))
		pong, unanswered := strings.Index(sent, "pong"), strings.Index(sent, "No answer was recorded: your run ended first.")
		if pong < 0 || unanswered < pong || strings.Index(sent, "two") < unanswered || strings.Contains(sent, "Posted.") {
			t.Errorf("%s: sent %s", p, sent)
		}
	}
	// A call the model never finished, cut off at its token bound, and
	// thinking cut off unsigned, are left out: a provider refuses them.
	bound := []agent.Earlier{{Turn: "one", Blocks: []json.RawMessage{
		json.RawMessage(`{"type":"thinking","thinking":"half a thought"}`),
		json.RawMessage(`{"type":"text","text":"so far"}`),
		json.RawMessage(`{"type":"tool_use","id":"t9","name":"talk_write","input":"{\"text\": \"cut he"}`),
	}}}
	for p, reply := range texts {
		s := newScript(t, reply("ok"))
		if _, err := (agent.Client{}).Run(context.Background(), agent.Request{Shape: p, URL: s.URL, Earlier: bound, Turn: "two"}, agent.Events{}); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if sent := unmarked(messages(s.body(1))); !strings.Contains(sent, "so far") || strings.Contains(sent, "t9") || strings.Contains(sent, "cut he") || strings.Contains(sent, "half a thought") {
			t.Errorf("%s: sent %s", p, sent)
		}
	}
	if _, err := (agent.Client{}).Run(context.Background(), agent.Request{Shape: agent.Messages, URL: "http://unused.invalid",
		Earlier: []agent.Earlier{{Turn: "one", Blocks: []json.RawMessage{json.RawMessage(`"not a block"`)}}}, Turn: "two"}, agent.Events{}); err == nil || !strings.Contains(err.Error(), "earlier run 1") {
		t.Fatalf("a block that does not read: err %v", err)
	}
}

// Each call of a run sends the call before it as an unchanged prefix: the
// same system prompt and tools, and its messages first. Every shape.
func TestASecondCallExtendsTheFirst(t *testing.T) {
	for p, says := range says {
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
			if k != "messages" && k != "input" && fmt.Sprint(second[k]) != fmt.Sprint(v) {
				t.Errorf("%s: %s changed: %v, then %v", p, k, v, second[k])
			}
		}
	}
}

// Ending the turn without talk_write is silence. Every shape.
func TestEndingWithoutSayIsSilent(t *testing.T) {
	for p, reply := range map[agent.Shape][]string{agent.Messages: anthropicText("nothing to add"), agent.ChatCompletions: openaiText("nothing to add"), agent.Responses: responsesText("nothing to add")} {
		s := newScript(t, reply)
		res, err := run(s, p, agent.Events{})
		if err != nil || !res.Silent || res.Said != "" || s.calls() != 1 {
			t.Fatalf("%s: result %+v, err %v, after %d calls", p, res, err, s.calls())
		}
	}
}

// A call that stops at its token bound is an error, read from the stop
// reason the API returns: Messages' max_tokens, Chat Completions' length.
func TestMaxTokensIsAnError(t *testing.T) {
	anth := append([]string{anthropicStart()}, anthropicTextBlock(0, "on and on")...)
	anth = append(anth, anthropicEnd("max_tokens")...)
	oai := []string{openaiChunk(`{"content":"on and on"}`, ""), openaiChunk(`{}`, "length"), `data: [DONE]`}
	for p, reply := range map[agent.Shape][]string{agent.Messages: anth, agent.ChatCompletions: oai} {
		s := newScript(t, reply)
		stop := map[agent.Shape]string{agent.Messages: "max_tokens", agent.ChatCompletions: "length"}[p]
		if _, err := run(s, p, agent.Events{}); err == nil || !strings.Contains(err.Error(), "token bound") || !strings.Contains(err.Error(), stop) || s.calls() != 1 {
			t.Fatalf("%s: err %v after %d calls", p, err, s.calls())
		}
	}
}

// A run that reaches 8 calls without an accepted talk_write is an error.
func TestEightCallsWithoutSayIsAnError(t *testing.T) {
	s := newScript(t, anthropicSays("again", ""))
	_, err := run(s, agent.Messages, agent.Events{})
	if err == nil || !strings.Contains(err.Error(), "8 model calls") || s.calls() != 8 {
		t.Fatalf("err %v after %d calls", err, s.calls())
	}
}

// The profile's body merges over what DeFiOS writes: a key it does not
// write is added as is, nested objects stay nested, and anything else
// replaces what is there, even DeFiOS's own (ADR 0019). Both shapes.
func TestTheProfileBodyMergesOverTheRequest(t *testing.T) {
	for p, reply := range map[agent.Shape][]string{agent.Messages: anthropicText("ok"), agent.ChatCompletions: openaiText("ok"), agent.Responses: responsesText("ok")} {
		s := newScript(t, reply)
		_, err := agent.Client{}.Run(context.Background(), agent.Request{
			Shape: p, URL: s.URL, Earlier: earlier, Turn: turn, Body: map[string]any{
				"model": "m", "stream": true, "max_tokens": int64(1024), "temperature": 0.2,
				"thinking": map[string]any{"type": "adaptive", "budget": map[string]any{"tokens": int64(7)}},
				"tools":    []any{"theirs"},
			},
		}, agent.Events{})
		if err != nil {
			t.Fatal(err)
		}
		body := s.body(1)
		if body["model"] != "m" || body["stream"] != true || body["max_tokens"] != float64(1024) || body["temperature"] != 0.2 {
			t.Errorf("%s: body = %v", p, body)
		}
		if got := fmt.Sprint(body["thinking"]); got != "map[budget:map[tokens:7] type:adaptive]" {
			t.Errorf("%s: thinking = %s", p, got)
		}
		if got := fmt.Sprint(body["tools"]); got != "[theirs]" {
			t.Errorf("%s: tools = %s, want the body's in place of talk_write", p, got)
		}
		if len(messages(body)) == 0 {
			t.Errorf("%s: the conversation is gone: %v", p, body)
		}
	}
}

// A reply that is neither an event stream nor JSON is an error, not
// retried.
func TestAReplyOfAnotherTypeIsAnError(t *testing.T) {
	srv, calls := failing(t, func(_ int, w http.ResponseWriter) bool {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html>hello</html>`))
		return true
	})
	if _, err := generate(srv, agent.Events{}); err == nil || *calls != 1 || !strings.Contains(err.Error(), "text/html") {
		t.Fatalf("err %v after %d calls, want one error naming text/html", err, *calls)
	}
}

// A reply that is not streamed is read whole, picked by its Content-Type
// whatever the body's stream says: its blocks stream as the streamed reader
// streams them, in one go once the call ends, and the run records the same
// blocks and usage. The next call carries them like a streamed call's.
// Every shape; Chat Completions has no thinking.
func TestANonStreamedReplyIsRead(t *testing.T) {
	for p, says := range jsonSays {
		s := newScript(t, says("hmm", "let me see", ""), says("ok", "now", "all good"))
		var rec recorder
		res, err := run(s, p, rec.events())
		if err != nil || res.Silent || res.Said != "all good" || s.calls() != 2 {
			t.Fatalf("%s: result %+v, err %v after %d calls", p, res, err, s.calls())
		}
		want := `thinking:hmm text:let me see tool:talk_write {"text":""} thinking:ok text:now tool:talk_write {"text":"all good"}`
		blocks := "block block block"
		if p == agent.ChatCompletions {
			want = `text:let me see tool:talk_write {"text":""} text:now tool:talk_write {"text":"all good"}`
			blocks = "block block"
		}
		if rec.String() != want {
			t.Errorf("%s: events %s\nwant   %s", p, &rec, want)
		}
		if got, want := rec.kinds(), "request "+blocks+" usage block request "+blocks+" usage"; got != want {
			t.Errorf("%s: records = %s, want %s", p, got, want)
		}
		second := fmt.Sprint(messages(s.body(2)))
		if !strings.Contains(second, "let me see") || !strings.Contains(second, "empty") {
			t.Errorf("%s: the second call does not carry the first: %s", p, second)
		}
		if p == agent.Responses && !strings.Contains(second, "gAAAA-enc") {
			t.Errorf("%s: the reasoning item is not replayed: %s", p, second)
		}
		if p == agent.Messages && !strings.Contains(second, "signature:sig") {
			t.Errorf("%s: the thinking block is not replayed: %s", p, second)
		}
	}
}

// A JSON reply cut off before its end is retried like a stream that was
// cut off. Every shape.
func TestATruncatedJSONReplyIsRetried(t *testing.T) {
	for p, says := range jsonSays {
		whole := strings.TrimPrefix(says("hmm", "partial", "x")[0], jsonMark)
		cut := asJSON(whole[:len(whole)/2])
		s := newScript(t, cut, cut, says("hmm", "ok", "fine"))
		res, err := run(s, p, agent.Events{})
		if err != nil || res.Said != "fine" || s.calls() != 3 {
			t.Fatalf("%s: result %+v, err %v after %d calls", p, res, err, s.calls())
		}
		s = newScript(t, cut)
		if _, err := run(s, p, agent.Events{}); err == nil || !strings.Contains(err.Error(), "cut off") || s.calls() != 4 {
			t.Fatalf("%s: err %v after %d calls, want cut off after 4", p, err, s.calls())
		}
	}
}

// A Request with no known shape fails before any call.
func TestARunWithNoShapeMakesNoCall(t *testing.T) {
	s := newScript(t, anthropicSays("hmm", "hi"))
	if _, err := run(s, agent.Shape{}, agent.Events{}); err == nil || !strings.Contains(err.Error(), "no API shape") || s.calls() != 0 {
		t.Fatalf("err %v after %d calls", err, s.calls())
	}
}

// A JSON reply over the size the agent reads is an error of its own, not
// one cut off: it is not retried.
func TestAnOversizedJSONReplyIsNotRetried(t *testing.T) {
	huge := `{"type":"message","content":[{"type":"text","text":"` + strings.Repeat("a", 17<<20) + `"}],"stop_reason":"end_turn"}`
	s := newScript(t, asJSON(huge))
	_, err := run(s, agent.Messages, agent.Events{})
	if err == nil || strings.Contains(err.Error(), "cut off") || !strings.Contains(err.Error(), "too large") || s.calls() != 1 {
		t.Fatalf("err %v after %d calls, want too large after 1", err, s.calls())
	}
}

// A JSON reply that stopped at its token bound is an error, read from the
// stop reason it carries, with its blocks and usage recorded. Every shape.
func TestAJSONReplyAtItsTokenBoundIsAnError(t *testing.T) {
	for p, reply := range map[agent.Shape]string{
		agent.Messages:        `{"type":"message","content":[{"type":"text","text":"on and on"}],"stop_reason":"max_tokens","usage":{"input_tokens":10,"output_tokens":5}}`,
		agent.ChatCompletions: `{"choices":[{"index":0,"message":{"role":"assistant","content":"on and on"},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		agent.Responses: `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[` +
			`{"id":"msg_0","type":"message","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"on and on"}]}],` + responsesUsage + `}`,
	} {
		s := newScript(t, asJSON(reply))
		var rec recorder
		_, err := run(s, p, rec.events())
		if err == nil || !strings.Contains(err.Error(), "token bound") || s.calls() != 1 {
			t.Fatalf("%s: err %v after %d calls", p, err, s.calls())
		}
		if rec.String() != "text:on and on" || rec.kinds() != "request block usage" {
			t.Errorf("%s: events %s, records %s", p, &rec, rec.kinds())
		}
	}
}

// A JSON reply that says it failed is an error: a Responses server error
// is retried, an Anthropic invalid request is not.
func TestAFailedJSONReplyIsAnError(t *testing.T) {
	failed := asJSON(`{"id":"resp_1","status":"failed","output":[],"error":{"code":"server_error","message":"boom"}}`)
	s := newScript(t, failed, jsonSays[agent.Responses]("a", "b", "fine"))
	if res, err := run(s, agent.Responses, agent.Events{}); err != nil || res.Said != "fine" || s.calls() != 2 {
		t.Fatalf("responses: result %+v err %v after %d calls", res, err, s.calls())
	}
	s = newScript(t, asJSON(`{"type":"error","error":{"type":"invalid_request_error","message":"no way"}}`))
	if _, err := run(s, agent.Messages, agent.Events{}); err == nil || !strings.Contains(err.Error(), "no way") || s.calls() != 1 {
		t.Fatalf("messages: err %v after %d calls", err, s.calls())
	}
}

// The suffix of a Provider's api_path picks its shape; any other path has
// none.
func TestShapeOfAPIPath(t *testing.T) {
	for path, want := range map[string]string{
		"/v1/messages":             "messages",
		"/anthropic/v1/messages":   "messages",
		"/v1/chat/completions":     "chat_completions",
		"/chat/completions":        "chat_completions",
		"/openai/chat/completions": "chat_completions",
		"/v1/responses":            "responses",
		"/openai/v1/responses":     "responses",
	} {
		if sh, ok := agent.ShapeOf(path); !ok || sh.Name != want {
			t.Errorf("%s: shape %+v, %v; want %s", path, sh, ok, want)
		}
	}
	for _, path := range []string{"/v1/foo", "/v1/messages/", "/v1/completions", "/v1/decisions", "", "/v1/messagesx"} {
		if sh, ok := agent.ShapeOf(path); ok {
			t.Errorf("%q: shape %+v, want none", path, sh)
		}
	}
}

// MaxTurns bounds a run's calls in place of the default 8.
func TestMaxTurnsBoundsTheRun(t *testing.T) {
	s := newScript(t, anthropicSays("again", ""))
	_, err := agent.Client{}.Run(context.Background(), agent.Request{
		Shape: agent.Messages, URL: s.URL, Earlier: earlier, Turn: turn, MaxTurns: 3, Tools: []agent.Tool{agent.TalkWrite},
	}, agent.Events{})
	if err == nil || !strings.Contains(err.Error(), "3 model calls") || s.calls() != 3 {
		t.Fatalf("err %v after %d calls", err, s.calls())
	}
}

// A body the API rejects, such as a max_tokens over the model's cap, is a
// 400 from the provider: an error, not retried.
func TestAProviderRefusingAnOptionIsNotRetried(t *testing.T) {
	srv, calls := failing(t, func(_ int, w http.ResponseWriter) bool {
		w.WriteHeader(400)
		w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: 99999 > 64000"}}`))
		return true
	})
	if _, err := generate(srv, agent.Events{}); err == nil || *calls != 1 || !strings.Contains(err.Error(), "max_tokens: 99999") {
		t.Fatalf("err %v after %d calls, want the 400 once", err, *calls)
	}
}

// A run records each request body as sent, each output block, each call's
// usage and then the answer to each of its tool calls, and never the key.
func TestRunRecordsRequestsBlocksAndUsage(t *testing.T) {
	s := newScript(t, anthropicSays("first", ""), anthropicSays("second", "done"))
	var rec recorder
	if _, err := run(s, agent.Messages, rec.events()); err != nil {
		t.Fatal(err)
	}
	if got := rec.kinds(); got != "request block block usage block request block block usage" {
		t.Fatalf("records = %s", got)
	}
	var sent map[string]any
	if err := json.Unmarshal(rec.records[5].Data, &sent); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sent) != fmt.Sprint(s.body(2)) {
		t.Fatalf("recorded request %v, sent %v", sent, s.body(2))
	}
	if !strings.Contains(string(rec.records[2].Data), `"talk_write"`) || !strings.Contains(string(rec.records[3].Data), "output_tokens") ||
		!strings.Contains(string(rec.records[4].Data), `"is_error":true`) {
		t.Fatalf("records = %s, %s, %s", rec.records[2].Data, rec.records[3].Data, rec.records[4].Data)
	}
	for _, r := range rec.records {
		if strings.Contains(string(r.Data), "sk-secret") {
			t.Fatalf("a record holds the key: %s", r.Data)
		}
	}
}

// With no key, no credential header is sent at all.
func TestNoKeySendsNoCredential(t *testing.T) {
	for p, reply := range map[agent.Shape][]string{agent.Messages: anthropicText("ok"), agent.ChatCompletions: openaiText("ok"), agent.Responses: responsesText("ok")} {
		s := newScript(t, reply)
		if _, err := (agent.Client{}).Run(context.Background(), agent.Request{Shape: p, URL: s.URL, Turn: "p"}, agent.Events{}); err != nil {
			t.Fatal(err)
		}
		if h := s.headers[0]; h.Get("x-api-key")+h.Get("Authorization") != "" {
			t.Errorf("%s: credential headers %v", p, h)
		}
	}
}

// failing serves fail(n) for the nth call while it returns true, then a talk_write
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
	return c.Run(context.Background(), agent.Request{Shape: agent.Messages, URL: srv.URL, Turn: "p", Tools: []agent.Tool{agent.TalkWrite}}, ev)
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
		if err != nil || res.Said != "fine" || *calls != 4 || rec.String() != `text:ok tool:talk_write {"text":"fine"}` {
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
	if rec.String() != `text:partial reset text:partial reset text:ok tool:talk_write {"text":"fine"}` {
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
	_, err := agent.Client{Backoff: time.Millisecond}.Run(ctx, agent.Request{Shape: agent.Messages, URL: srv.URL, Turn: "p"}, agent.Events{})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err %v after %d calls, want context.Canceled once", err, calls)
	}
}
