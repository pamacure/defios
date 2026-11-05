package talk_test

import (
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"

	"defios.com/internal/cosl"
	"defios.com/internal/talk"
	"defios.com/internal/transcript"
	coslv1 "defios.com/proto/cosl/v1"
)

// agentSays commits by's agent handle's text in Group-level talk, and is
// its msg_id, hex.
func (f *fixture) agentSays(by cosl.Author, handle, text string) string {
	s := f.commit(f.sign(by, nil, &coslv1.AppFreeform{Text: text, ContentType: "text/plain", Handle: handle, Depth: 1}))
	return hex.EncodeToString(s.MsgID[:])
}

// transcribe writes lines as the Transcript of chain in Group-level talk.
func (f *fixture) transcribe(chain string, lines ...transcript.Line) {
	w := transcript.NewWriter(f.paths, "home", nil, chain, f.t.Logf)
	defer w.Close()
	for _, l := range lines {
		w.Write(l)
	}
}

func block(text string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"type": "text", "text": text})
	return b
}

// blockTexts is each message's text, then the texts of its blocks.
func blockTexts(es []talk.Expanded) [][]string {
	var out [][]string
	for _, e := range es {
		got := []string{e.Text}
		for _, b := range e.Blocks {
			var v struct{ Text string }
			json.Unmarshal(b, &v)
			got = append(got, v.Text)
		}
		out = append(out, got)
	}
	return out
}

// An agent's message this Node's chain said comes with the blocks of the
// run that said it, a failed run's left out; a person's, another Node's
// agent's, and one whose Transcript is gone come with none.
func TestExpandPairsEachMessageWithTheRunThatSaidIt(t *testing.T) {
	f := newFixture(t)
	f.say(f.alice, "check this")
	fixed := f.agentSays(f.alice, "auditor", "fixed")
	f.agentSays(f.bob, "critic", "from bob's Node")
	f.agentSays(f.alice, "scribe", "transcript gone")
	run := func(step int, handle, kind string) transcript.Line {
		return transcript.Line{Step: step, Handle: handle, Depth: 1, Kind: kind}
	}
	l := func(l transcript.Line, set func(*transcript.Line)) transcript.Line { set(&l); return l }
	f.transcribe("abab",
		l(run(1, "auditor", transcript.KindBlock), func(l *transcript.Line) { l.Block = block("looking") }),
		l(run(1, "auditor", transcript.KindBlock), func(l *transcript.Line) { l.Block = block("found it") }),
		l(run(1, "auditor", transcript.KindOutcome), func(l *transcript.Line) { l.Outcome, l.MsgID, l.Seq = transcript.OutcomeSaid, fixed, 2 }),
		l(run(2, "quant", transcript.KindBlock), func(l *transcript.Line) { l.Block = block("failed try") }),
		l(run(2, "quant", transcript.KindOutcome), func(l *transcript.Line) { l.Outcome, l.Error = transcript.OutcomeError, "boom" }),
	)

	got, err := f.reader().Expand(nil, f.read(nil).Chat())
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"check this"}, {"fixed", "looking", "found it"}, {"from bob's Node"}, {"transcript gone"}}
	if g := blockTexts(got); !reflect.DeepEqual(g, want) {
		t.Errorf("expanded = %q, want %q", g, want)
	}
}
