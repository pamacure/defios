package serve

import "defios.com/internal/transcript"

// Transcript outcomes: how a run ended (ADR 0018); see package transcript.
const (
	OutcomeSaid      = transcript.OutcomeSaid
	OutcomeSilent    = transcript.OutcomeSilent
	OutcomeError     = transcript.OutcomeError
	OutcomeCancelled = transcript.OutcomeCancelled
)

// Transcript line kinds: which of a TranscriptLine's fields it holds.
const (
	KindRequest = transcript.KindRequest
	KindBlock   = transcript.KindBlock
	KindUsage   = transcript.KindUsage
	KindActed   = transcript.KindActed
	KindOutcome = transcript.KindOutcome
)

// TranscriptLine is one line of a chain's Transcript.
type TranscriptLine = transcript.Line

// runOutcome is the closing line of a run that posted said (its MsgID empty
// if nothing) and ended with err: see transcript.Outcome.
func runOutcome(said SendView, err error, cancelled, stopped bool) (TranscriptLine, bool) {
	return transcript.Outcome(said.MsgID, said.Seq, err, cancelled, stopped)
}
