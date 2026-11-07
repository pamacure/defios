package serve

import (
	"context"
	"slices"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/trigger"
)

// scheduleSlack is how late a check may come after a Schedule's firing
// for it still to run: a tick wakes a moment late on a busy machine. One
// waking from sleep checks later still, by when the firings it slept
// through are missed, never made up.
const scheduleSlack = 10 * time.Second

// scheduleTick is how often a Schedule loop checks what is due, and
// rereads config.toml: a firing runs within it of its time.
const scheduleTick = time.Second

// keepSchedules fires this Node's Schedules (ADR 0032) while serve serves
// its Group, until ctx ends: each at a time that passes from now on, by
// the wall clock. A firing that fell before, or while serve was not
// serving, is skipped, as is one that falls while the Schedule's last
// firing still runs. No state is kept. config.toml's Jobs and Schedules
// are reread whenever the file changes, as for Hooks.
func (s *server) keepSchedules(ctx context.Context, store *cosl.Store) {
	l := &scheduleLoop{
		triggers: s.newTriggers(ctx, store, "schedule"),
		last:     time.Now().Round(0), // the wall clock alone: a monotonic reading stops while the machine sleeps
	}
	defer l.queues.wait()
	tick := time.NewTicker(scheduleTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		l.reread()
		l.check(time.Now().Round(0))
	}
}

// scheduleLoop is keepSchedules' state between checks.
type scheduleLoop struct {
	*triggers
	// last is the latest time checked. A wall clock set back does not
	// move it, so the times it passes again do not fire twice.
	last time.Time
}

// check queues a firing of each Schedule due in (last, now].
func (l *scheduleLoop) check(now time.Time) {
	names := make([]string, 0, len(l.cfg.Schedules))
	for name := range l.cfg.Schedules {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		at, ok := trigger.Due(l.cfg.Schedules[name], l.last, now, scheduleSlack)
		if !ok {
			continue
		}
		ev := trigger.Event{
			Type: trigger.ScheduleEvent, Trigger: name, Group: l.s.opts.Alias,
			From: l.s.opts.Node.NodeID.String(), Time: at.Format(time.RFC3339),
		}
		if !l.queues.idle(name) {
			l.s.opts.Logf("serve: schedule %s: skipped its firing for %s: the last still runs", name, ev.Time)
			continue
		}
		l.queue(name, "for "+ev.Time, l.cfg.Schedules[name].Jobs, l.cfg.Jobs, ev)
	}
	if now.After(l.last) {
		l.last = now
	}
}
