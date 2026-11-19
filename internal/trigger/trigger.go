// Package trigger matches live COSL events against this Node's Hooks,
// finds when its Schedules are due, and runs the Jobs they name, agent
// Jobs through serve (docs/adr/0032-triggers-run-jobs-live-only.md).
// Which events are live, and when a Trigger fires, is serve's to decide.
package trigger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/identity"
	"defios.com/internal/secret"
	"defios.com/internal/signreq"
)

// Event is what a Trigger fires on, as a Job is given it: as JSON on an
// exec Job's stdin or a webhook's body. For a Hook it is a COSL message,
// Type its message type, or a change in a Sign Request's status, Type
// "request.<status>"; for a Schedule, Type "schedule" and the Time it
// fired for, from this Node at depth 0.
type Event struct {
	Type    string `json:"type"`
	Trigger string `json:"trigger"` // the Hook or Schedule that fired
	Group   string `json:"group"`   // the Group's alias on this Node
	Seq     uint64 `json:"seq,omitempty"`
	// MsgID is the message's hex msg_id; for a status, the request's.
	MsgID string `json:"msg_id,omitempty"`
	// From is the hex Node ID that wrote it; for a status, the proposer.
	From string `json:"from"`
	// Handle is the agent that wrote it, or "user" for a person; empty for
	// a message type that names neither.
	Handle string `json:"handle,omitempty"`
	// Conversation is its Workspace name, or "group" for Group-level talk.
	Conversation   string `json:"conversation"`
	ConversationID string `json:"conversation_id,omitempty"` // hex; empty for Group-level talk
	// Depth is how deep in a chain of agents it was written.
	Depth uint32 `json:"depth"`
	// Text is an app.freeform's.
	Text string `json:"text,omitempty"`
	// SignRequestID is the hex msg_id of the Sign Request it is about.
	SignRequestID string `json:"sign_request_id,omitempty"`
	// Status is a request.<status> event's new status.
	Status string `json:"status,omitempty"`
	// Time is the RFC 3339 time a Schedule fired for.
	Time string `json:"time,omitempty"`
}

// ScheduleEvent is the Type of a Schedule's firing.
const ScheduleEvent = "schedule"

// Due is the firing of s that a check at now runs, if any: the latest
// time s fires after last, the previous check, and no more than slack
// before now. A firing before that was missed, or run already, and is
// never made up.
func Due(s config.Schedule, last, now time.Time, slack time.Duration) (time.Time, bool) {
	from := now.Add(-slack)
	if last.After(from) {
		from = last
	}
	t := s.Next(from)
	if t.IsZero() || t.After(now) {
		return time.Time{}, false
	}
	for {
		n := s.Next(t)
		if n.IsZero() || n.After(now) {
			return t, true
		}
		t = n
	}
}

// Matches reports whether every filter h sets allows ev, which self, this
// Node, sees.
func Matches(h config.Hook, ev Event, self identity.ID) bool {
	if len(h.Events) > 0 && !slices.Contains(h.Events, ev.Type) {
		return false
	}
	if len(h.From) > 0 && !slices.ContainsFunc(h.From, func(f string) bool {
		switch f {
		case config.FromSelf:
			return ev.From == self.String()
		case config.FromOthers:
			return ev.From != self.String()
		}
		return f == ev.From
	}) {
		return false
	}
	if len(h.Handles) > 0 && (ev.Handle == "" || !slices.Contains(h.Handles, ev.Handle)) {
		return false
	}
	if len(h.Conversation) > 0 && !slices.Contains(h.Conversation, ev.Conversation) {
		return false
	}
	if h.Text != "" {
		re, err := regexp.Compile(h.Text)
		if err != nil || ev.Type != cosl.TypeName(cosl.TypeAppFreeform) || !re.MatchString(ev.Text) {
			return false
		}
	}
	return true
}

// Runner runs Jobs for one Group: exec and webhook Jobs itself, agent Jobs
// by Agent.
type Runner struct {
	Group string // the Group's alias
	// Dir is the Group Workspace, where an exec Job runs.
	Dir string
	// Env is set in every exec Job's environment besides the event's
	// DEFIOS_* variables: where this Node's CLI finds it.
	Env map[string]string
	// Secrets resolves a Job's Secrets; a fresh one per run, so a `secret
	// set` needs no restart.
	Secrets func() *secret.Resolver
	// Getenv is serve's environment, of which an exec Job gets PATH only.
	Getenv func(string) string
	// Logf is the serve log.
	Logf func(format string, args ...any)
	// Client calls webhooks; nil is a plain client.
	Client *http.Client
	// Agent runs agent Job name, j, on ev, and returns once its chain has
	// ended; nil runs none.
	Agent func(ctx context.Context, name string, j config.Job, ev Event) error
	// JobRun is the token naming the Job Run of the Job it runs, given an
	// exec Job as DEFIOS_JOB_RUN: the CLI hands it to serve with each talk
	// message the Job writes, and serve records the message on that Job Run
	// (ADR 0040). Empty outside a Trigger's firing.
	JobRun string
	// Backoff is the wait before a webhook's first retry, doubling after
	// each; zero is a second.
	Backoff time.Duration
}

// webhookRetries is how many times a webhook is retried after a 5xx or a
// dropped connection.
const webhookRetries = 3

// Output is what a Job's run produced, as its Job Run keeps it (ADR
// 0040): an exec Job's or a webhook's, as far as it got. An agent Job's
// run stays in its agent's Transcript.
type Output struct {
	Exec    *ExecOutput    `json:"exec,omitempty"`
	Webhook *WebhookOutput `json:"webhook,omitempty"`
}

// ExecOutput is an exec Job's command, as run, its exit code, nil if it
// never started, and the last OutputMax bytes of its stdout and stderr.
type ExecOutput struct {
	Command  []string `json:"command"`
	ExitCode *int     `json:"exit_code,omitempty"`
	Stdout   string   `json:"stdout"`
	Stderr   string   `json:"stderr"`
}

// WebhookOutput is a webhook Job's request and responses. URL is as
// configured, a Secret's $NAME unexpanded; no request header is kept, and
// of the last response's headers only its Content-Type. Body is the last
// OutputMax bytes of the last response's body.
type WebhookOutput struct {
	Method      string    `json:"method"`
	URL         string    `json:"url"`
	Attempts    []Attempt `json:"attempts"`
	Body        string    `json:"body,omitempty"`
	ContentType string    `json:"content_type,omitempty"`
}

// Attempt is one webhook attempt: its response's HTTP status, 0 for none,
// and its error, if it failed.
type Attempt struct {
	Status int    `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// OutputMax is how much of a Job's output, or a webhook's response body,
// its Job Run keeps: the last of it, marked where it was cut.
const OutputMax = 64 << 10

// tail keeps the last max bytes written to it.
type tail struct {
	max int
	buf []byte
	cut int64 // bytes dropped from the front
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.cut += int64(over)
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

// String is what was kept, behind a marker if anything was cut.
func (t *tail) String() string {
	if t.cut == 0 {
		return string(t.buf)
	}
	return fmt.Sprintf("[%d bytes cut]\n%s", t.cut, t.buf)
}

// Run runs j, named name, on ev, and returns what it produced. Its error
// is why the Job failed.
func (r Runner) Run(ctx context.Context, name string, j config.Job, ev Event) (Output, error) {
	body, err := json.Marshal(ev)
	if err != nil {
		return Output{}, err
	}
	switch j.Kind() {
	case config.JobAgent:
		if r.Agent != nil {
			return Output{}, r.Agent(ctx, name, j, ev)
		}
	case config.JobExec:
		x := &ExecOutput{}
		return Output{Exec: x}, r.exec(ctx, name, j, ev, body, x)
	case config.JobWebhook:
		w := &WebhookOutput{Method: http.MethodPost, URL: j.Webhook}
		return Output{Webhook: w}, r.webhook(ctx, j, body, w)
	}
	return Output{}, fmt.Errorf("%s Jobs do not run here", j.Kind())
}

// Vars are the DEFIOS_* variables an exec Job is given for ev, run as Job
// Run jobRun. DEFIOS_DEPTH is the depth a message the Job writes takes: one
// below the event's.
func Vars(ev Event, jobRun string) map[string]string {
	return map[string]string{
		"DEFIOS_GROUP":           ev.Group,
		"DEFIOS_TRIGGER":         ev.Trigger,
		"DEFIOS_EVENT":           ev.Type,
		"DEFIOS_MSG_ID":          ev.MsgID,
		"DEFIOS_SIGN_REQUEST_ID": ev.SignRequestID,
		"DEFIOS_CONVERSATION":    ev.Conversation,
		"DEFIOS_DEPTH":           strconv.FormatUint(uint64(ev.Depth)+1, 10),
		"DEFIOS_JOB_RUN":         jobRun,
	}
}

// defiosVar is a $DEFIOS_* or ${DEFIOS_*} reference in an exec argument.
var defiosVar = regexp.MustCompile(`\$\{(DEFIOS_[A-Z0-9_]+)\}|\$(DEFIOS_[A-Z0-9_]+)`)

// Expand is arg with each $DEFIOS_* reference replaced by its value in
// vars, empty if unset. Nothing else in arg is touched.
func Expand(arg string, vars map[string]string) string {
	return defiosVar.ReplaceAllStringFunc(arg, func(m string) string {
		return vars[strings.Trim(m, "${}")]
	})
}

func (r Runner) exec(ctx context.Context, name string, j config.Job, ev Event, stdin []byte, x *ExecOutput) error {
	vars := Vars(ev, r.JobRun)
	for k, v := range r.Env {
		vars[k] = v
	}
	env := []string{"PATH=" + r.Getenv("PATH")}
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	if len(j.Secrets) > 0 {
		res := r.Secrets()
		for _, s := range j.Secrets {
			v, err := res.Resolve("$" + s)
			if err != nil {
				return err
			}
			env = append(env, s+"="+v)
		}
	}
	argv := make([]string, len(j.Exec))
	for i, a := range j.Exec {
		argv[i] = Expand(a, vars)
	}
	x.Command = argv
	ctx, cancel := context.WithTimeout(ctx, j.ExecTimeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin = r.Dir, env, bytes.NewReader(stdin)
	cmd.WaitDelay = time.Second // a child holding the output open does not outlast the timeout
	// Each stream to the log on its own, so a line of one is never split
	// by the other's.
	outLog := &lineLog{logf: r.Logf, prefix: "job " + name + ": "}
	errLog := &lineLog{logf: r.Logf, prefix: "job " + name + ": "}
	stdout, stderr := &tail{max: OutputMax}, &tail{max: OutputMax}
	cmd.Stdout, cmd.Stderr = io.MultiWriter(outLog, stdout), io.MultiWriter(errLog, stderr)
	err := cmd.Run()
	outLog.flush()
	errLog.flush()
	x.Stdout, x.Stderr = stdout.String(), stderr.String()
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		x.ExitCode = &code
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("timed out after %s", j.ExecTimeout())
	}
	return err
}

// lineLog writes each line of a Job's output to the serve log.
type lineLog struct {
	logf    func(string, ...any)
	prefix  string
	partial []byte
}

func (l *lineLog) Write(p []byte) (int, error) {
	l.partial = append(l.partial, p...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			return len(p), nil
		}
		l.logf("%s%s", l.prefix, l.partial[:i])
		l.partial = l.partial[i+1:]
	}
}

func (l *lineLog) flush() {
	if len(l.partial) > 0 {
		l.logf("%s%s", l.prefix, l.partial)
		l.partial = nil
	}
}

func (r Runner) webhook(ctx context.Context, j config.Job, body []byte, w *WebhookOutput) error {
	res := r.Secrets()
	url := j.Webhook
	if strings.HasPrefix(url, "$") {
		var err error
		if url, err = res.Resolve(url); err != nil {
			return err
		}
	}
	headers := http.Header{"Content-Type": {"application/json"}}
	for k, v := range j.Headers {
		if strings.HasPrefix(v, "$") {
			var err error
			if v, err = res.Resolve(v); err != nil {
				return err
			}
		}
		headers.Set(k, v)
	}
	wait := r.Backoff
	if wait == 0 {
		wait = time.Second
	}
	var err error
	for attempt := 0; ; attempt++ {
		var retry bool
		var a Attempt
		retry, a, err = r.post(ctx, url, headers, body, j.WebhookTimeout(), w)
		w.Attempts = append(w.Attempts, a)
		if !retry || attempt == webhookRetries {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait *= 2
	}
}

// post is one webhook attempt, as w keeps it, and whether a failure is
// worth retrying. A response replaces w's body and Content-Type. A URL
// from a Secret is never echoed.
func (r Runner) post(ctx context.Context, url string, headers http.Header, body []byte, timeout time.Duration, w *WebhookOutput) (retry bool, a Attempt, err error) {
	defer func() {
		if err != nil {
			a.Error = err.Error()
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, a, errors.New("webhook: the URL does not make a request")
	}
	req.Header = headers.Clone()
	client := r.Client
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		// Not err itself: it quotes the URL, which may be a Secret.
		if ctx.Err() == context.DeadlineExceeded {
			return true, a, fmt.Errorf("webhook: no response within %s", timeout)
		}
		return true, a, errors.New("webhook: no response: the connection failed or was dropped")
	}
	defer resp.Body.Close()
	a.Status = resp.StatusCode
	// The whole body, within the attempt's timeout, to keep its end.
	kept := &tail{max: OutputMax}
	if _, err := io.Copy(kept, resp.Body); err != nil {
		kept.Write([]byte("\n[the response ended early]"))
	}
	w.Body, w.ContentType = kept.String(), resp.Header.Get("Content-Type")
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, a, nil
	case resp.StatusCode >= 500:
		return true, a, fmt.Errorf("webhook: %s", resp.Status)
	}
	return false, a, fmt.Errorf("webhook: %s", resp.Status)
}

// Firing is the names of the Hooks in hooks that fire on ev, in name
// order: those it matches, if its depth is at most maxDepth, the Group's
// [agent] max_depth, so that Jobs answering events cannot loop unbounded.
// An event at maxDepth fires, so a Hook can answer an agent's reply, but no
// agent may answer it in turn.
func Firing(hooks map[string]config.Hook, ev Event, self identity.ID, maxDepth int) []string {
	if int(ev.Depth) > maxDepth {
		return nil
	}
	var out []string
	for name, h := range hooks {
		if Matches(h, ev, self) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// StatusChanges is the request.<status> events from prev, each Sign
// Request's status by hex msg_id, to list, in list's order, and the
// statuses list now holds. A request not in prev is a change from nothing.
// The events name their Conversation by ID only, and no Trigger or Group.
func StatusChanges(prev map[string]string, list []signreq.Summary) ([]Event, map[string]string) {
	var out []Event
	cur := make(map[string]string, len(list))
	for _, s := range list {
		cur[s.MsgID] = s.Status
		if was, ok := prev[s.MsgID]; ok && was == s.Status {
			continue
		}
		handle := s.ProposerHandle
		if handle == "" {
			handle = config.HandleUser
		}
		out = append(out, Event{
			Type: config.RequestEventPrefix + s.Status, Seq: s.Seq, MsgID: s.MsgID, SignRequestID: s.MsgID,
			From: s.Proposer, Handle: handle, Depth: s.Depth, ConversationID: s.Conversation, Status: s.Status,
		})
	}
	return out, cur
}
