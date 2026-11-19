package serve

import (
	"context"
	"encoding/hex"
	"net/http"
	"slices"
	"sync"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/jobrun"
	"defios.com/internal/trigger"
)

// States of a JobRun.
const (
	JobPending = jobrun.Pending
	JobRunning = jobrun.Running
	JobOK      = jobrun.OK
	JobFailed  = jobrun.Failed
	JobNotRun  = jobrun.NotRun // a Job before it failed, or serve stopped first
)

// JobRun is one Job Run (ADR 0040): one Job a Trigger's firing ran, or
// was to run, and how far it got.
type JobRun = jobrun.Run

// HookRun is one firing of one of this Node's Hooks on a Sign Request:
// its Job Runs, in the order they run. ID is its first Job Run's.
type HookRun struct {
	ID      uint64        `json:"id"`
	Trigger string        `json:"trigger"`
	Event   trigger.Event `json:"event"` // what it fired on; SignRequestID names the request
	At      time.Time     `json:"at"`
	Jobs    []JobRun      `json:"jobs"`
}

// jobRuns keeps the Job Runs of this Node's Triggers' firings in the
// Group's job-runs/. Its zero value keeps none until open.
type jobRuns struct {
	mu    sync.Mutex
	dir   string
	store *jobrun.Store // nil until open, or if it failed to
	// live are the Job Runs of firings under way, by ID, until they end:
	// a message recorded on one must reach the firing's copy, which it
	// keeps whole each time it changes.
	live map[uint64]liveRun
	logf func(string, ...any)
	// onChange, if set, hears a Hook's firing on a Sign Request each time
	// it changes, under mu: it must not block or call back.
	onChange func(HookRun)
}

// open starts keeping Job Runs in dir, once: a failure is logged, and
// nothing is kept.
func (r *jobRuns) open(dir string, logf func(string, ...any)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.store != nil {
		return
	}
	r.dir, r.logf = dir, logf
	s, err := jobrun.Open(dir)
	if err != nil {
		logf("serve: job runs: %v", err)
		return
	}
	r.store = s
}

// start records Trigger name firing on ev with jobs as defs define them,
// all pending, and returns the firing to move along as they run.
func (r *jobRuns) start(name string, jobs []string, defs map[string]config.Job, ev trigger.Event) *firing {
	f := &firing{r: r, onSignRequest: ev.Type != trigger.ScheduleEvent && ev.SignRequestID != ""}
	now := time.Now()
	for _, job := range jobs {
		j := defs[job]
		run := JobRun{Trigger: name, Event: ev, Fired: now, Job: job, Kind: j.Kind(), State: JobPending}
		if run.Kind == config.JobAgent {
			run.Handle = j.Agent
		}
		f.runs = append(f.runs, run)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.store != nil {
		err := r.store.Add(f.runs)
		r.logErr(err)
		if err == nil {
			if r.live == nil {
				r.live = map[uint64]liveRun{}
			}
			for i, run := range f.runs {
				r.live[run.ID] = liveRun{f, i}
			}
		}
	}
	f.tell()
	return f
}

// liveRun is Job Run i of firing f.
type liveRun struct {
	f *firing
	i int
}

// message records msgID, a talk message written with token, on the Job
// Run token names. A token naming no Job Run kept is passed over: the
// message is written all the same.
func (r *jobRuns) message(token, msgID string) {
	id, ok := jobrun.TokenID(token)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.store == nil {
		return
	}
	l, live := r.live[id]
	var run *JobRun
	if live {
		run = &l.f.runs[l.i] // the firing's own copy, which it keeps whole
	} else {
		kept, err := jobrun.Get(r.dir, id)
		if err != nil {
			return
		}
		run = &kept
	}
	if run.Token() != token {
		return
	}
	run.Messages = append(run.Messages, msgID)
	r.logErr(r.store.Put(*run))
	if live {
		l.f.tell()
	}
}

// logErr logs a Job Run that could not be kept; r.mu is held.
func (r *jobRuns) logErr(err error) {
	if err != nil {
		r.logf("serve: job runs: %v", err)
	}
}

// hookRuns is the Hooks' firings on Sign Requests kept, oldest first.
func (r *jobRuns) hookRuns() ([]HookRun, error) {
	r.mu.Lock()
	dir := r.dir
	r.mu.Unlock()
	if dir == "" {
		return []HookRun{}, nil
	}
	runs, err := jobrun.List(dir)
	if err != nil {
		return nil, err
	}
	out := []HookRun{}
	at := map[uint64]int{}
	for _, run := range slices.Backward(runs) { // oldest first
		if run.Event.Type == trigger.ScheduleEvent || run.Event.SignRequestID == "" {
			continue
		}
		i, ok := at[run.Firing]
		if !ok {
			i = len(out)
			at[run.Firing] = i
			out = append(out, hookRun(nil))
		}
		out[i].Jobs = append(out[i].Jobs, run)
		out[i] = hookRun(out[i].Jobs)
	}
	return out, nil
}

// firing moves one firing's Job Runs along.
type firing struct {
	r             *jobRuns
	onSignRequest bool // a Hook's on a Sign Request: onChange hears it
	runs          []JobRun
}

// update changes Job Run i with change, and each after it with rest, if
// set, and keeps those that changed.
func (f *firing) update(i int, change, rest func(*JobRun)) {
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	for j := i; j < len(f.runs); j++ {
		c := change
		if j > i {
			if c = rest; c == nil {
				break
			}
		}
		before := f.runs[j]
		c(&f.runs[j])
		if f.r.store != nil && !jobRunEqual(before, f.runs[j]) {
			f.r.logErr(f.r.store.Put(f.runs[j]))
		}
		if s := f.runs[j].State; s != JobPending && s != JobRunning {
			delete(f.r.live, f.runs[j].ID) // ended: changed no more but by message
		}
	}
	f.tell()
}

// jobRunEqual reports whether a and b, the same Job Run, are unchanged in
// all an update changes.
func jobRunEqual(a, b JobRun) bool {
	return a.State == b.State && a.Error == b.Error && a.Started.Equal(b.Started) && a.Ended.Equal(b.Ended) &&
		a.Conversation == b.Conversation && a.Chain == b.Chain && a.Exec == b.Exec && a.Webhook == b.Webhook
}

// tell tells onChange the firing, if it is a Hook's on a Sign Request;
// r.mu is held.
func (f *firing) tell() {
	if !f.onSignRequest || f.r.onChange == nil || len(f.runs) == 0 {
		return
	}
	f.r.onChange(hookRun(slices.Clone(f.runs)))
}

// hookRun is the firing whose Job Runs are runs, in order; empty for none.
func hookRun(runs []JobRun) HookRun {
	if len(runs) == 0 {
		return HookRun{}
	}
	first := runs[0]
	return HookRun{ID: first.Firing, Trigger: first.Trigger, Event: first.Event, At: first.Fired, Jobs: runs}
}

func notRun(run *JobRun) { run.State = JobNotRun }

// token is the token naming Job i's Job Run, "" if none is kept: a
// message written with it could not be recorded.
func (f *firing) token(i int) string {
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	if l, ok := f.r.live[f.runs[i].ID]; !ok || l.f != f {
		return ""
	}
	return f.runs[i].Token()
}

// running marks Job i running.
func (f *firing) running(i int) {
	f.update(i, func(run *JobRun) { run.State, run.Started = JobRunning, time.Now() }, nil)
}

// chain records where Job i, an agent Job, answers.
func (f *firing) chain(i int, conv []byte, chain string) {
	f.update(i, func(run *JobRun) { run.Conversation, run.Chain = hex.EncodeToString(conv), chain }, nil)
}

// ended marks Job i ended as v says, with what it produced; a failure
// leaves the Jobs after it not run.
func (f *firing) ended(i int, v JobView, out trigger.Output) {
	var rest func(*JobRun)
	if !v.OK {
		rest = notRun
	}
	f.update(i, func(run *JobRun) {
		run.Ended, run.Output = time.Now(), out
		run.State, run.Error = JobOK, ""
		if !v.OK {
			run.State, run.Error = JobFailed, v.Error
		}
	}, rest)
}

// stopped marks the Jobs from i on not run: serve stopped first.
func (f *firing) stopped(i int) { f.update(i, notRun, notRun) }

func (s *server) hookRunRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/hooks/runs", func(w http.ResponseWriter, _ *http.Request) {
		runs, err := s.jobRuns.hookRuns()
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, runs)
	})
}

// HookRuns is the firings of this Node's Hooks on Sign Requests whose Job
// Runs the Group keeps, oldest first.
func (c Client) HookRuns(ctx context.Context) ([]HookRun, error) {
	var v []HookRun
	err := c.do(ctx, "/v1/hooks/runs", nil, &v)
	return v, err
}

// HookRuns is Client.HookRuns in this process.
func (r *Running) HookRuns(context.Context) ([]HookRun, error) { return r.s.jobRuns.hookRuns() }
