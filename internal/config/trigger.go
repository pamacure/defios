package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/secret"
	"defios.com/internal/signreq"
)

// Job is what a Trigger runs when it fires (ADR 0032): exactly one of
// summoning an agent, running a local command, or calling a webhook. It is
// defined once, under [jobs.<name>] in the Group's config.toml, so any
// number of Hooks may name it.
type Job struct {
	// Agent is the Handle of the agent an agent Job summons, with Prompt as
	// its instruction, in Target: "group", a Conversation's Workspace name,
	// or unset for the event's Conversation. Parsed now; run by a later
	// issue (#126).
	Agent  string `toml:"agent,omitempty"`
	Prompt string `toml:"prompt,omitempty"`
	Target string `toml:"target,omitempty"`
	// Exec is an exec Job's argument list, run with no shell: the program
	// first, found on PATH. $DEFIOS_* variables in it are expanded. Secrets
	// are the Secrets set in its environment, each under its own name.
	Exec    []string `toml:"exec,omitempty"`
	Secrets []string `toml:"secrets,omitempty"`
	// Webhook is the URL a webhook Job POSTs the event to, an http(s) URL
	// or a Secret reference; each Headers value is a literal or a Secret
	// reference.
	Webhook string            `toml:"webhook,omitempty"`
	Headers map[string]string `toml:"headers,omitempty"`
	// Timeout bounds an exec Job's run or each webhook attempt; zero is
	// the kind's default.
	Timeout Age `toml:"timeout,omitzero"`
}

// Kinds of Job.
const (
	JobAgent   = "agent"
	JobExec    = "exec"
	JobWebhook = "webhook"
)

// Default timeouts, for an unset timeout.
const (
	DefaultExecTimeout    = 60 * time.Second
	DefaultWebhookTimeout = 10 * time.Second
)

// Kind is which of agent, exec or webhook j is.
func (j Job) Kind() string {
	switch {
	case j.Agent != "":
		return JobAgent
	case len(j.Exec) > 0:
		return JobExec
	case j.Webhook != "":
		return JobWebhook
	}
	return ""
}

// ExecTimeout is how long an exec run of j may take.
func (j Job) ExecTimeout() time.Duration { return j.timeout(DefaultExecTimeout) }

// WebhookTimeout is how long each webhook attempt of j may take.
func (j Job) WebhookTimeout() time.Duration { return j.timeout(DefaultWebhookTimeout) }

func (j Job) timeout(def time.Duration) time.Duration {
	if j.Timeout == 0 {
		return def
	}
	return time.Duration(j.Timeout)
}

// Hook is a Trigger that fires on a live COSL event its filters all match,
// running its Jobs in order until one fails (ADR 0032). An unset filter
// matches every event.
type Hook struct {
	// Events are message type names, such as "sign.request", and Sign
	// Request statuses as "request.<status>".
	Events []string `toml:"events,omitempty"`
	// From is "self", "others" or Node IDs: who wrote the event.
	From Names `toml:"from,omitempty"`
	// Handles are agents' Handles, or "user" for a person.
	Handles []string `toml:"handles,omitempty"`
	// Conversation is Workspace names, or "group" for Group-level talk.
	Conversation Names `toml:"conversation,omitempty"`
	// Text is a regular expression an app.freeform's text must match.
	Text string `toml:"text,omitempty"`
	// Jobs are the Jobs it runs, by name, in order.
	Jobs []string `toml:"jobs"`
}

// Names is a list a config.toml may write as one string.
type Names []string

// UnmarshalTOML reads one string or a list of strings.
func (n *Names) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		*n = Names{x}
		return nil
	case []any:
		out := make(Names, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return fmt.Errorf("want a string or a list of strings, not a list holding %T", e)
			}
			out = append(out, s)
		}
		*n = out
		return nil
	}
	return fmt.Errorf("want a string or a list of strings, not %T", v)
}

// Hook from filter values naming no Node.
const (
	FromSelf   = "self"
	FromOthers = "others"
)

// HandleUser is a Hook's handles entry for a person's message.
const HandleUser = "user"

// ConversationGroup is a Hook's conversation entry for Group-level talk.
const ConversationGroup = "group"

// RequestEventPrefix starts an event that is a Sign Request's status.
const RequestEventPrefix = "request."

// workspaceName is a Conversation Workspace's name: its Key's curve and a
// hex prefix of its public key (key.WorkspaceNames).
var workspaceName = regexp.MustCompile(`^[a-z0-9]+-[0-9a-f]+$`)

// validateTriggers checks c's Jobs and Hooks.
func (c Config) validateTriggers(md toml.MetaData) error {
	for name, j := range c.Jobs {
		if err := group.ValidateAlias(name); err != nil {
			return fmt.Errorf("[jobs.%s]: name: %w", name, err)
		}
		if err := j.validate(md, name); err != nil {
			return fmt.Errorf("[jobs.%s]: %w", name, err)
		}
	}
	for name, h := range c.Hooks {
		if err := group.ValidateAlias(name); err != nil {
			return fmt.Errorf("[hooks.%s]: name: %w", name, err)
		}
		if err := h.validate(c.Jobs); err != nil {
			return fmt.Errorf("[hooks.%s]: %w", name, err)
		}
	}
	return nil
}

func (j Job) validate(md toml.MetaData, name string) error {
	kinds := 0
	for _, set := range []bool{j.Agent != "", md.IsDefined("jobs", name, "exec"), j.Webhook != ""} {
		if set {
			kinds++
		}
	}
	if kinds != 1 {
		return errors.New("a Job has exactly one of agent, exec or webhook")
	}
	if md.IsDefined("jobs", name, "exec") && (len(j.Exec) == 0 || j.Exec[0] == "") {
		return errors.New("exec must name a program to run")
	}
	only := func(key string, set bool, kinds ...string) error {
		if set && !slices.Contains(kinds, j.Kind()) {
			return fmt.Errorf("%s is for %s Jobs only", key, strings.Join(kinds, " and "))
		}
		return nil
	}
	if err := errors.Join(
		only("prompt", j.Prompt != "", JobAgent),
		only("target", j.Target != "", JobAgent),
		only("secrets", j.Secrets != nil, JobExec),
		only("headers", j.Headers != nil, JobWebhook),
		only("timeout", md.IsDefined("jobs", name, "timeout"), JobExec, JobWebhook),
	); err != nil {
		return err
	}
	switch j.Kind() {
	case JobAgent:
		if err := ValidateHandle(j.Agent); err != nil {
			return fmt.Errorf("agent: %w", err)
		}
		if j.Target != "" && j.Target != ConversationGroup && !workspaceName.MatchString(j.Target) {
			return fmt.Errorf("target %q is neither %q nor a Conversation's Workspace name", j.Target, ConversationGroup)
		}
	case JobExec:
		for i, s := range j.Secrets {
			if err := secret.ValidateName(s); err != nil {
				return fmt.Errorf("secrets: %w", err)
			}
			if slices.Contains(j.Secrets[:i], s) {
				return fmt.Errorf("secrets: %q is listed twice", s)
			}
		}
	case JobWebhook:
		if err := validateURLOrRef(j.Webhook); err != nil {
			return fmt.Errorf("webhook: %w", err)
		}
		for k, v := range j.Headers {
			if strings.HasPrefix(v, "$") {
				if _, err := secret.ParseRef(v); err != nil {
					return fmt.Errorf("headers.%s: %w", k, err)
				}
			}
		}
	}
	return nil
}

// validateURLOrRef accepts a Secret reference, or an http(s) URL with a
// host. A literal URL is not echoed, for it may carry a token.
func validateURLOrRef(v string) error {
	if strings.HasPrefix(v, "$") {
		_, err := secret.ParseRef(v)
		return err
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an http or https URL with a host, or a Secret reference")
	}
	return nil
}

func (h Hook) validate(jobs map[string]Job) error {
	if len(h.Jobs) == 0 {
		return errors.New("jobs must name at least one Job")
	}
	for i, name := range h.Jobs {
		if _, ok := jobs[name]; !ok {
			return fmt.Errorf("jobs: no [jobs.%s]", name)
		}
		if slices.Contains(h.Jobs[:i], name) {
			return fmt.Errorf("jobs: %q is listed twice", name)
		}
	}
	for _, ev := range h.Events {
		if err := ValidateEvent(ev); err != nil {
			return fmt.Errorf("events: %w", err)
		}
	}
	for _, f := range h.From {
		if f == FromSelf || f == FromOthers {
			continue
		}
		if _, err := identity.ParseID(f); err != nil {
			return fmt.Errorf("from: %q is neither %q, %q nor a Node ID", f, FromSelf, FromOthers)
		}
	}
	for _, hd := range h.Handles {
		if hd == HandleUser {
			continue
		}
		if err := ValidateHandle(hd); err != nil {
			return fmt.Errorf("handles: %w", err)
		}
	}
	for _, c := range h.Conversation {
		if c != ConversationGroup && !workspaceName.MatchString(c) {
			return fmt.Errorf("conversation: %q is neither %q nor a Conversation's Workspace name", c, ConversationGroup)
		}
	}
	if _, err := regexp.Compile(h.Text); err != nil {
		return fmt.Errorf("text: %w", err)
	}
	return nil
}

// ValidateEvent checks an event name a Hook filters on: a COSL message
// type, or a Sign Request status as request.<status>.
func ValidateEvent(ev string) error {
	if st, ok := strings.CutPrefix(ev, RequestEventPrefix); ok {
		if !slices.Contains(signreq.Statuses, st) {
			return fmt.Errorf("%q: no Sign Request status %q; the statuses are %s", ev, st, strings.Join(signreq.Statuses, ", "))
		}
		return nil
	}
	if _, ok := cosl.TypeByName(ev); !ok {
		return fmt.Errorf("%q is no message type, nor request.<status>", ev)
	}
	return nil
}

// DefaultJobs are the Jobs a new Group's config.toml is written with, and
// no Trigger runs them until the operator writes one that does: sign_accept
// and sign_refuse, which take this Node's stance on the Sign Request the
// event names. They are ordinary exec Jobs, to read, change or delete.
func DefaultJobs() map[string]Job {
	return map[string]Job{
		"sign_accept": {Exec: []string{"defios", "sign", "accept", "$DEFIOS_SIGN_REQUEST_ID"}},
		"sign_refuse": {Exec: []string{"defios", "sign", "refuse", "$DEFIOS_SIGN_REQUEST_ID", "--reason", "refused by this Node's sign_refuse Job"}},
	}
}
