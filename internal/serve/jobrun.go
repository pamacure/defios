package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"defios.com/internal/config"
	"defios.com/internal/trigger"
)

// ErrNoJob means config.toml defines no Job of that name.
var ErrNoJob = errors.New("serve: no such Job")

// jobRunRequest is POST /v1/job/run's body: the Job, by name, and the
// event to run it on, if any.
type jobRunRequest struct {
	Job   string         `json:"job"`
	Event *trigger.Event `json:"event,omitempty"`
}

// JobView is how a Job run ended: OK, or the Error it failed on.
type JobView struct {
	Job   string `json:"job"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *server) jobRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/job/run", func(w http.ResponseWriter, r *http.Request) {
		var req jobRunRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		v, err := s.jobRun(r.Context(), req)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, v)
	})
}

// jobRun runs the Job req names, as config.toml now defines it, on its
// event, and waits for it to end (ADR 0032). It is no Trigger's firing:
// the event's trigger is cleared, its group is this one, and its depth,
// 0 if no event is given, is the one a Job's writes go one below. A Job
// that fails is reported in the view, not as an error.
func (s *server) jobRun(ctx context.Context, req jobRunRequest) (JobView, error) {
	_, store, ok := s.current()
	if !ok {
		return JobView{}, ErrNoReplica
	}
	e, err := config.Load(s.opts.Paths, s.opts.Alias, nil, nil)
	if err != nil {
		return JobView{}, err
	}
	j, ok := e.Group.Jobs[req.Job]
	if !ok {
		return JobView{}, fmt.Errorf("%w: no [jobs.%s] in this Group's config.toml", ErrNoJob, req.Job)
	}
	var ev trigger.Event
	if req.Event != nil {
		ev = *req.Event
	}
	ev.Trigger, ev.Group = "", s.opts.Alias
	if ev.From == "" {
		ev.From = s.opts.Node.NodeID.String()
	}
	r := s.jobRunner(store, "serve: job run "+req.Job+": ", nil)
	return runJob(ctx, &s.chains, r, req.Job, j, ev), nil
}

// JobRun is `job run`: run Job name on ev, nil for none, and wait for it
// to end, however long that takes; ctx ending cuts it short.
func (c Client) JobRun(ctx context.Context, name string, ev *trigger.Event) (JobView, error) {
	var v JobView
	err := c.doUntimed(ctx, "/v1/job/run", jobRunRequest{Job: name, Event: ev}, &v)
	return v, err
}

// JobRun is Client.JobRun in this process.
func (r *Running) JobRun(ctx context.Context, name string, ev *trigger.Event) (JobView, error) {
	return r.s.jobRun(ctx, jobRunRequest{Job: name, Event: ev})
}
