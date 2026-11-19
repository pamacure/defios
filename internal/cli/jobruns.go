package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"defios.com/internal/config"
	"defios.com/internal/node"
	"defios.com/internal/trigger"
)

// Job Runs are kept by the serve that ran them, and read from disk here,
// served or not (ADR 0040).

func triggerCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "trigger",
		Short: "What this Node's Hooks and Schedules ran: their Job Runs",
	}
	var job string
	var last int
	runs := &cobra.Command{
		Use:   "runs",
		Short: "List the Job Runs this Node keeps for the Group, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if last < 0 {
				return exitUsagef("--last must be at least 0")
			}
			return runTriggerRuns(cmd.Context(), g, job, last)
		},
	}
	runs.Flags().StringVar(&job, "job", "", "only this Job's runs")
	runs.Flags().IntVar(&last, "last", 0, "only the last N runs (0 for all)")
	show := &cobra.Command{
		Use:   "run <id>",
		Short: "Show one Job Run in full: what fired it, what its Job produced, and the msg_ids of the talk messages an exec Job wrote",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runTriggerRun(cmd.Context(), g, args[0]) },
	}
	bindGroupFlag(g, runs)
	bindGroupFlag(g, show)
	root.AddCommand(runs, show)
	return root
}

func runTriggerRuns(ctx context.Context, g *globals, job string, last int) error {
	h, alias, err := triggerTarget(g)
	if err != nil {
		return err
	}
	runs, err := h.JobRuns(ctx, alias, job, last)
	if err != nil {
		return exitOperational(err)
	}
	if g.json {
		return g.emitJSON(runs)
	}
	p := g.out()
	for _, r := range runs {
		what := r.Job + " " + r.Kind
		if r.Handle != "" {
			what += " @" + r.Handle
		}
		fmt.Fprintf(g.app.Stdout, "%s  %s  %s  %s  %s\n", p.primary(strconv.FormatUint(r.ID, 10)), p.dim(utc(r.Fired)), firedBy(r), what, jobState(p, r))
	}
	return nil
}

func runTriggerRun(ctx context.Context, g *globals, arg string) error {
	id, err := strconv.ParseUint(arg, 10, 64)
	if err != nil {
		return exitUsagef("%q is not a Job Run ID: give the number trigger runs lists", arg)
	}
	h, alias, err := triggerTarget(g)
	if err != nil {
		return err
	}
	r, err := h.JobRun(ctx, alias, id)
	if errors.Is(err, node.ErrNoJobRun) {
		return exitUsagef("%w; it may have been dropped: a Group keeps its last 200", err)
	}
	if err != nil {
		return exitOperational(err)
	}
	if g.json {
		return g.emitJSON(r)
	}
	p := g.out()
	var b strings.Builder
	field := func(name, value string) { fmt.Fprintf(&b, "%-10s %s\n", name, value) }
	block := func(name, text string) {
		if text == "" {
			return
		}
		b.WriteString(name + "\n")
		for _, l := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
			b.WriteString("  " + l + "\n")
		}
	}
	fmt.Fprintf(&b, "%s\n", p.primary(fmt.Sprintf("Job Run %d", r.ID)))
	field("trigger", firedBy(r)+firedOn(r.Event))
	field("job", fmt.Sprintf("%s (%s)", r.Job, r.Kind))
	field("state", jobState(p, r))
	field("fired", p.dim(utc(r.Fired)))
	if !r.Started.IsZero() {
		field("started", p.dim(utc(r.Started)))
	}
	if !r.Ended.IsZero() {
		field("ended", p.dim(utc(r.Ended)))
	}
	if r.Kind == config.JobAgent {
		where := "Group-level talk"
		if r.Conversation != "" {
			where = "Conversation " + r.Conversation
		}
		field("agent", "@"+r.Handle+" in "+where)
		if r.Chain != "" {
			field("chain", r.Chain)
		}
	}
	if x := r.Exec; x != nil {
		field("command", shellWords(x.Command))
		if x.ExitCode != nil {
			field("exit code", strconv.Itoa(*x.ExitCode))
		}
		block("stdout", x.Stdout)
		block("stderr", x.Stderr)
	}
	if w := r.Webhook; w != nil {
		field("request", w.Method+" "+w.URL)
		attempts := make([]string, len(w.Attempts))
		for i, a := range w.Attempts {
			attempts[i] = "none"
			if a.Status != 0 {
				attempts[i] = strconv.Itoa(a.Status)
			}
			if a.Error != "" {
				attempts[i] += ": " + a.Error
			}
		}
		field("attempts", strings.Join(attempts, ", "))
		label := "response"
		if w.ContentType != "" {
			label = fmt.Sprintf("%-10s %s", label, w.ContentType)
		}
		block(label, w.Body)
	}
	block("messages", strings.Join(r.Messages, "\n"))
	fmt.Fprint(g.app.Stdout, b.String())
	return nil
}

// firedBy names the Trigger that ran r: "hook <name>" or "schedule <name>".
func firedBy(r node.JobRun) string {
	if r.Event.Type == trigger.ScheduleEvent {
		return "schedule " + r.Trigger
	}
	return "hook " + r.Trigger
}

// firedOn says what a Trigger fired on: a Schedule's time, a Hook's event.
func firedOn(ev trigger.Event) string {
	if ev.Type == trigger.ScheduleEvent {
		return " for " + ev.Time
	}
	on := " on " + ev.Type
	if ev.Seq != 0 {
		on += fmt.Sprintf(" at seq %d", ev.Seq)
	}
	return on
}

// jobState is r's state, with its error if it failed.
func jobState(p palette, r node.JobRun) string {
	if r.Error != "" {
		return p.status(r.State) + ": " + r.Error
	}
	return p.status(r.State)
}

// utc is t as RFC 3339 in UTC, "" if zero.
func utc(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// shellWords joins args, quoting any that a shell would split.
func shellWords(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = a
		if a == "" || strings.ContainsAny(a, " \t\n\"'") {
			out[i] = strconv.Quote(a)
		}
	}
	return strings.Join(out, " ")
}
