package model

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	coslv1 "defios.com/proto/cosl/v1"
)

func claude(alias, model string) *coslv1.ModelConfig {
	return &coslv1.ModelConfig{Alias: alias, Provider: "anthropic", Model: model}
}

// rowOf wraps a payload as the COSL row the FSM would have stored.
func rowOf(t *testing.T, seq uint64, typ uint32, msg proto.Message) cosl.Row {
	t.Helper()
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Marshal(&coslv1.Envelope{Version: 1, MsgType: typ, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return cosl.Row{Seq: seq, MsgType: typ, MsgID: [32]byte{byte(seq)}, Envelope: env}
}

func TestFoldSupersedesAndRemoves(t *testing.T) {
	rows := []cosl.Row{
		rowOf(t, 1, cosl.TypeModelCreate, claude("b", "claude-haiku-5-5")),
		rowOf(t, 2, cosl.TypeModelCreate, claude("a", "claude-sonnet-5-5")),
		rowOf(t, 3, cosl.TypeModelUpdate, claude("a", "claude-opus-5-5")),
		rowOf(t, 4, cosl.TypeModelRemove, &coslv1.ModelRemove{Alias: "b"}),
		rowOf(t, 5, cosl.TypeAppFreeform, &coslv1.AppFreeform{Text: "noise"}),
	}
	got, err := Fold(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Alias != "a" || got[0].ModelID != "claude-opus-5-5" || got[0].ConfigID != [32]byte{3} {
		t.Fatalf("fold = %+v", got)
	}
}

func TestValidate(t *testing.T) {
	good := []*coslv1.ModelConfig{
		claude("opus", "claude-opus-5-5"),
		{Alias: "gpt", Provider: "openai", Model: "gpt-5"},
		{Alias: "proxied", Provider: "anthropic", Model: "claude-opus-5-5", BaseUrl: "https://llm.example/anthropic", ApiPath: "/v1/messages"},
		{Alias: "local", Provider: "openai", Model: "llama", BaseUrl: "http://127.0.0.1:8080", ApiPath: "/v1/chat/completions"},
	}
	for _, c := range good {
		if err := Validate(c); err != nil {
			t.Errorf("%s: %v", c.Alias, err)
		}
	}
	bad := map[string]*coslv1.ModelConfig{
		"bad alias":        claude("Bad Alias", "m"),
		"empty alias":      claude("", "m"),
		"unknown provider": {Alias: "x", Provider: "acme", Model: "m"},
		"no model":         claude("x", ""),
		"multiline model":  claude("x", "a\nb"),
		"bad url scheme":   {Alias: "x", Provider: "openai", Model: "m", BaseUrl: "ws://llm.example"},
		"url no host":      {Alias: "x", Provider: "openai", Model: "m", BaseUrl: "https://"},
		"url userinfo":     {Alias: "x", Provider: "openai", Model: "m", BaseUrl: "https://user:SECRET@llm.example"},
		"url query":        {Alias: "x", Provider: "openai", Model: "m", BaseUrl: "https://llm.example?key=SECRET"},
		"relative path":    {Alias: "x", Provider: "openai", Model: "m", ApiPath: "v1/messages"},
		"path with query":  {Alias: "x", Provider: "openai", Model: "m", ApiPath: "/v1/messages?key=SECRET"},
	}
	for name, c := range bad {
		if err := Validate(c); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}

func TestFoldIgnoresRacesAndInvalidMessages(t *testing.T) {
	rows := []cosl.Row{
		rowOf(t, 1, cosl.TypeModelCreate, claude("a", "claude-opus-5-5")),
		rowOf(t, 2, cosl.TypeModelCreate, claude("a", "claude-haiku-5-5")),                                   // lost the create race
		rowOf(t, 3, cosl.TypeModelUpdate, claude("ghost", "claude-opus-5-5")),                                // nothing to supersede
		rowOf(t, 4, cosl.TypeModelUpdate, &coslv1.ModelConfig{Alias: "a", Provider: "openai", Model: "gpt"}), // provider change
		rowOf(t, 5, cosl.TypeModelUpdate, claude("a", "")),                                                   // invalid
		rowOf(t, 6, cosl.TypeModelRemove, &coslv1.ModelRemove{Alias: "ghost"}),
		rowOf(t, 7, cosl.TypeModelCreate, claude("Bad Alias", "m")), // invalid
	}
	got, err := Fold(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ModelID != "claude-opus-5-5" || got[0].ConfigID != [32]byte{1} {
		t.Fatalf("fold = %+v", got)
	}
}

func TestRemovedAliasCanBeCreatedAgain(t *testing.T) {
	got, err := Fold([]cosl.Row{
		rowOf(t, 1, cosl.TypeModelCreate, claude("a", "claude-opus-5-5")),
		rowOf(t, 2, cosl.TypeModelRemove, &coslv1.ModelRemove{Alias: "a"}),
		rowOf(t, 3, cosl.TypeModelCreate, &coslv1.ModelConfig{Alias: "a", Provider: "openai", Model: "gpt-5"}),
	})
	if err != nil || len(got) != 1 || got[0].Provider != "openai" {
		t.Fatalf("fold = %+v, %v", got, err)
	}
}

// The writer refuses exactly what Fold would skip, so an operator gets an
// error rather than a silent no-op.
func TestCheckMirrorsTheFold(t *testing.T) {
	live := []Model{{Alias: "a", Provider: "anthropic", ModelID: "claude-opus-5-5"}}
	cases := []struct {
		name  string
		typ   uint32
		cfg   *coslv1.ModelConfig
		alias string
		want  error
	}{
		{"create new", cosl.TypeModelCreate, claude("b", "m"), "b", nil},
		{"create live", cosl.TypeModelCreate, claude("a", "m"), "a", ErrExists},
		{"update live", cosl.TypeModelUpdate, claude("a", "claude-haiku-5-5"), "a", nil},
		{"update absent", cosl.TypeModelUpdate, claude("b", "m"), "b", ErrUnknown},
		{"update provider", cosl.TypeModelUpdate, &coslv1.ModelConfig{Alias: "a", Provider: "openai", Model: "gpt"}, "a", ErrInvalid},
		{"remove live", cosl.TypeModelRemove, nil, "a", nil},
		{"remove absent", cosl.TypeModelRemove, nil, "b", ErrUnknown},
	}
	for _, c := range cases {
		err := Check(live, c.typ, c.cfg, c.alias)
		if (c.want == nil && err != nil) || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}
