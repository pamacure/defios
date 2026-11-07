// Package trigger matches live COSL events against this Node's Hooks and
// runs the exec and webhook Jobs they name (docs/adr/0032-triggers-run-jobs-live-only.md).
// Which events are live, and when a Hook fires, is serve's to decide.
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

// Event is what a Hook fires on, as a Job is given it: as JSON on an exec
// Job's stdin or a webhook's body. It is a COSL message, Type its message
// type, or a change in a Sign Request's status, Type "request.<status>".
type Event struct {
	Type    string `json:"type"`
	Trigger string `json:"trigger"` // the Hook that fired
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

// Runner runs exec and webhook Jobs for one Group.
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
	// Backoff is the wait before a webhook's first retry, doubling after
	// each; zero is a second.
	Backoff time.Duration
}

// webhookRetries is how many times a webhook is retried after a 5xx or a
// dropped connection.
const webhookRetries = 3

// Run runs j, named name, on ev. Its error is why the Job failed.
func (r Runner) Run(ctx context.Context, name string, j config.Job, ev Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	switch j.Kind() {
	case config.JobExec:
		return r.exec(ctx, name, j, ev, body)
	case config.JobWebhook:
		return r.webhook(ctx, j, body)
	}
	return fmt.Errorf("%s Jobs do not run yet", j.Kind())
}

// Vars are the DEFIOS_* variables an exec Job is given for ev. DEFIOS_DEPTH
// is the depth a message the Job writes takes: one below the event's.
func Vars(ev Event) map[string]string {
	return map[string]string{
		"DEFIOS_GROUP":           ev.Group,
		"DEFIOS_TRIGGER":         ev.Trigger,
		"DEFIOS_EVENT":           ev.Type,
		"DEFIOS_MSG_ID":          ev.MsgID,
		"DEFIOS_SIGN_REQUEST_ID": ev.SignRequestID,
		"DEFIOS_CONVERSATION":    ev.Conversation,
		"DEFIOS_DEPTH":           strconv.FormatUint(uint64(ev.Depth)+1, 10),
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

func (r Runner) exec(ctx context.Context, name string, j config.Job, ev Event, stdin []byte) error {
	vars := Vars(ev)
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
	ctx, cancel := context.WithTimeout(ctx, j.ExecTimeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin = r.Dir, env, bytes.NewReader(stdin)
	cmd.WaitDelay = time.Second // a child holding the output open does not outlast the timeout
	out := &lineLog{logf: r.Logf, prefix: "job " + name + ": "}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	out.flush()
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

func (r Runner) webhook(ctx context.Context, j config.Job, body []byte) error {
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
		retry, err = r.post(ctx, url, headers, body, j.WebhookTimeout())
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

// post is one webhook attempt, and whether a failure is worth retrying.
// A URL from a Secret is never echoed.
func (r Runner) post(ctx context.Context, url string, headers http.Header, body []byte, timeout time.Duration) (retry bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, errors.New("webhook: the URL does not make a request")
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
			return true, fmt.Errorf("webhook: no response within %s", timeout)
		}
		return true, errors.New("webhook: no response: the connection failed or was dropped")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode >= 500:
		return true, fmt.Errorf("webhook: %s", resp.Status)
	}
	return false, fmt.Errorf("webhook: %s", resp.Status)
}

// Firing is the names of the Hooks in hooks that fire on ev, in name
// order: those it matches, if its depth is below maxDepth, the Group's
// [agent] max_depth, so that Jobs answering events cannot loop unbounded.
func Firing(hooks map[string]config.Hook, ev Event, self identity.ID, maxDepth int) []string {
	if int(ev.Depth) >= maxDepth {
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
