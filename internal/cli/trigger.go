package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"defios.com/internal/config"
	"defios.com/internal/node"
	"defios.com/internal/serve"
	"defios.com/internal/trigger"
)

// Triggers and Jobs are defined in the Group's config.toml only (ADR
// 0032). These verbs list and run them; the library writes them, but no
// verb here does yet.

// triggerTarget is the Node and Group alias a trigger verb acts on.
func triggerTarget(g *globals) (*node.Host, string, error) {
	h, err := openNode(g)
	if err != nil {
		return nil, "", err
	}
	alias, err := g.resolveAlias()
	if err != nil {
		return nil, "", err
	}
	return h, alias, nil
}

func hookCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "hook",
		Short: "This Node's Hooks: Triggers that run Jobs on live events (defined in the Group's config.toml)",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List this Node's Hooks: what each fires on and the Jobs it runs",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runHookList(cmd.Context(), g) },
	}
	bindGroupFlag(g, list)
	root.AddCommand(list)
	return root
}

func scheduleCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "schedule",
		Short: "This Node's Schedules: Triggers that run Jobs at set times (defined in the Group's config.toml)",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List this Node's Schedules: when each fires, its next firing, and the Jobs it runs",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runScheduleList(cmd.Context(), g) },
	}
	bindGroupFlag(g, list)
	root.AddCommand(list)
	return root
}

func jobCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "job",
		Short: "This Node's Jobs: what its Triggers run (defined in the Group's config.toml)",
	}
	var eventFile string
	run := &cobra.Command{
		Use:   "run <name>",
		Short: "Run a Job now, as no Trigger, on the event in --event or none, and wait for it to end (requires serve); its writes are one deeper than the event, 1 with none",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runJobRun(cmd, g, args[0], eventFile) },
	}
	run.Flags().StringVar(&eventFile, "event", "", "a JSON file holding the event, as a Job is given it on stdin")
	list := &cobra.Command{
		Use:   "list",
		Short: "List this Node's Jobs: each one's kind and what it runs",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runJobList(cmd.Context(), g) },
	}
	bindGroupFlag(g, list)
	root.AddCommand(list, run)
	return root
}

// jobView is one Job, as config.toml defines it. A webhook's URL is shown
// only when it is a Secret reference: a literal one may carry a token.
type jobView struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Agent   string   `json:"agent,omitempty"`
	Prompt  string   `json:"prompt,omitempty"`
	Target  string   `json:"target,omitempty"`
	Exec    []string `json:"exec,omitempty"`
	Secrets []string `json:"secrets,omitempty"`
	Webhook string   `json:"webhook,omitempty"`
	Timeout string   `json:"timeout,omitempty"`
}

func runJobList(ctx context.Context, g *globals) error {
	h, alias, err := triggerTarget(g)
	if err != nil {
		return err
	}
	jobs, err := h.Jobs(ctx, alias)
	if err != nil {
		return exitOperational(err)
	}
	views := make([]jobView, 0, len(jobs))
	for _, j := range jobs {
		v := jobView{Name: j.Name, Kind: j.Kind(), Agent: j.Agent, Prompt: j.Prompt, Target: j.Target, Exec: j.Exec, Secrets: j.Secrets}
		if strings.HasPrefix(j.Webhook, "$") {
			v.Webhook = j.Webhook
		}
		if j.Timeout != 0 {
			v.Timeout = j.Timeout.String()
		}
		views = append(views, v)
	}
	if g.json {
		return g.emitJSON(views)
	}
	p := g.out()
	for _, v := range views {
		parts := []string{p.primary(v.Name), v.Kind}
		switch v.Kind {
		case config.JobAgent:
			parts[1] += " @" + v.Agent
			if v.Target != "" {
				parts = append(parts, "in "+v.Target)
			}
		case config.JobExec:
			args := make([]string, len(v.Exec))
			for i, a := range v.Exec {
				args[i] = a
				if a == "" || strings.ContainsAny(a, " \t\n\"'") {
					args[i] = strconv.Quote(a)
				}
			}
			parts[1] += " " + strings.Join(args, " ")
			if len(v.Secrets) > 0 {
				parts = append(parts, "secrets "+strings.Join(v.Secrets, ", "))
			}
		case config.JobWebhook:
			if v.Webhook != "" {
				parts[1] += " " + v.Webhook
			}
		}
		if v.Timeout != "" {
			parts = append(parts, "timeout "+v.Timeout)
		}
		fmt.Fprintln(g.app.Stdout, strings.Join(parts, "  "))
	}
	return nil
}

// hookView is one Hook, as config.toml defines it.
type hookView struct {
	Name         string   `json:"name"`
	Events       []string `json:"events,omitempty"`
	From         []string `json:"from,omitempty"`
	Handles      []string `json:"handles,omitempty"`
	Conversation []string `json:"conversation,omitempty"`
	Text         string   `json:"text,omitempty"`
	Jobs         []string `json:"jobs"`
}

func runHookList(ctx context.Context, g *globals) error {
	h, alias, err := triggerTarget(g)
	if err != nil {
		return err
	}
	hooks, err := h.Hooks(ctx, alias)
	if err != nil {
		return exitOperational(err)
	}
	views := make([]hookView, 0, len(hooks))
	for _, h := range hooks {
		views = append(views, hookView{
			Name: h.Name, Events: h.Events, From: h.From, Handles: h.Handles,
			Conversation: h.Conversation, Text: h.Text, Jobs: h.Jobs,
		})
	}
	if g.json {
		return g.emitJSON(views)
	}
	p := g.out()
	for _, v := range views {
		parts := []string{p.primary(v.Name), "on every event"}
		if len(v.Events) > 0 {
			parts[1] = "on " + strings.Join(v.Events, ", ")
		}
		for _, f := range []struct {
			key  string
			vals []string
		}{{"from", v.From}, {"handles", v.Handles}, {"conversation", v.Conversation}} {
			if len(f.vals) > 0 {
				parts = append(parts, f.key+" "+strings.Join(f.vals, ", "))
			}
		}
		if v.Text != "" {
			parts = append(parts, "text "+v.Text)
		}
		parts = append(parts, "jobs "+strings.Join(v.Jobs, ", "))
		fmt.Fprintln(g.app.Stdout, strings.Join(parts, "  "))
	}
	return nil
}

// scheduleView is one Schedule, as config.toml defines it, and its next
// firing, in its zone, if it fires again.
type scheduleView struct {
	Name string   `json:"name"`
	Cron string   `json:"cron,omitempty"`
	TZ   string   `json:"tz,omitempty"`
	At   string   `json:"at,omitempty"`
	Next string   `json:"next,omitempty"`
	Jobs []string `json:"jobs"`
}

func runScheduleList(ctx context.Context, g *globals) error {
	h, alias, err := triggerTarget(g)
	if err != nil {
		return err
	}
	schedules, err := h.Schedules(ctx, alias)
	if err != nil {
		return exitOperational(err)
	}
	views := make([]scheduleView, 0, len(schedules))
	for _, s := range schedules {
		v := scheduleView{Name: s.Name, Cron: s.Cron, TZ: s.TZ, At: s.At, Jobs: s.Jobs}
		if !s.Next.IsZero() {
			v.Next = s.Next.Format(time.RFC3339)
		}
		views = append(views, v)
	}
	if g.json {
		return g.emitJSON(views)
	}
	p := g.out()
	for _, v := range views {
		when := "at " + v.At
		if v.Cron != "" {
			when = fmt.Sprintf("cron %q", v.Cron)
			if v.TZ != "" {
				when += " " + v.TZ
			}
		}
		next := v.Next
		if next == "" {
			next = "none"
		}
		fmt.Fprintf(g.app.Stdout, "%s  %s  next %s  jobs %s\n", p.primary(v.Name), when, p.dim(next), strings.Join(v.Jobs, ", "))
	}
	return nil
}

// runJobRun runs Job name through serve on the event in eventFile, if
// given, and exits as the Job ended.
func runJobRun(cmd *cobra.Command, g *globals, name, eventFile string) error {
	var ev *trigger.Event
	if eventFile != "" {
		b, err := os.ReadFile(eventFile)
		if err != nil {
			return exitUsagef("--event: %w", err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		ev = new(trigger.Event)
		if err := dec.Decode(ev); err != nil {
			return exitUsagef("--event %s: not an event: %w", eventFile, err)
		}
	}
	h, err := openNode(g)
	if err != nil {
		return err
	}
	v, err := h.RunJob(cmd.Context(), node.ServedGroup, name, ev)
	if errors.Is(err, node.ErrNoJob) {
		return exitUsagef("%w", err)
	}
	if err != nil {
		return serveError(err)
	}
	if g.json {
		if err := g.emitJSON(v); err != nil {
			return err
		}
	} else if v.OK {
		fmt.Fprintf(g.app.Stdout, "job %s done\n", name)
	}
	if !v.OK {
		return exitOperationalf("job %s failed: %s", name, v.Error)
	}
	return nil
}

// triggerLine is how follow shows a trigger.fire or job.end event.
func triggerLine(p palette, ev serve.ChainEvent) string {
	switch ev.Type {
	case serve.EventTriggerFire:
		if ev.Event != nil && ev.Event.Type == trigger.ScheduleEvent {
			return p.dim(fmt.Sprintf("[schedule %s fired for %s]", ev.Trigger, ev.Event.Time))
		}
		on := ""
		if ev.Event != nil {
			on = " on " + ev.Event.Type
			if ev.Event.Seq != 0 {
				on += fmt.Sprintf(" at seq %d", ev.Event.Seq)
			}
		}
		return p.dim(fmt.Sprintf("[hook %s fired%s]", ev.Trigger, on))
	default: // job.end
		if ev.OK != nil && *ev.OK {
			return p.dim(fmt.Sprintf("[job %s done]", ev.Job))
		}
		return fmt.Sprintf("[job %s %s: %s]", ev.Job, p.status("failed"), ev.Error)
	}
}
