// Package agent runs a summoned agent against a Provider (docs/adr/0015-
// agent-profiles-handles-and-agent-chains.md, docs/adr/0018-an-agent-says-
// one-message.md, docs/adr/0019-providers-replace-models-on-the-cosl.md). A
// run is a loop of model calls in one API Shape, streamed or not, offering one
// tool, say: an accepted say is the run's result and ends it; a turn that
// ends without one is silence. Each call is retried when a provider says it
// is worth it.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultMaxTurns is max_turns when a Request sets none: how many model
// calls a run may make before it must have said something or stayed
// silent.
const DefaultMaxTurns = 8

// MaxSay is the most bytes a say may carry; a longer one is refused.
const MaxSay = 4096

// sayTool is the one tool every run offers.
const sayTool = "say"

// sayDescription tells the model what say does.
const sayDescription = "Post your message to the group chat. Only what you pass here is posted; " +
	"everything else you write stays private. Calling it ends your turn. " +
	"Keep it to a paragraph or two of plain text, at most 4096 bytes. " +
	"End your turn without calling it to stay silent."

// saySchema is say's input: one string.
// It admits no other property, as a strict function tool requires.
var saySchema = json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"The message to post."}},"required":["text"],"additionalProperties":false}`)

// Request is one run to make.
type Request struct {
	Shape  Shape  // the API shape, from the Provider's api_path
	URL    string // the whole endpoint: base URL and API path
	APIKey string // empty sends none
	// System is the stable part of the system prompt, the same for an agent
	// at every depth; SystemDepth, what differs by depth, follows it directly
	// as one prompt. Anthropic gets them as two blocks with a cache
	// breakpoint between; OpenAI caches the shared prefix itself (ADR 0018).
	System      string
	SystemDepth string
	Turns       []Turn // the talk so far, alternating and ending with User

	// Body is the Agent Profile's request body, JSON values only: it merges
	// over what DeFiOS writes, the conversation and the say tool (ADR 0019).
	// The model id, streaming and any token bound are set here or not at all.
	Body map[string]any
	// MaxTurns bounds the calls in the run; 0 is DefaultMaxTurns.
	MaxTurns int
}

// maxTurns is how many calls req's run may make.
func (req Request) maxTurns() int {
	if req.MaxTurns > 0 {
		return req.MaxTurns
	}
	return DefaultMaxTurns
}

// Turn is one message of the talk as the Model sees it (ADR 0018): the
// agent's own message is an Assistant turn, everything else User content.
type Turn struct {
	Role string // User or Assistant
	Text string
}

// The roles a Turn takes; both providers name them alike.
const (
	User      = "user"
	Assistant = "assistant"
)

// Result is how a run ended: with Said, what its accepted say carried, or
// Silent when the model ended its turn without one.
type Result struct {
	Said   string
	Silent bool
}

// Record is something a run generated or sent, for the Transcript.
type Record struct {
	Kind string // RecordRequest, RecordBlock or RecordUsage
	Data json.RawMessage
}

// The kinds of Record.
const (
	RecordRequest = "request" // a request body as sent, never its headers
	RecordBlock   = "block"   // one output block of a call
	RecordUsage   = "usage"   // a call's usage
)

// Kinds of output an agent generates, as it streams (ADR 0018).
const (
	KindThinking = "thinking"
	KindText     = "text"
	KindTool     = "tool" // a tool call, say like any other, as its name and input
)

// Events receive a run as it goes. Any may be nil.
type Events struct {
	// Delta is output of kind as it is generated: every block of every
	// model call of the run. Redacted thinking has no text and streams
	// nothing.
	Delta func(kind, text string)
	// Reset means a retry starts the current call over: forget its deltas.
	Reset func()
	// Record receives each request as sent and each call's output and usage.
	Record func(Record)
}

func (ev Events) record(kind string, v any) {
	if ev.Record == nil {
		return
	}
	data, ok := v.(json.RawMessage)
	if !ok {
		var err error
		if data, err = json.Marshal(v); err != nil {
			return
		}
	}
	ev.Record(Record{Kind: kind, Data: data})
}

// stream hands a run's output to Events.Delta as it is generated. A block
// of the kind the last one was, other than text, starts on a new line.
type stream struct {
	delta   func(kind, text string)
	last    string // the kind streamed last
	started bool   // the current attempt has streamed
}

// block starts a block of kind with text.
func (s *stream) block(kind, text string) {
	if kind == s.last && kind != KindText {
		text = "\n" + text
	}
	s.more(kind, text)
}

// more is text of the block of kind streaming now.
func (s *stream) more(kind, text string) {
	if text == "" {
		return
	}
	s.last, s.started = kind, true
	if s.delta != nil {
		s.delta(kind, text)
	}
}

// Retries is how many times a retryable failure is tried again (ADR 0015).
const Retries = 3

// maxRetryAfter bounds how long a provider's Retry-After may hold a chain.
const maxRetryAfter = 2 * time.Minute

// Client runs agents.
type Client struct {
	// HTTP is the client to call providers with; nil is a default one.
	HTTP *http.Client
	// Backoff is the wait before the first retry, doubling after each; zero
	// is a second. A provider's Retry-After takes its place.
	Backoff time.Duration
}

// Block is one output block of a model call, whichever the provider.
type Block struct {
	Type      string          `json:"type"` // text, thinking, redacted_thinking, reasoning or tool_use
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"` // redacted_thinking
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	// Item is a Responses output item as it came back, replayed as is on
	// the run's next call: a reasoning item keeps its encrypted content.
	Item json.RawMessage `json:"item,omitempty"`
}

// call is what one model call returned.
type call struct {
	blocks []Block
	stop   string // the provider's stop or finish reason
	// incomplete is a Responses reply that came back incomplete, for the
	// reason in stop.
	incomplete bool
	usage      map[string]any // as the provider reported it
}

// Run makes req's run: model calls until a say is accepted, the model ends
// its turn without one, or something fails. Each call carries everything
// the run has generated so far. A call is retried on a
// rate limit, overload, 5xx response or dropped connection, up to Retries
// times. A call that stops at its token bound, or a run that reaches its
// turn bound without a say, is an error.
func (c Client) Run(ctx context.Context, req Request, ev Events) (Result, error) {
	conv, err := newConversation(req)
	if err != nil {
		return Result{}, err
	}
	st := &stream{delta: ev.Delta}
	for range req.maxTurns() {
		body, err := conv.body()
		if err != nil {
			return Result{}, err
		}
		out, err := c.call(ctx, req, body, ev, st)
		if err != nil {
			return Result{}, err
		}
		for _, b := range out.blocks {
			ev.record(RecordBlock, b)
		}
		if out.usage != nil {
			ev.record(RecordUsage, out.usage)
		}
		if out.stop == "max_tokens" || out.stop == "length" || out.stop == "max_output_tokens" {
			return Result{}, fmt.Errorf("agent: the model stopped at its token bound (stop reason %s) before it finished", out.stop)
		}
		if out.incomplete {
			return Result{}, fmt.Errorf("agent: the reply came back incomplete (%s)", out.stop)
		}
		var results []toolResult
		for _, b := range out.blocks {
			if b.Type != "tool_use" {
				continue
			}
			text, refusal := checkSay(b)
			if refusal == "" {
				return Result{Said: text}, nil
			}
			results = append(results, toolResult{id: b.ID, text: refusal})
		}
		if len(results) == 0 {
			return Result{Silent: true}, nil
		}
		conv.add(out.blocks, results)
	}
	return Result{}, fmt.Errorf("agent: %d model calls without an accepted say", req.maxTurns())
}

// checkSay is the text of an acceptable say, or why b is refused.
func checkSay(b Block) (text, refusal string) {
	if b.Name != sayTool {
		return "", fmt.Sprintf("There is no tool named %q; say is the only tool.", b.Name)
	}
	var in struct {
		Text *string `json:"text"`
	}
	if err := json.Unmarshal(b.Input, &in); err != nil || in.Text == nil {
		return "", `say refused: its input must be {"text": "<your message>"}.`
	}
	switch n := len(*in.Text); {
	case strings.TrimSpace(*in.Text) == "":
		return "", "say refused: the text is empty. Call say with your message, or end your turn without say to stay silent."
	case n > MaxSay:
		return "", fmt.Sprintf("say refused: the text is %d bytes, over the limit of %d. Say it in fewer words and call say again.", n, MaxSay)
	}
	return *in.Text, ""
}

// toolResult is the answer to a tool call: always an error, since an
// accepted say ends the run.
type toolResult struct{ id, text string }

// conversation is a run's request so far, in its provider's shape. Each
// call's content begins with the content of the one before it, unchanged,
// so a provider can read that prefix from cache (ADR 0018). The bodies are
// not byte-for-byte prefixes: from the third call on, the earlier tool
// result's Anthropic cache breakpoint moves to the newest one.
type conversation struct {
	req    Request
	system []any // Anthropic's system blocks
	msgs   []any
	// latest is the Anthropic tool result holding the moving cache
	// breakpoint; the next one takes it, keeping within the four allowed.
	latest map[string]any
}

// ephemeral is an Anthropic cache breakpoint.
var ephemeral = map[string]any{"type": "ephemeral"}

func newConversation(req Request) (*conversation, error) {
	c := &conversation{req: req}
	switch req.Shape {
	case Messages:
		if req.System != "" {
			c.system = append(c.system, map[string]any{"type": "text", "text": req.System, "cache_control": ephemeral})
		}
		if req.SystemDepth != "" {
			c.system = append(c.system, map[string]any{"type": "text", "text": req.SystemDepth})
		}
	case ChatCompletions, Responses:
		if system := req.System + req.SystemDepth; system != "" {
			c.msgs = append(c.msgs, map[string]any{"role": "system", "content": system})
		}
	default:
		return nil, fmt.Errorf("agent: no API shape for this call; an api_path must end in one of %s", strings.Join(ShapeSuffixes(), ", "))
	}
	for i, t := range req.Turns {
		var content any = t.Text
		if req.Shape == Messages && i == len(req.Turns)-1 {
			content = []any{map[string]any{"type": "text", "text": t.Text, "cache_control": ephemeral}}
		}
		c.msgs = append(c.msgs, map[string]any{"role": t.Role, "content": content})
	}
	return c, nil
}

// add appends a call's output and the answers to its tool calls.
func (c *conversation) add(blocks []Block, results []toolResult) {
	switch c.req.Shape {
	case Messages:
		c.addMessages(blocks, results)
	case Responses:
		c.addResponses(blocks, results)
	default:
		c.addChatCompletions(blocks, results)
	}
}

// addResponses appends each output item as it came back, then the answers
// to its function calls.
func (c *conversation) addResponses(blocks []Block, results []toolResult) {
	for _, b := range blocks {
		switch {
		case b.Item != nil:
			c.msgs = append(c.msgs, b.Item)
		case b.Type == "text":
			c.msgs = append(c.msgs, map[string]any{"role": "assistant", "content": b.Text})
		case b.Type == "tool_use":
			c.msgs = append(c.msgs, map[string]any{"type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": toolArguments(b.Input)})
		}
	}
	for _, r := range results {
		c.msgs = append(c.msgs, map[string]any{"type": "function_call_output", "call_id": r.id, "output": r.text})
	}
}

// toolArguments is a tool call's input as the arguments string it came as.
func toolArguments(input json.RawMessage) string {
	var s string
	if json.Unmarshal(input, &s) == nil {
		return s // arguments that were not JSON, kept as sent
	}
	return string(input)
}

func (c *conversation) addMessages(blocks []Block, results []toolResult) {
	content := make([]any, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			content = append(content, map[string]any{"type": "text", "text": b.Text})
		case "thinking":
			content = append(content, map[string]any{"type": "thinking", "thinking": b.Thinking, "signature": b.Signature})
		case "redacted_thinking":
			content = append(content, map[string]any{"type": "redacted_thinking", "data": b.Data})
		case "tool_use":
			content = append(content, map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": b.Input})
		}
	}
	answers := make([]any, len(results))
	for i, r := range results {
		answers[i] = map[string]any{"type": "tool_result", "tool_use_id": r.id, "content": r.text, "is_error": true}
	}
	if c.latest != nil {
		delete(c.latest, "cache_control")
	}
	c.latest = answers[len(answers)-1].(map[string]any)
	c.latest["cache_control"] = ephemeral
	c.msgs = append(c.msgs,
		map[string]any{"role": "assistant", "content": content},
		map[string]any{"role": "user", "content": answers})
}

func (c *conversation) addChatCompletions(blocks []Block, results []toolResult) {
	var text strings.Builder
	var calls []any
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			calls = append(calls, map[string]any{"id": b.ID, "type": "function", "function": map[string]any{"name": b.Name, "arguments": toolArguments(b.Input)}})
		}
	}
	msg := map[string]any{"role": "assistant", "tool_calls": calls}
	if text.Len() > 0 {
		msg["content"] = text.String()
	}
	c.msgs = append(c.msgs, msg)
	for _, r := range results {
		c.msgs = append(c.msgs, map[string]any{"role": "tool", "tool_call_id": r.id, "content": r.text})
	}
}

// body is the next call's request body: the conversation and the say tool
// in the shape's own fields, with the profile's body merged over them. It
// fails only on a body value JSON cannot hold.
func (c *conversation) body() ([]byte, error) {
	v := map[string]any{"messages": c.msgs}
	switch c.req.Shape {
	case Messages:
		if len(c.system) > 0 {
			v["system"] = c.system
		}
		v["tools"] = []any{map[string]any{"name": sayTool, "description": sayDescription, "input_schema": saySchema}}
	case Responses:
		v = map[string]any{"input": c.msgs}
		v["tools"] = []any{map[string]any{"type": "function", "name": sayTool, "description": sayDescription, "parameters": saySchema}}
	default:
		v["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": sayTool, "description": sayDescription, "parameters": saySchema}}}
	}
	body, err := json.Marshal(mergeBody(v, c.req.Body))
	if err != nil {
		return nil, fmt.Errorf("agent: the profile's body: %w", err)
	}
	return body, nil
}

// call makes one model call with body, retrying what is worth retrying; a
// retry after the call began streaming resets it.
func (c Client) call(ctx context.Context, req Request, body []byte, ev Events, st *stream) (call, error) {
	wait := c.Backoff
	if wait == 0 {
		wait = time.Second
	}
	for n := 0; ; n++ {
		last := st.last
		st.started = false
		ev.record(RecordRequest, json.RawMessage(body))
		out, err := c.attempt(ctx, req, body, st)
		if err == nil {
			return out, nil
		}
		var retry *retryable
		if ctx.Err() != nil || !errors.As(err, &retry) || n == Retries {
			if ctx.Err() != nil {
				return call{}, ctx.Err()
			}
			return call{}, err
		}
		if st.started {
			st.last = last
			if ev.Reset != nil {
				ev.Reset()
			}
		}
		pause := wait
		if retry.after >= 0 {
			pause = min(retry.after, maxRetryAfter)
		}
		select {
		case <-ctx.Done():
			return call{}, ctx.Err()
		case <-time.After(pause):
		}
		wait *= 2
	}
}

// retryable is a failure worth trying again: after, when not negative, is
// how long the provider asked to wait.
type retryable struct {
	err   error
	after time.Duration
}

func (e *retryable) Error() string { return e.err.Error() }
func (e *retryable) Unwrap() error { return e.err }

func retry(err error) error { return &retryable{err: err, after: -1} }

// retryStatus is whether a provider's HTTP status is worth retrying: a rate
// limit, Anthropic's overload (529) or any 5xx.
func retryStatus(code int) bool { return code == http.StatusTooManyRequests || code >= 500 }

// retryAfter reads a Retry-After header in seconds; -1 when there is none.
func retryAfter(h http.Header) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil || secs < 0 {
		return -1
	}
	return time.Duration(secs) * time.Second
}

// retryEvents are the Anthropic stream error types worth retrying.
var retryEvents = map[string]bool{"overloaded_error": true, "rate_limit_error": true, "api_error": true}

func (c Client) attempt(ctx context.Context, req Request, body []byte, st *stream) (call, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL, bytes.NewReader(body))
	if err != nil {
		return call{}, fmt.Errorf("agent: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream, application/json")
	req.Shape.auth.set(hreq.Header, req.APIKey)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return call{}, retry(fmt.Errorf("agent: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		err := fmt.Errorf("agent: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
		if retryStatus(resp.StatusCode) {
			return call{}, &retryable{err: err, after: retryAfter(resp.Header)}
		}
		return call{}, err
	}
	var p parser
	switch req.Shape {
	case Messages:
		p = &anthropicParser{st: st}
	case Responses:
		p = &responsesParser{st: st}
	default:
		p = &openaiParser{st: st}
	}
	// The reply's type picks its reader, whatever the body's stream says.
	switch mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt {
	case "text/event-stream":
		err = readEvents(resp.Body, p.event)
	case "application/json":
		err = readJSON(resp.Body, p.reply)
	default:
		err = fmt.Errorf("agent: the reply is %q, neither an event stream nor JSON", mt)
	}
	if err != nil {
		return call{}, err
	}
	return p.result(), nil
}

// maxReply bounds a reply that is not streamed.
const maxReply = 16 << 20

// readJSON hands a reply that is not streamed to fn whole. A body that
// does not parse was cut off, like a stream that ends too soon.
func readJSON(r io.Reader, fn func(body []byte) error) error {
	body, err := io.ReadAll(io.LimitReader(r, maxReply))
	if err != nil {
		return retry(fmt.Errorf("agent: reading the reply: %w", err))
	}
	if !json.Valid(body) {
		return retry(errors.New("agent: the reply was cut off"))
	}
	return fn(body)
}

// readEvents hands each server-sent event's data to fn until fn says the
// call is done. A stream that ends before then was cut off.
func readEvents(r io.Reader, fn func(data string) (done bool, err error)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line != "" {
			if v, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(v, " "))
			}
			continue
		}
		if len(data) == 0 {
			continue
		}
		done, err := fn(strings.Join(data, "\n"))
		data = data[:0]
		if err != nil || done {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return retry(fmt.Errorf("agent: reading the reply: %w", err))
	}
	return retry(errors.New("agent: the reply was cut off"))
}

// parser assembles one call's reply into blocks: a stream event by event,
// or a reply that is not streamed whole, which streams its blocks as it is
// read so that both look the same to Events.
type parser interface {
	event(data string) (done bool, err error)
	reply(body []byte) error
	result() call
}

// anthropicParser reads a Messages stream.
type anthropicParser struct {
	st     *stream
	blocks []Block
	inputs []strings.Builder // tool_use input JSON as it streams, by index
	out    call
}

func (p *anthropicParser) event(data string) (bool, error) {
	var ev struct {
		Type         string          `json:"type"`
		Index        int             `json:"index"`
		ContentBlock Block           `json:"content_block"`
		Message      json.RawMessage `json:"message"`
		Usage        map[string]any  `json:"usage"`
		Delta        struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return false, fmt.Errorf("agent: bad event: %w", err)
	}
	switch ev.Type {
	case "message_start":
		var m struct {
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal(ev.Message, &m) == nil {
			p.mergeUsage(m.Usage)
		}
	case "content_block_start":
		if ev.Index < 0 || ev.Index > 1024 {
			return false, fmt.Errorf("agent: bad event: block index %d", ev.Index)
		}
		for len(p.blocks) <= ev.Index {
			p.blocks = append(p.blocks, Block{})
			p.inputs = append(p.inputs, strings.Builder{})
		}
		b := ev.ContentBlock
		b.Input = nil // it streams as input_json_delta
		p.blocks[ev.Index] = b
		switch b.Type {
		case "text":
			p.st.block(KindText, b.Text)
		case "thinking":
			p.st.block(KindThinking, b.Thinking)
		case "tool_use":
			p.st.block(KindTool, b.Name+" ")
		}
	case "content_block_delta":
		if ev.Index < 0 || ev.Index >= len(p.blocks) {
			return false, fmt.Errorf("agent: bad event: delta for block %d before it started", ev.Index)
		}
		b := &p.blocks[ev.Index]
		switch ev.Delta.Type {
		case "text_delta":
			b.Text += ev.Delta.Text
			p.st.more(KindText, ev.Delta.Text)
		case "thinking_delta":
			b.Thinking += ev.Delta.Thinking
			p.st.more(KindThinking, ev.Delta.Thinking)
		case "signature_delta":
			b.Signature += ev.Delta.Signature
		case "input_json_delta":
			p.inputs[ev.Index].WriteString(ev.Delta.PartialJSON)
			p.st.more(KindTool, ev.Delta.PartialJSON)
		}
	case "message_delta":
		p.out.stop = ev.Delta.StopReason
		p.mergeUsage(ev.Usage)
	case "message_stop":
		return true, nil
	case "error":
		err := fmt.Errorf("agent: %s: %s", ev.Error.Type, ev.Error.Message)
		if retryEvents[ev.Error.Type] {
			err = retry(err)
		}
		return false, err
	}
	return false, nil
}

// reply reads a Messages reply that is not streamed.
func (p *anthropicParser) reply(body []byte) error {
	var m struct {
		Type       string         `json:"type"`
		Content    []Block        `json:"content"`
		StopReason string         `json:"stop_reason"`
		Usage      map[string]any `json:"usage"`
		Error      struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return fmt.Errorf("agent: bad reply: %w", err)
	}
	if m.Type == "error" {
		err := fmt.Errorf("agent: %s: %s", m.Error.Type, m.Error.Message)
		if retryEvents[m.Error.Type] {
			err = retry(err)
		}
		return err
	}
	for _, b := range m.Content {
		var input strings.Builder
		switch b.Type {
		case "text":
			p.st.block(KindText, b.Text)
		case "thinking":
			p.st.block(KindThinking, b.Thinking)
		case "tool_use":
			var compact bytes.Buffer
			if json.Compact(&compact, b.Input) == nil {
				input.Write(compact.Bytes())
			}
			p.st.block(KindTool, b.Name+" ")
			p.st.more(KindTool, input.String())
		}
		b.Input = nil
		p.blocks = append(p.blocks, b)
		p.inputs = append(p.inputs, input)
	}
	p.out.stop = m.StopReason
	p.mergeUsage(m.Usage)
	return nil
}

func (p *anthropicParser) mergeUsage(u map[string]any) {
	if len(u) == 0 {
		return
	}
	if p.out.usage == nil {
		p.out.usage = map[string]any{}
	}
	for k, v := range u {
		p.out.usage[k] = v
	}
}

func (p *anthropicParser) result() call {
	for i := range p.blocks {
		if p.blocks[i].Type == "tool_use" {
			p.blocks[i].Input = toolInput(p.inputs[i].String())
		}
	}
	p.out.blocks = p.blocks
	return p.out
}

// toolInput is a tool call's streamed input as JSON: {} when empty, and a
// JSON string when it is not valid JSON, so the refusal can say so.
func toolInput(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	raw, _ := json.Marshal(s)
	return raw
}

// openaiParser reads a Chat Completions stream.
type openaiParser struct {
	st    *stream
	text  strings.Builder
	calls []Block
	args  []strings.Builder
	out   call
}

func (p *openaiParser) event(data string) (bool, error) {
	if data == "[DONE]" {
		return true, nil
	}
	var ev struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return false, fmt.Errorf("agent: bad event: %w", err)
	}
	if len(ev.Usage) > 0 {
		p.out.usage = ev.Usage
	}
	for _, c := range ev.Choices {
		if c.Delta.Content != "" {
			if p.text.Len() == 0 {
				p.st.block(KindText, c.Delta.Content)
			} else {
				p.st.more(KindText, c.Delta.Content)
			}
			p.text.WriteString(c.Delta.Content)
		}
		for _, tc := range c.Delta.ToolCalls {
			if tc.Index < 0 || tc.Index > 1024 {
				return false, fmt.Errorf("agent: bad event: tool call index %d", tc.Index)
			}
			for len(p.calls) <= tc.Index {
				p.calls = append(p.calls, Block{Type: "tool_use"})
				p.args = append(p.args, strings.Builder{})
			}
			b := &p.calls[tc.Index]
			b.ID += tc.ID
			if b.Name == "" && tc.Function.Name != "" {
				p.st.block(KindTool, tc.Function.Name+" ")
			}
			b.Name += tc.Function.Name
			p.args[tc.Index].WriteString(tc.Function.Arguments)
			p.st.more(KindTool, tc.Function.Arguments)
		}
		if c.FinishReason != "" {
			p.out.stop = c.FinishReason
		}
	}
	return false, nil
}

// reply reads a Chat Completions reply that is not streamed.
func (p *openaiParser) reply(body []byte) error {
	var r struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("agent: bad reply: %w", err)
	}
	if len(r.Usage) > 0 {
		p.out.usage = r.Usage
	}
	for _, c := range r.Choices {
		if c.Message.Content != "" {
			p.st.block(KindText, c.Message.Content)
			p.text.WriteString(c.Message.Content)
		}
		for _, tc := range c.Message.ToolCalls {
			p.st.block(KindTool, tc.Function.Name+" ")
			p.st.more(KindTool, tc.Function.Arguments)
			var args strings.Builder
			args.WriteString(tc.Function.Arguments)
			p.calls = append(p.calls, Block{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name})
			p.args = append(p.args, args)
		}
		if c.FinishReason != "" {
			p.out.stop = c.FinishReason
		}
	}
	return nil
}

func (p *openaiParser) result() call {
	if p.text.Len() > 0 {
		p.out.blocks = append(p.out.blocks, Block{Type: "text", Text: p.text.String()})
	}
	for i, b := range p.calls {
		b.Input = toolInput(p.args[i].String())
		p.out.blocks = append(p.out.blocks, b)
	}
	return p.out
}
