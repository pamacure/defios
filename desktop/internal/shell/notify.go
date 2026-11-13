package shell

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"defios.com/internal/node"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
)

// Notice is an OS notification. Clicking it opens the app on Group's
// Conversation and, if set, the card of the Sign Request whose msg_id is
// SignRequestID; Group "" opens the app as it is.
type Notice struct {
	Title, Body                        string
	Group, Conversation, SignRequestID string
}

// notifier posts notifications for what needs a person: a Sign Request
// awaiting this device's stance, a keygen Session awaiting acceptance, a
// Group proposal; and new messages, if that preference is on.
//
// A new Sign Request's notification waits Deps.HookGrace: if a Hook of
// this device's fires on it meanwhile, the notification says who is on
// it instead. A Job of that Hook that fails while this device has taken
// no stance notifies again; one that failed before that first notification
// makes it say the request needs the person.
type notifier struct {
	s *Shell

	mu      sync.Mutex
	reqs    map[string]signreq.Summary // each Sign Request's latest
	told    map[string]bool            // Sign Requests whose notification is posted or waits
	posted  map[string]bool            // Sign Requests whose first notification is posted
	runs    map[string][]node.HookRun  // by Sign Request
	failed  map[string]bool            // Jobs whose failure was told, by run and Job
	keygens map[string]bool
}

func newNotifier(s *Shell) *notifier {
	return &notifier{
		s: s, reqs: map[string]signreq.Summary{}, told: map[string]bool{}, posted: map[string]bool{},
		runs: map[string][]node.HookRun{}, failed: map[string]bool{}, keygens: map[string]bool{},
	}
}

// post sends n, unless notifications are off or the app is closing.
func (nt *notifier) post(n Notice) {
	if nt.s.ctx.Err() != nil || !nt.s.Prefs().Notifications() {
		return
	}
	nt.s.d.Notify(n)
}

// later runs f once the Hook grace has passed.
func (nt *notifier) later(f func()) { time.AfterFunc(nt.s.d.HookGrace, f) }

// event notifies of ev, an event of the Node that serves group.
func (nt *notifier) event(group, self string, ev node.Event) {
	switch {
	case ev.Proposal != nil:
		nt.proposal(*ev.Proposal)
	case group == "":
	case ev.Session != nil && ev.Session.Group == group:
		nt.session(group, ev.Session.Session)
	case ev.SignRequest != nil && ev.SignRequest.Group == group:
		nt.signRequest(group, ev.SignRequest.Request)
	case ev.HookRun != nil && ev.HookRun.Group == group:
		nt.hookRun(group, ev.HookRun.Run)
	case ev.Message != nil && ev.Message.Group == group:
		nt.message(group, self, ev.Message)
	}
}

func (nt *notifier) proposal(p node.Proposal) {
	from := shortKey(p.Initiator)
	for _, r := range p.Roster {
		if r.NodeID == p.Initiator && r.DisplayName != "" {
			from = r.DisplayName
		}
	}
	nt.post(Notice{
		Title: "Group proposal · needs you",
		Body:  fmt.Sprintf("%s invites this device to form a Group of %d devices. Open DeFiOS to accept or decline.", from, len(p.Roster)),
	})
}

// session notifies of a keygen Session another device started, unless
// this device accepted it within the grace, as it does one it started.
func (nt *notifier) session(group string, sum node.Session) {
	if sum.SignRequest != "" || sum.Status != session.Open {
		return
	}
	nt.mu.Lock()
	seen := nt.keygens[sum.SessionID]
	nt.keygens[sum.SessionID] = true
	nt.mu.Unlock()
	if seen {
		return
	}
	var tell func()
	tell = func() {
		if nt.s.talk.startingKeygen() {
			// This device may have started it: StartKeygen has not said.
			nt.later(tell)
			return
		}
		if nt.s.talk.hasAccepted(sum.SessionID) {
			return
		}
		what := fmt.Sprintf("%s, %d of %d", curveName(sum.Curve), sum.T, sum.N)
		if sum.ProposedAlias != "" {
			what = sum.ProposedAlias + " (" + what + ")"
		}
		nt.post(Notice{
			Title: "New Key · needs you",
			Body:  fmt.Sprintf("A new Key is being made: %s. Accept it on the Talk page to take part.", what),
			Group: group, Conversation: node.GroupTalk,
		})
	}
	nt.later(tell)
}

// awaitsStance reports whether r waits on this device's stance.
func awaitsStance(r signreq.Summary) bool {
	return r.Status == signreq.Open && r.Stance == signreq.StanceUnaddressed
}

func (nt *notifier) signRequest(group string, r signreq.Summary) {
	nt.mu.Lock()
	nt.reqs[r.MsgID] = r
	tell := !nt.told[r.MsgID] && awaitsStance(r)
	if tell {
		nt.told[r.MsgID] = true
	}
	nt.mu.Unlock()
	if !tell {
		return
	}
	nt.later(func() {
		nt.mu.Lock()
		r, runs := nt.reqs[r.MsgID], slices.Clone(nt.runs[r.MsgID])
		nt.posted[r.MsgID] = true
		failed := nt.newlyFailed(r, runs)
		nt.mu.Unlock()
		n := Notice{Group: group, Conversation: r.Conversation, SignRequestID: r.MsgID}
		switch {
		case !awaitsStance(r):
			return
		case len(failed) > 0:
			nt.postFailed(group, r, failed)
			return
		case len(runs) > 0:
			n.Title, n.Body = "New Sign Request · "+onIt(runs)+", no action needed", r.Reason
		default:
			n.Title = "New Sign Request · needs you"
			n.Body = nt.s.talk.asker(r) + " asks: " + r.Reason
		}
		nt.post(n)
	})
}

// newlyFailed are runs' Jobs that failed and were not told yet, marked
// told, while r awaits this device's stance; nt.mu is held.
func (nt *notifier) newlyFailed(r signreq.Summary, runs []node.HookRun) []node.JobRun {
	if !awaitsStance(r) {
		return nil
	}
	var out []node.JobRun
	for _, run := range runs {
		for _, j := range run.Jobs {
			k := fmt.Sprintf("%d/%s", run.ID, j.Job)
			if j.State == node.JobFailed && !nt.failed[k] {
				nt.failed[k] = true
				out = append(out, j)
			}
		}
	}
	return out
}

// postFailed says r needs the person, as its Jobs failed.
func (nt *notifier) postFailed(group string, r signreq.Summary, failed []node.JobRun) {
	var why []string
	for _, j := range failed {
		what := "Job " + j.Job
		if j.Handle != "" {
			what = fmt.Sprintf("@%s (Job %s)", j.Handle, j.Job)
		}
		why = append(why, fmt.Sprintf("%s failed: %s.", what, j.Error))
	}
	body := strings.Join(why, " ")
	if r.Reason != "" {
		body += fmt.Sprintf(" “%s” awaits your stance.", r.Reason)
	}
	nt.post(Notice{Title: "Sign Request needs you", Body: body, Group: group, Conversation: r.Conversation, SignRequestID: r.MsgID})
}

// onIt says who of runs' Jobs is on a request: their agents, or the Jobs
// if none has one.
func onIt(runs []node.HookRun) string {
	var agents, jobs []string
	for _, r := range runs {
		for _, j := range r.Jobs {
			if j.Handle != "" && !slices.Contains(agents, "@"+j.Handle) {
				agents = append(agents, "@"+j.Handle)
			}
			if !slices.Contains(jobs, j.Job) {
				jobs = append(jobs, j.Job)
			}
		}
	}
	who := agents
	if len(who) == 0 {
		who = jobs
		if len(who) == 1 {
			return "Job " + who[0] + " is on it"
		}
		return "Jobs " + listed(who) + " are on it"
	}
	if len(who) == 1 {
		return who[0] + " is on it"
	}
	return listed(who) + " are on it"
}

// listed joins words as a list: "a", "a and b", "a, b and c".
func listed(words []string) string {
	if len(words) <= 1 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// hookRun records run, and notifies of each of its Jobs that failed while
// its request awaits this device's stance, once the request's first
// notification is out: until then that one tells of them.
func (nt *notifier) hookRun(group string, run node.HookRun) {
	id := run.Event.SignRequestID
	nt.mu.Lock()
	runs := nt.runs[id]
	if i := slices.IndexFunc(runs, func(r node.HookRun) bool { return r.ID == run.ID }); i >= 0 {
		runs[i] = run
	} else {
		runs = append(runs, run)
	}
	nt.runs[id] = runs
	r := nt.reqs[id]
	var failed []node.JobRun
	if nt.posted[id] {
		failed = nt.newlyFailed(r, []node.HookRun{run})
	}
	nt.mu.Unlock()
	if len(failed) > 0 {
		nt.postFailed(group, r, failed)
	}
}

// message notifies of another's message, with new-message notifications
// on.
func (nt *notifier) message(group, self string, ev *node.MessageEvent) {
	m := ev.Message
	if m.Mine || !m.Readable() || !nt.s.Prefs().MessageNotifications {
		return
	}
	from := nt.s.talk.from(m)
	switch {
	case m.Handle != "":
		from += " · @" + m.Handle
	case m.Depth > 0:
		from += " · Job"
	}
	body := m.Text
	if r := []rune(body); len(r) > 200 {
		body = string(r[:200]) + "…"
	}
	nt.post(Notice{
		Title: fmt.Sprintf("%s in %s", from, nt.s.talk.convName(ev.Conversation)),
		Body:  body, Group: group, Conversation: ev.Conversation,
	})
}

// startingKeygen reports whether StartKeygen is under way here.
func (t *talk) startingKeygen() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.starting > 0
}

// hasAccepted reports whether this device accepted keygen Session id.
func (t *talk) hasAccepted(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.accepted[id]
}

// asker names who proposed r: its device, and its agent.
func (t *talk) asker(r signreq.Summary) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	who := t.device(r.Proposer)
	if r.ProposerHandle != "" {
		who += " · @" + r.ProposerHandle
	}
	return who
}

// from names the device that wrote m.
func (t *talk) from(m node.Message) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m.NodeID != t.self && m.DisplayName != "" {
		return m.DisplayName
	}
	return t.device(m.NodeID)
}

// convName names Conversation conv as the left column does.
func (t *talk) convName(conv string) string {
	if conv == node.GroupTalk {
		return "Group"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if k := t.key(conv); k != nil {
		if k.Alias != "" {
			return k.Alias
		}
		return curveName(k.Curve) + " " + shortKey(k.PublicKey)
	}
	return shortKey(conv)
}
