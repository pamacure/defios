package serve

import "defios.com/internal/transcript"

// Transcript outcomes: how a run ended (ADR 0018); see package transcript.
const (
	OutcomeWrote     = transcript.OutcomeWrote
	OutcomeSilent    = transcript.OutcomeSilent
	OutcomeError     = transcript.OutcomeError
	OutcomeCancelled = transcript.OutcomeCancelled
)

// Transcript line kinds: which of a TranscriptLine's fields it holds.
const (
	KindTurn     = transcript.KindTurn
	KindRequest  = transcript.KindRequest
	KindBlock    = transcript.KindBlock
	KindUsage    = transcript.KindUsage
	KindActed    = transcript.KindActed
	KindSummoned = transcript.KindSummoned
	KindOutcome  = transcript.KindOutcome
)

// TranscriptLine is one line of an Agent's Transcript.
type TranscriptLine = transcript.Line

// runOutcome is the closing line of a run that posted wrote (its MsgID empty
// if nothing) and ended with err: see transcript.Outcome.
func runOutcome(wrote SendView, err error, cancelled, stopped bool) (TranscriptLine, bool) {
	return transcript.Outcome(wrote.MsgID, wrote.Seq, err, cancelled, stopped)
}
