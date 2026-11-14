package shell

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/node"
	"defios.com/internal/trigger"
)

// The Triggers page: the edit-target Group's Jobs, and the Hooks and
// Schedules that run them (ADR 0032), as forms, with "Edit TOML" over
// its config.toml for what a form can't change. Every change goes
// through internal/node.

// TriggersNode is what the Triggers page needs of a Node besides the rest
// of Node.
type TriggersNode interface {
	Jobs(ctx context.Context, group string) ([]node.Job, error)
	AddJob(ctx context.Context, group, name string, j config.Job) error
	EditJob(ctx context.Context, group, name string, j config.Job) error
	DeleteJob(ctx context.Context, group, name string) error
	Hooks(ctx context.Context, group string) ([]node.Hook, error)
	AddHook(ctx context.Context, group, name string, hk config.Hook) error
	EditHook(ctx context.Context, group, name string, hk config.Hook) error
	DeleteHook(ctx context.Context, group, name string) error
	Schedules(ctx context.Context, group string) ([]node.Schedule, error)
	AddSchedule(ctx context.Context, group, name string, s config.Schedule) error
	EditSchedule(ctx context.Context, group, name string, s config.Schedule) error
	DeleteSchedule(ctx context.Context, group, name string) error
	GroupConfigTOML(ctx context.Context, group string) (string, error)
	SetGroupConfigTOML(ctx context.Context, group, text string) error
	RunJob(ctx context.Context, group, name string, ev *trigger.Event) (node.JobResult, error)
}

// Fields of a Trigger form a refusal may name, besides FieldName.
const (
	FieldName    = "name"
	FieldAgent   = "agent"
	FieldExec    = "exec"
	FieldWebhook = "webhook"
	FieldTimeout = "timeout"
)

// TriggersView is the Triggers page.
type TriggersView struct {
	// Group is the edit-target Group, and Served whether it is served
	// now: Run now needs it to be.
	Group  string
	Served bool

	Jobs      []JobView
	Hooks     []HookView
	Schedules []ScheduleView
	// ListProblem is why config.toml could not be read.
	ListProblem *Problem
	// Handles are this device's agents, for an agent Job to summon.
	Handles []string
	// Events are what a Hook may fire on.
	Events []string

	// Problem is why the last change was refused, Where the form it is
	// about ("job:<name>", "new-job", "hook:<name>", "new-hook",
	// "schedule:<name>", "new-schedule" or "run:<name>") and Field the
	// field. Table is the table to find in "Edit TOML" when the Problem
	// is "not-in-place", as "jobs.<name>".
	Problem *Problem
	Where   string
	Field   string
	Table   string
	// Saved: the last change was written.
	Saved bool
	// Ran is how the Job Run now ran ended.
	Ran *node.JobResult
}

// JobView is one Job. Its webhook URL is WebhookSecret's when it names a
// Secret, else Webhook.
type JobView struct {
	Name, Kind            string
	Agent, Prompt, Target string
	Exec, Secrets         []string
	Webhook               string
	WebhookSecret         string
	Headers               []Header
	// Timeout is "" for the kind's default.
	Timeout string
	// UsedBy are the Triggers that run it, as "Hook <name>" or
	// "Schedule <name>".
	UsedBy []string
}

// Header is a webhook Job's header: its Value literal, or the Secret of
// that name.
type Header struct {
	Name, Value, Secret string
}

// HookView is one Hook.
type HookView struct {
	Name                                string
	Events, From, Handles, Conversation []string
	Text                                string
	Jobs                                []string
}

// ScheduleView is one Schedule and its next firing; nil when it fires no
// more.
type ScheduleView struct {
	Name, Cron, TZ, At string
	Jobs               []string
	Next               *time.Time
}

// JobDraft is a Job's form as filled in: New adds one by Name, else Name
// names the one to edit. Kind is "agent", "exec" or "webhook"; only that
// kind's fields are read.
type JobDraft struct {
	New                   bool
	Name, Kind            string
	Agent, Prompt, Target string
	Exec, Secrets         []string
	Webhook               string
	WebhookSecret         string
	Headers               []Header
	Timeout               string
}

// HookDraft is a Hook's form as filled in.
type HookDraft struct {
	New                                 bool
	Name                                string
	Events, From, Handles, Conversation []string
	Text                                string
	Jobs                                []string
}

// ScheduleDraft is a Schedule's form as filled in: Cron (with TZ) or At.
type ScheduleDraft struct {
	New                bool
	Name, Cron, TZ, At string
	Jobs               []string
}

// TOMLView is "Edit TOML": the edit-target Group's config.toml as text.
type TOMLView struct {
	Group   string
	Text    string
	Problem *Problem
	Saved   bool
}

// Triggers is the Triggers page.
func (s *Shell) Triggers() TriggersView {
	v, ok := s.triggersTarget()
	if ok {
		s.fillTriggers(s.node(), &v)
	}
	return v
}

// SaveJob adds or edits the Job d is.
func (s *Shell) SaveJob(d JobDraft) TriggersView {
	return s.triggerChange("job", d.New, d.Name, func(n Node, group string) error {
		j, err := jobOf(d)
		if err != nil {
			return err
		}
		if d.New {
			return n.AddJob(s.ctx, group, d.Name, j)
		}
		return n.EditJob(s.ctx, group, d.Name, j)
	})
}

// DeleteJob deletes Job name; refused, naming them, while Triggers run it.
func (s *Shell) DeleteJob(name string) TriggersView {
	return s.triggerChange("job", false, name, func(n Node, group string) error { return n.DeleteJob(s.ctx, group, name) })
}

// SaveHook adds or edits the Hook d is.
func (s *Shell) SaveHook(d HookDraft) TriggersView {
	hk := config.Hook{Events: d.Events, From: d.From, Handles: d.Handles, Conversation: d.Conversation, Text: d.Text, Jobs: d.Jobs}
	return s.triggerChange("hook", d.New, d.Name, func(n Node, group string) error {
		if d.New {
			return n.AddHook(s.ctx, group, d.Name, hk)
		}
		return n.EditHook(s.ctx, group, d.Name, hk)
	})
}

// DeleteHook deletes Hook name.
func (s *Shell) DeleteHook(name string) TriggersView {
	return s.triggerChange("hook", false, name, func(n Node, group string) error { return n.DeleteHook(s.ctx, group, name) })
}

// SaveSchedule adds or edits the Schedule d is.
func (s *Shell) SaveSchedule(d ScheduleDraft) TriggersView {
	sc := config.Schedule{Cron: strings.TrimSpace(d.Cron), TZ: strings.TrimSpace(d.TZ), At: strings.TrimSpace(d.At), Jobs: d.Jobs}
	return s.triggerChange("schedule", d.New, d.Name, func(n Node, group string) error {
		if d.New {
			return n.AddSchedule(s.ctx, group, d.Name, sc)
		}
		return n.EditSchedule(s.ctx, group, d.Name, sc)
	})
}

// DeleteSchedule deletes Schedule name.
func (s *Shell) DeleteSchedule(name string) TriggersView {
	return s.triggerChange("schedule", false, name, func(n Node, group string) error { return n.DeleteSchedule(s.ctx, group, name) })
}

// RunJob runs Job name of the edit-target Group now and waits for it to
// end; the Group must be the one served. Its outcome also shows in Talk.
// It holds no lock while the Job runs, which may take long.
func (s *Shell) RunJob(name string) TriggersView {
	v, ok := s.triggersTarget()
	if !ok {
		return v
	}
	n := s.node()
	v.Where = "run:" + name
	if !v.Served {
		v.Problem = problemOf(node.ErrNotServed)
	} else if res, err := n.RunJob(s.ctx, v.Group, name, nil); err != nil {
		v.Problem = problemOf(err)
	} else {
		v.Ran = &res
	}
	s.fillTriggers(n, &v)
	return v
}

// TriggersTOML is the edit-target Group's config.toml as text. Reading
// it needs no s.op, as Triggers does not.
func (s *Shell) TriggersTOML() TOMLView {
	v, ok := s.triggersTarget()
	t := TOMLView{Group: v.Group}
	if !ok {
		return t
	}
	text, err := s.node().GroupConfigTOML(s.ctx, t.Group)
	t.Text, t.Problem = text, problemOrNil(err)
	return t
}

// SaveTriggersTOML replaces the edit-target Group's config.toml with
// text, once it loads; text that does not writes nothing.
func (s *Shell) SaveTriggersTOML(text string) TOMLView {
	s.op.Lock()
	defer s.op.Unlock()
	v, ok := s.triggersTarget()
	t := TOMLView{Group: v.Group, Text: text}
	if !ok {
		return t
	}
	if err := s.node().SetGroupConfigTOML(s.ctx, t.Group, text); err != nil {
		t.Problem = problemOf(err)
	} else {
		t.Saved = true
	}
	return t
}

// triggerChange makes change to the edit-target Group's kind ("job",
// "hook" or "schedule") name, from its form, or the new one's if isNew.
func (s *Shell) triggerChange(kind string, isNew bool, name string, change func(Node, string) error) TriggersView {
	s.op.Lock()
	defer s.op.Unlock()
	v, ok := s.triggersTarget()
	if !ok {
		return v
	}
	n := s.node()
	v.Where = kind + ":" + name
	if isNew {
		v.Where = "new-" + kind
	}
	if err := change(n, v.Group); err != nil {
		v.Problem, v.Field = problemOf(err), triggerField(err)
		if errors.Is(err, config.ErrNotInPlace) {
			v.Table = kind + "s." + name
		}
	} else {
		v.Saved = true
	}
	s.fillTriggers(n, &v)
	return v
}

// triggersTarget is the view's Group and whether it is served; ok false
// while no Group is served in this app.
func (s *Shell) triggersTarget() (TriggersView, bool) {
	st := s.State()
	v := TriggersView{Group: st.EditGroup, Served: st.EditGroupServed}
	return v, s.node() != nil && st.View == ViewServed && v.Group != ""
}

// fillTriggers adds the Jobs, Hooks and Schedules, and what the forms
// offer.
func (s *Shell) fillTriggers(n Node, v *TriggersView) {
	v.Events = config.Events()
	v.Handles = []string{}
	if list, err := n.AgentProfiles(s.ctx); err == nil {
		for _, p := range list {
			v.Handles = append(v.Handles, p.Handle)
		}
	}
	jobs, err := n.Jobs(s.ctx, v.Group)
	if err != nil {
		v.ListProblem = problemOf(err)
		return
	}
	hooks, err := n.Hooks(s.ctx, v.Group)
	if err != nil {
		v.ListProblem = problemOf(err)
		return
	}
	scheds, err := n.Schedules(s.ctx, v.Group)
	if err != nil {
		v.ListProblem = problemOf(err)
		return
	}
	usedBy := map[string][]string{}
	v.Hooks = make([]HookView, len(hooks))
	for i, hk := range hooks {
		v.Hooks[i] = HookView{Name: hk.Name, Events: orEmpty(hk.Events), From: orEmpty(hk.From), Handles: orEmpty(hk.Handles),
			Conversation: orEmpty(hk.Conversation), Text: hk.Text, Jobs: orEmpty(hk.Jobs)}
		for _, j := range hk.Jobs {
			usedBy[j] = append(usedBy[j], "Hook "+hk.Name)
		}
	}
	v.Schedules = make([]ScheduleView, len(scheds))
	for i, sc := range scheds {
		v.Schedules[i] = ScheduleView{Name: sc.Name, Cron: sc.Cron, TZ: sc.TZ, At: sc.At, Jobs: orEmpty(sc.Jobs)}
		if !sc.Next.IsZero() {
			next := sc.Next
			v.Schedules[i].Next = &next
		}
		for _, j := range sc.Jobs {
			usedBy[j] = append(usedBy[j], "Schedule "+sc.Name)
		}
	}
	v.Jobs = make([]JobView, len(jobs))
	for i, j := range jobs {
		jv := JobView{Name: j.Name, Kind: j.Kind(), Agent: j.Agent, Prompt: j.Prompt, Target: j.Target,
			Exec: orEmpty(j.Exec), Secrets: orEmpty(j.Secrets), Headers: []Header{}, UsedBy: orEmpty(usedBy[j.Name])}
		if name, ok := secretName(j.Webhook); ok {
			jv.WebhookSecret = name
		} else {
			jv.Webhook = j.Webhook
		}
		for _, k := range slices.Sorted(maps.Keys(j.Headers)) {
			h := Header{Name: k}
			if name, ok := secretName(j.Headers[k]); ok {
				h.Secret = name
			} else {
				h.Value = j.Headers[k]
			}
			jv.Headers = append(jv.Headers, h)
		}
		if j.Timeout != 0 {
			jv.Timeout = j.Timeout.String()
		}
		v.Jobs[i] = jv
	}
}

// jobOf is the Job d describes; a field missing for its kind is refused
// naming it.
func jobOf(d JobDraft) (config.Job, error) {
	var j config.Job
	refuse := func(field, msg string) error { return &node.InvalidError{Err: errors.New(msg), Field: field} }
	switch d.Kind {
	case config.JobAgent:
		if j.Agent = strings.TrimPrefix(strings.TrimSpace(d.Agent), "@"); j.Agent == "" {
			return j, refuse(FieldAgent, "choose the agent it summons")
		}
		j.Prompt, j.Target = strings.TrimSpace(d.Prompt), strings.TrimSpace(d.Target)
		return j, nil
	case config.JobExec:
		for _, a := range d.Exec {
			if a != "" || len(j.Exec) > 0 {
				j.Exec = append(j.Exec, a)
			}
		}
		if len(j.Exec) == 0 {
			return j, refuse(FieldExec, "give the program to run")
		}
		if len(d.Secrets) > 0 {
			j.Secrets = d.Secrets
		}
	case config.JobWebhook:
		switch {
		case d.WebhookSecret != "":
			j.Webhook = "$" + d.WebhookSecret
		case strings.TrimSpace(d.Webhook) != "":
			j.Webhook = strings.TrimSpace(d.Webhook)
		default:
			return j, refuse(FieldWebhook, "give the URL it calls, or the Secret that holds it")
		}
		for _, h := range d.Headers {
			name := strings.TrimSpace(h.Name)
			if name == "" {
				continue
			}
			if j.Headers == nil {
				j.Headers = map[string]string{}
			}
			if h.Secret != "" {
				j.Headers[name] = "$" + h.Secret
			} else {
				j.Headers[name] = h.Value
			}
		}
	default:
		return j, refuse("kind", "choose agent, exec or webhook")
	}
	if t := strings.TrimSpace(d.Timeout); t != "" {
		age, err := config.ParseAge(t)
		if err != nil {
			return j, refuse(FieldTimeout, "a timeout is a duration such as 30s or 2m")
		}
		j.Timeout = age
	}
	return j, nil
}

// secretName is the name of the Secret ref names, for a plain $NAME.
func secretName(ref string) (string, bool) {
	name, ok := strings.CutPrefix(ref, "$")
	return name, ok && name != ""
}

func orEmpty[T ~[]string](list T) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// triggerField is the field a Trigger form's refusal names, "" for none.
func triggerField(err error) string {
	if errors.Is(err, node.ErrAlreadyDefined) {
		return FieldName
	}
	return fieldOf(err)
}

// triggersProblem maps the Triggers page's errors; nil for another.
func triggersProblem(err error) *Problem {
	var inUse *node.JobInUseError
	switch {
	case errors.As(err, &inUse):
		by := inUse.Triggers()
		verb := "run"
		if len(by) == 1 {
			verb = "runs"
		}
		return &Problem{"job-in-use", fmt.Sprintf("Job %s can't be deleted while %s %s it. Take it out of them first.",
			inUse.Job, strings.Join(by, " and "), verb)}
	case errors.Is(err, config.ErrNotInPlace):
		return &Problem{"not-in-place", "config.toml writes this in a form the form can't change, such as an inline table. Change it with Edit TOML instead."}
	case errors.Is(err, node.ErrAlreadyDefined):
		return &Problem{"already-defined", "This Group already has one by that name."}
	case errors.Is(err, node.ErrNotDefined):
		return &Problem{"not-defined", "This Group no longer has one by that name; it may have been changed in Edit TOML."}
	case errors.Is(err, node.ErrNoJob):
		return &Problem{"no-job", "The Group as served has no such Job yet."}
	}
	return nil
}
