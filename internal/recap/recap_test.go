package recap

import (
	"errors"
	"slices"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	coslv1 "defios.com/proto/cosl/v1"
)

// rowOf wraps a payload as the COSL row the FSM would have stored.
func rowOf(t *testing.T, seq uint64, typ uint32, msg proto.Message) cosl.Row {
	t.Helper()
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return rawRow(t, seq, typ, payload)
}

// rawRow is a row whose payload bytes need not decode: the FSM does not
// parse payloads, so any Roster Node can commit one.
func rawRow(t *testing.T, seq uint64, typ uint32, payload []byte) cosl.Row {
	t.Helper()
	env, err := proto.Marshal(&coslv1.Envelope{Version: 1, MsgType: typ, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return cosl.Row{Seq: seq, MsgType: typ, MsgID: [32]byte{byte(seq)}, Envelope: env}
}

func talk(t *testing.T, seq uint64, text string) cosl.Row {
	return rowOf(t, seq, cosl.TypeAppFreeform, &coslv1.AppFreeform{Text: text, ContentType: "text/plain"})
}

func recapRow(t *testing.T, seq, base uint64, text string) cosl.Row {
	return rowOf(t, seq, cosl.TypeAppRecap, &coslv1.AppRecap{Text: text, BaseSeq: base})
}

func seqs(rows []cosl.Row) []uint64 {
	out := make([]uint64, len(rows))
	for i, r := range rows {
		out[i] = r.Seq
	}
	return out
}

func TestFoldWithoutRecapIsAllTalk(t *testing.T) {
	rows := []cosl.Row{
		talk(t, 1, "a"),
		rowOf(t, 2, cosl.TypeModelRemove, &coslv1.ModelRemove{Alias: "noise"}),
		talk(t, 3, "b"),
	}
	v, err := Fold(rows)
	if err != nil {
		t.Fatal(err)
	}
	if v.Live != nil || !slices.Equal(seqs(v.Talk), []uint64{1, 3}) {
		t.Fatalf("view = %+v", v)
	}
}

func TestLiveRecapStandsInForTheTalkItCovers(t *testing.T) {
	rows := []cosl.Row{
		talk(t, 1, "a"),
		talk(t, 2, "b"),
		talk(t, 4, "c"),
		recapRow(t, 5, 2, "a and b"),
		talk(t, 6, "d"),
	}
	v, err := Fold(rows)
	if err != nil {
		t.Fatal(err)
	}
	if v.Live == nil || v.Live.Row.Seq != 5 || v.Live.Text != "a and b" || v.Live.BaseSeq != 2 || v.Live.Covers != 2 {
		t.Fatalf("live = %+v", v.Live)
	}
	if !slices.Equal(seqs(v.Talk), []uint64{4, 6}) {
		t.Fatalf("talk = %v", seqs(v.Talk))
	}
}

// A Recap that does not apply to the state it meets is skipped, identically
// on every Node, and the live Recap stays the one before it.
func TestFoldSkipsRecapsThatDoNotApply(t *testing.T) {
	cases := map[string]cosl.Row{
		"no further than the live one":  recapRow(t, 10, 3, "stale"),
		"behind the live one":           recapRow(t, 10, 2, "older"),
		"base outside the Conversation": recapRow(t, 10, 5, "seq 5 is another Conversation's"),
		"base not talk":                 recapRow(t, 10, 7, "seq 7 is the live Recap"),
		"base after itself":             recapRow(t, 10, 11, "the future"),
		"base is itself":                recapRow(t, 10, 10, "itself"),
		"empty text":                    recapRow(t, 10, 8, ""),
		"text too long":                 recapRow(t, 10, 8, string(make([]byte, cosl.MaxFreeformBytes+1))),
		"text not UTF-8":                rawRow(t, 10, cosl.TypeAppRecap, protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "\xff")),
	}
	for name, bad := range cases {
		rows := []cosl.Row{
			talk(t, 1, "a"),
			talk(t, 2, "b"),
			talk(t, 3, "c"),
			talk(t, 4, "d"),
			recapRow(t, 7, 3, "a to c"),
			talk(t, 8, "e"),
			bad,
			talk(t, 11, "f"),
		}
		v, err := Fold(rows)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if v.Live == nil || v.Live.Row.Seq != 7 || v.Live.Covers != 3 || !slices.Equal(seqs(v.Talk), []uint64{4, 8, 11}) {
			t.Errorf("%s: live %+v talk %v", name, v.Live, seqs(v.Talk))
		}
	}
}

// A later Recap that covers further supersedes the live one, and its covered
// count includes the talk the earlier one covered.
func TestFoldSupersedesByCoveringFurther(t *testing.T) {
	rows := []cosl.Row{
		talk(t, 1, "a"),
		recapRow(t, 2, 1, "a"),
		talk(t, 3, "b"),
		talk(t, 4, "c"),
		recapRow(t, 5, 3, "a and b"),
	}
	v, err := Fold(rows)
	if err != nil {
		t.Fatal(err)
	}
	if v.Live == nil || v.Live.Row.Seq != 5 || v.Live.Covers != 2 || !slices.Equal(seqs(v.Talk), []uint64{4}) {
		t.Fatalf("live %+v talk %v", v.Live, seqs(v.Talk))
	}
}

// Check is the writer's precondition: a Recap the fold would skip is an
// error, a stale one distinct from one naming no talk.
func TestCheck(t *testing.T) {
	v, err := Fold([]cosl.Row{
		talk(t, 1, "a"),
		talk(t, 2, "b"),
		recapRow(t, 3, 1, "a"),
		talk(t, 5, "c"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []uint64{2, 5} {
		if err := Check(v, base); err != nil {
			t.Errorf("base %d: %v", base, err)
		}
	}
	if err := Check(v, 1); !errors.Is(err, ErrStale) {
		t.Errorf("base 1: %v, want ErrStale", err)
	}
	for _, base := range []uint64{3, 4, 6} {
		if err := Check(v, base); !errors.Is(err, ErrBadBase) {
			t.Errorf("base %d: %v, want ErrBadBase", base, err)
		}
	}
	for _, base := range []uint64{0, 1} {
		if err := Check(View{}, base); !errors.Is(err, ErrBadBase) {
			t.Errorf("no talk, base %d: %v, want ErrBadBase", base, err)
		}
	}
}
