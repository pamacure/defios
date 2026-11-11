package serve

import (
	"context"
	"encoding/hex"
	"net/http"
	"slices"
	"sync"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/trigger"
)

// States of a Job in a HookRun.
const (
	JobPending = "pending" // waits on the Jobs before it
	JobRunning = "running"
	JobOK      = "ok"
	JobFailed  = "failed"
	// JobNotRun: a Job before it failed, or serve stopped first, so it
	// never ran.
	JobNotRun = "not_run"
)

// maxHookRuns bounds the firings a serve remembers; the oldest go first.
const maxHookRuns = 512

// HookRun is one firing of one of this Node's Hooks on a Sign Request, as
// the serve that ran it saw it: a serve remembers those since it started.
type HookRun struct {
	ID      uint64        `json:"id"` // this serve's count of firings, from 1
	Trigger string        `json:"trigger"`
	Event   trigger.Event `json:"event"` // what it fired on; SignRequestID names the request
	At      time.Time     `json:"at"`
	Jobs    []JobRun      `json:"jobs"` // in the order they run
}

// JobRun is one Job of a HookRun and how far it got.
type JobRun struct {
	Job  string `json:"job"`
	Kind string `json:"kind"` // agent, exec or webhook
	// Handle is an agent Job's agent. Conversation and Chain are where it
	// answers, hex ("" for Group-level talk), and the chain it runs as,
	// once it started: a Transcript names the run by it.
	Handle       string `json:"handle,omitempty"`
	Conversation string `json:"conversation,omitempty"`
	Chain        string `json:"chain,omitempty"`
	State        string `json:"state"`
	Error        string `json:"error,omitempty"`
}

// hookRuns are the HookRuns this serve remembers. Its zero value is ready.
type hookRuns struct {
	mu    sync.Mutex
	runs  []*HookRun
	count uint64
	// onChange, if set, hears a copy of a run each time it changes, under
	// mu: it must not block or call back.
	onChange func(HookRun)
}

// start records Hook name firing on ev with jobs as defs define them, and
// returns the record to move along as they run.
func (h *hookRuns) start(name string, jobs []string, defs map[string]config.Job, ev trigger.Event) *hookRun {
	run := &HookRun{Trigger: name, Event: ev, At: time.Now()}
	for _, job := range jobs {
		j := defs[job]
		jr := JobRun{Job: job, Kind: j.Kind(), State: JobPending}
		if jr.Kind == config.JobAgent {
			jr.Handle = j.Agent
		}
		run.Jobs = append(run.Jobs, jr)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	run.ID = h.count
	h.runs = append(h.runs, run)
	if over := len(h.runs) - maxHookRuns; over > 0 {
		h.runs = slices.Delete(h.runs, 0, over)
	}
	h.tell(run)
	return &hookRun{h: h, run: run}
}

// all is a copy of every run remembered, oldest first.
func (h *hookRuns) all() []HookRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]HookRun, len(h.runs))
	for i, r := range h.runs {
		out[i] = r.clone()
	}
	return out
}

// tell tells onChange run. h.mu is held.
func (h *hookRuns) tell(run *HookRun) {
	if h.onChange != nil {
		h.onChange(run.clone())
	}
}

func (r *HookRun) clone() HookRun {
	c := *r
	c.Jobs = slices.Clone(r.Jobs)
	return c
}

// hookRun moves one HookRun along. A nil one records nothing.
type hookRun struct {
	h   *hookRuns
	run *HookRun
}

func (r *hookRun) update(f func(*HookRun)) {
	if r == nil {
		return
	}
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	f(r.run)
	r.h.tell(r.run)
}

// running marks Job i running.
func (r *hookRun) running(i int) {
	r.update(func(run *HookRun) { run.Jobs[i].State = JobRunning })
}

// chain records where Job i, an agent Job, answers.
func (r *hookRun) chain(i int, conv []byte, chain string) {
	r.update(func(run *HookRun) {
		run.Jobs[i].Conversation, run.Jobs[i].Chain = hex.EncodeToString(conv), chain
	})
}

// ended marks Job i ended as v says; a failure leaves the Jobs after it
// not run.
func (r *hookRun) ended(i int, v JobView) {
	r.update(func(run *HookRun) {
		if v.OK {
			run.Jobs[i].State = JobOK
			return
		}
		run.Jobs[i].State, run.Jobs[i].Error = JobFailed, v.Error
		for j := i + 1; j < len(run.Jobs); j++ {
			run.Jobs[j].State = JobNotRun
		}
	})
}

// stopped marks the Jobs from i on not run: serve stopped first.
func (r *hookRun) stopped(i int) {
	r.update(func(run *HookRun) {
		for j := i; j < len(run.Jobs); j++ {
			run.Jobs[j].State = JobNotRun
		}
	})
}

func (s *server) hookRunRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/hooks/runs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.hookRuns.all())
	})
}

// HookRuns is the firings of this Node's Hooks on Sign Requests since serve
// started, oldest first.
func (c Client) HookRuns(ctx context.Context) ([]HookRun, error) {
	var v []HookRun
	err := c.do(ctx, "/v1/hooks/runs", nil, &v)
	return v, err
}

// HookRuns is Client.HookRuns in this process.
func (r *Running) HookRuns(context.Context) ([]HookRun, error) { return r.s.hookRuns.all(), nil }
