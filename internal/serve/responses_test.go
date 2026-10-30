package serve_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"defios.com/internal/group"
	"defios.com/internal/serve"
	coslv1 "defios.com/proto/cosl/v1"
)

// responsesEvent writes one Responses stream event of typ with fields.
func responsesEvent(w http.ResponseWriter, typ, fields string) {
	event(w, typ, `{"type":"`+typ+`",`+fields+`}`)
}

const responsesUsage = `"usage":{"input_tokens":11,"output_tokens":4,"output_tokens_details":{"reasoning_tokens":3}}`

// responsesReply streams a reasoning item, a message of text and a say of
// said, then completes, or with incomplete set ends incomplete for it.
func responsesReply(w http.ResponseWriter, text, said, incomplete string) {
	w.Header().Set("Content-Type", "text/event-stream")
	responsesEvent(w, "response.output_item.added", `"output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}`)
	responsesEvent(w, "response.reasoning_summary_text.delta", `"output_index":0,"delta":"pondering"`)
	responsesEvent(w, "response.output_item.done", `"output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"pondering"}],"encrypted_content":"enc-1"}`)
	raw, _ := json.Marshal(text)
	responsesEvent(w, "response.output_item.added", `"output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}`)
	responsesEvent(w, "response.output_text.delta", fmt.Sprintf(`"output_index":1,"delta":%s`, raw))
	responsesEvent(w, "response.output_item.done", fmt.Sprintf(`"output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":%s}]}`, raw))
	if incomplete != "" {
		responsesEvent(w, "response.incomplete", `"response":{"status":"incomplete","incomplete_details":{"reason":"`+incomplete+`"},`+responsesUsage+`}`)
		return
	}
	in, _ := json.Marshal(map[string]string{"text": said})
	args, _ := json.Marshal(string(in))
	responsesEvent(w, "response.output_item.added", `"output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"say","arguments":""}`)
	responsesEvent(w, "response.function_call_arguments.delta", fmt.Sprintf(`"output_index":2,"delta":%s`, args))
	responsesEvent(w, "response.output_item.done", fmt.Sprintf(`"output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"say","arguments":%s}`, args))
	responsesEvent(w, "response.completed", `"response":{"status":"completed",`+responsesUsage+`}`)
}

// openaiAgent puts the Provider "openai" on the log with a fake Responses
// API answering the nth call with reply(n), and configures a's Handle
// auditor to run on it with a streamed body that keeps reasoning off
// OpenAI's servers. It returns how many calls the fake has had.
func openaiAgent(t *testing.T, a *node, reply func(n int, w http.ResponseWriter)) func() int {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer sk-openai" {
			http.Error(w, "wrong path or key", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		reply(n, w)
	}))
	t.Cleanup(srv.Close)
	v, err := a.client().ProviderAdd(context.Background(), &coslv1.ProviderConfig{Alias: "openai", BaseUrl: srv.URL, ApiPath: "/v1/responses"})
	if err != nil {
		t.Fatal(err)
	}
	a.applied(t, v.Seq)
	dir := filepath.Join(t.TempDir(), "auditor")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SYSTEM.md"), []byte("You are auditor."), 0o600); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, a.paths.GroupConfigFile(group.ImplicitAlias), fmt.Sprintf(`[agents.auditor]
dir = %q
provider = "openai"

[agents.auditor.body]
model = "gpt-test"
stream = true
store = false
include = ["reasoning.encrypted_content"]

[agent]
profiles = ["auditor"]

[providers.openai]
api_key = "$OPENAI_KEY"
`, dir))
	a.secrets["OPENAI_KEY"] = "sk-openai"
	return func() int { mu.Lock(); defer mu.Unlock(); return calls }
}

// An agent on the openai preset's shape streams its reasoning, text and say
// in conversation follow; a refused say is answered with a
// function_call_output and the model tries again in the same run, its
// second call carrying the first call's reasoning item as it came back;
// the say commits, and each call's usage is in the Transcript.
func TestAnAgentSpeaksTheResponsesAPI(t *testing.T) {
	a, b, gid, _ := agentPair(t)
	calls := openaiAgent(t, a, func(n int, w http.ResponseWriter) {
		if n == 1 {
			responsesReply(w, "first", "", "")
			return
		}
		responsesReply(w, "second", "hello", "")
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `chain.start start:@auditor/1 thinking:pondering text:first tool:say {"text":""} thinking:pondering text:second tool:say {"text":"hello"} commit:hello end`
	if got := summary(untilEnd(t, s, v.Seq)); got != want {
		t.Fatalf("events = %s\nwant     %s", got, want)
	}
	if ffs := freeforms(t, b.groupLevelRows(t, gid, 2)); ffs[1].Text != "hello" || ffs[1].Handle != "auditor" {
		t.Fatalf("committed %+v", ffs[1])
	}
	if calls() != 2 {
		t.Fatalf("%d calls, want 2", calls())
	}
	lines := transcript(t, a, v.Seq)
	var requests []map[string]any
	usages := 0
	for _, l := range lines {
		switch l["kind"] {
		case serve.KindRequest:
			req, _ := l["request"].(map[string]any)
			requests = append(requests, req)
		case serve.KindUsage:
			if u, _ := l["usage"].(map[string]any); u["input_tokens"] == float64(11) && u["output_tokens_details"] != nil {
				usages++
			}
		}
	}
	if len(requests) != 2 || usages != 2 {
		t.Fatalf("transcript = %s", lineWords(lines))
	}
	second := requests[1]
	if second["model"] != "gpt-test" || second["store"] != false || fmt.Sprint(second["include"]) != "[reasoning.encrypted_content]" {
		t.Fatalf("second request = %v", second)
	}
	input, _ := json.Marshal(second["input"])
	for _, want := range []string{
		`{"encrypted_content":"enc-1","id":"rs_1","summary":[{"text":"pondering","type":"summary_text"}],"type":"reasoning"}`,
		`"type":"function_call_output"`,
	} {
		if !strings.Contains(string(input), want) {
			t.Fatalf("second request's input lacks %s: %s", want, input)
		}
	}
}

// A reply that comes back incomplete for max_output_tokens is an error and
// nothing commits.
func TestAnIncompleteResponseCommitsNothing(t *testing.T) {
	a, _, gid, _ := agentPair(t)
	openaiAgent(t, a, func(_ int, w http.ResponseWriter) { responsesReply(w, "on and on", "", "max_output_tokens") })
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	events := untilEnd(t, s, v.Seq)
	if last := events[len(events)-2]; last.Type != serve.EventAgentError || !strings.Contains(last.Error, "max_output_tokens") {
		t.Fatalf("events = %s, error %q", summary(events), last.Error)
	}
	time.Sleep(200 * time.Millisecond)
	for _, ff := range freeforms(t, a.groupLevelRows(t, gid, 1)) {
		if ff.Handle != "" {
			t.Fatalf("an agent committed %+v", ff)
		}
	}
}
