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

	"defios.com/internal/group"
	"defios.com/internal/serve"
	coslv1 "defios.com/proto/cosl/v1"
)

// bothWays puts the Provider "p" on a's log with a fake API at apiPath, and
// configures two Handles on it: @streamed, whose body sets stream = true,
// and @whole, whose body has no stream. The fake answers the nth call with
// reply(n, streamed), streamed being what the request's body asked for, as
// the real APIs do.
func bothWays(t *testing.T, a *node, apiPath string, reply func(n int, w http.ResponseWriter, streamed bool)) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		reply(n, w, body.Stream)
	}))
	t.Cleanup(srv.Close)
	v, err := a.client().ProviderAdd(context.Background(), &coslv1.ProviderConfig{Alias: "p", BaseUrl: srv.URL, ApiPath: apiPath})
	if err != nil {
		t.Fatal(err)
	}
	a.applied(t, v.Seq)
	var cfg strings.Builder
	for _, h := range []string{"streamed", "whole"} {
		dir := filepath.Join(t.TempDir(), h)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SYSTEM.md"), []byte("You are "+h+"."), 0o600); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&cfg, "[agents.%s]\ndir = %q\nprovider = \"p\"\n\n[agents.%s.body]\nmodel = \"m\"\n", h, dir, h)
		if h == "streamed" {
			cfg.WriteString("stream = true\n")
		}
		cfg.WriteString("\n")
	}
	cfg.WriteString("[agent]\nprofiles = [\"streamed\", \"whole\"]\n")
	writeConfig(t, a.paths.GroupConfigFile(group.ImplicitAlias), cfg.String())
}

// writeJSON writes a reply that is not streamed.
func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, body)
}

func quoted(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// sayArgs is say's input for text as JSON.
func sayArgs(text string) string {
	raw, _ := json.Marshal(map[string]string{"text": text})
	return string(raw)
}

// messagesReply thinks "pondering", writes text and says said, streamed or
// as one JSON reply.
func messagesReply(w http.ResponseWriter, streamed bool, text, said string) {
	if !streamed {
		writeJSON(w, `{"id":"m","type":"message","role":"assistant","content":[`+
			`{"type":"thinking","thinking":"pondering","signature":"sig-1"},`+
			`{"type":"text","text":`+quoted(text)+`},`+
			`{"type":"tool_use","id":"toolu_1","name":"say","input":`+sayArgs(said)+`}],`+
			`"stop_reason":"tool_use","usage":{"input_tokens":7,"output_tokens":3}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	event(w, "message_start", `{"type":"message_start","message":{"usage":{"input_tokens":7,"output_tokens":1}}}`)
	event(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
	event(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}`)
	event(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}`)
	event(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
	event(w, "content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
	event(w, "content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":`+quoted(text)+`}}`)
	event(w, "content_block_stop", `{"type":"content_block_stop","index":1}`)
	event(w, "content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"say","input":{}}}`)
	event(w, "content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":`+quoted(sayArgs(said))+`}}`)
	event(w, "content_block_stop", `{"type":"content_block_stop","index":2}`)
	endTurn(w, "tool_use")
}

// chatReply writes text and says said, streamed or as one JSON reply.
func chatReply(w http.ResponseWriter, streamed bool, text, said string) {
	const usage = `"usage":{"prompt_tokens":7,"completion_tokens":3}`
	if !streamed {
		writeJSON(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":`+quoted(text)+`,`+
			`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"say","arguments":`+quoted(sayArgs(said))+`}}]},`+
			`"finish_reason":"tool_calls"}],`+usage+`}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, data := range []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":` + quoted(text) + `},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"say","arguments":` + quoted(sayArgs(said)) + `}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],` + usage + `}`,
		`[DONE]`,
	} {
		fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
	}
}

// responsesWhole is responsesReply as one JSON reply: the same items.
func responsesWhole(w http.ResponseWriter, text, said string) {
	args, _ := json.Marshal(sayArgs(said))
	writeJSON(w, `{"id":"resp_1","object":"response","status":"completed","output":[`+
		`{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"pondering"}],"encrypted_content":"enc-1"},`+
		`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":`+quoted(text)+`}]},`+
		`{"id":"fc_1","type":"function_call","call_id":"call_1","name":"say","arguments":`+string(args)+`}],`+
		responsesUsage+`}`)
}

// runLines is a run's Transcript lines as compared across replies: the
// request bodies, which differ by stream, and the seq left out.
func runLines(t *testing.T, a *node, seq uint64) string {
	t.Helper()
	var out []string
	for _, l := range transcript(t, a, seq) {
		delete(l, "handle")
		delete(l, "seq")
		if l["kind"] == serve.KindRequest {
			delete(l, "request")
		}
		raw, _ := json.Marshal(l)
		out = append(out, string(raw))
	}
	return strings.Join(out, "\n")
}

// An agent whose body has no stream gets one JSON reply and reads it on
// each of the three shapes: conversation follow shows the whole reply, its
// thinking, text and say, once the call ends; the say commits; and the
// Transcript records the same blocks, usage and outcome as the streamed
// reply of the same content. A refused say is answered and tried again
// in the same run either way.
func TestAnAgentReadsAReplyThatIsNotStreamed(t *testing.T) {
	for _, tc := range []struct {
		apiPath string
		reply   func(w http.ResponseWriter, streamed bool, text, said string)
		think   string // what follow shows as thinking before the text
	}{
		{"/v1/messages", messagesReply, "thinking:pondering "},
		{"/v1/chat/completions", chatReply, ""},
		{"/v1/responses", func(w http.ResponseWriter, streamed bool, text, said string) {
			if streamed {
				responsesReply(w, text, said, "")
				return
			}
			responsesWhole(w, text, said)
		}, "thinking:pondering "},
	} {
		t.Run(strings.TrimPrefix(tc.apiPath, "/v1/"), func(t *testing.T) {
			a, b, gid, _ := agentPair(t)
			bothWays(t, a, tc.apiPath, func(n int, w http.ResponseWriter, streamed bool) {
				if n%2 == 1 {
					tc.reply(w, streamed, "first", "")
					return
				}
				tc.reply(w, streamed, "second", "hello")
			})
			s := watch(t, a)
			runs := map[string]string{}
			lines := map[string]string{}
			for _, h := range []string{"streamed", "whole"} {
				v, err := a.client().ConversationSend(context.Background(), "@"+h, nil)
				if err != nil {
					t.Fatal(err)
				}
				runs[h] = strings.ReplaceAll(summary(untilEnd(t, s, v.Seq)), "@"+h, "@h")
				lines[h] = runLines(t, a, v.Seq)
			}
			want := `chain.start start:@h/1 ` + tc.think + `text:first tool:say {"text":""} ` +
				tc.think + `text:second tool:say {"text":"hello"} commit:hello end`
			for h, got := range runs {
				if got != want {
					t.Errorf("@%s: events = %q\nwant         %q", h, got, want)
				}
			}
			if lines["whole"] != lines["streamed"] {
				t.Errorf("transcripts differ:\nwhole:\n%s\nstreamed:\n%s", lines["whole"], lines["streamed"])
			}
			for _, want := range []string{`"kind":"usage"`, `"outcome":"said"`, `"kind":"block"`} {
				if !strings.Contains(lines["whole"], want) {
					t.Errorf("transcript lacks %s:\n%s", want, lines["whole"])
				}
			}
			ffs := freeforms(t, b.groupLevelRows(t, gid, 4))
			if ffs[1].Text != "hello" || ffs[1].Handle != "streamed" || ffs[3].Text != "hello" || ffs[3].Handle != "whole" {
				t.Fatalf("committed %+v, %+v", ffs[1], ffs[3])
			}
		})
	}
}

// A JSON reply cut off before its end is retried like a cut-off stream and
// the run goes on; one that stopped at its token bound is an error, the
// Transcript saying why.
func TestAJSONReplyCutOffIsRetriedAndOneAtItsBoundIsAnError(t *testing.T) {
	a, _, _, _ := agentPair(t)
	bothWays(t, a, "/v1/messages", func(n int, w http.ResponseWriter, _ bool) {
		switch n {
		case 1:
			writeJSON(w, `{"id":"m","type":"message","content":[{"type":"te`)
		case 2:
			messagesReply(w, false, "ok", "fine")
		default:
			writeJSON(w, `{"type":"message","content":[{"type":"text","text":"on and on"}],"stop_reason":"max_tokens","usage":{"input_tokens":7,"output_tokens":3}}`)
		}
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@whole", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.Seq)); !strings.HasSuffix(got, `tool:say {"text":"fine"} commit:fine end`) {
		t.Fatalf("events = %s", got)
	}
	if got := lineWords(transcript(t, a, v.Seq)); strings.Count(got, ":request") != 2 || !strings.HasSuffix(got, "outcome=said") {
		t.Fatalf("transcript = %s", got)
	}
	v, err = a.client().ConversationSend(context.Background(), "@whole", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.Seq)); got != "chain.start start:@whole/1 text:on and on error:@whole end" {
		t.Fatalf("events = %s", got)
	}
	lines := transcript(t, a, v.Seq)
	last := lines[len(lines)-1]
	if last["outcome"] != serve.OutcomeError || !strings.Contains(fmt.Sprint(last["error"]), "token bound") {
		t.Fatalf("transcript = %s, last %v", lineWords(lines), last)
	}
}
