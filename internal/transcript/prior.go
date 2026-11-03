package transcript

// Prior is each step of a chain's latest attempt, by step, as its
// Transcript shows it: what a retry reuses, or runs again (ADR 0018).
// It is plain data, so a chain's plan can walk it without reading files.
type Prior map[int]PriorRun

// PriorRun is a step's latest attempt. Outcome is "" for a run cut off by
// a stop.
type PriorRun struct {
	Handle  string
	Attempt int
	Outcome string
	MsgID   string // with OutcomeSaid: what it said, hex
}

// Finished reports whether a retry reuses the run instead of running it:
// it said or stayed silent.
func (r PriorRun) Finished() bool {
	return r.Outcome == OutcomeSaid || r.Outcome == OutcomeSilent
}

// Prior is t's steps' latest attempts; nil when it has none.
func (t Transcript) Prior() Prior {
	var p Prior
	for _, r := range t.Runs() {
		if p == nil {
			p = Prior{}
		}
		p[r.Step] = PriorRun{Handle: r.Handle, Attempt: r.Attempt, Outcome: r.Outcome, MsgID: r.MsgID}
	}
	return p
}
