package provider

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/agent"
	"defios.com/internal/cosl"
	coslv1 "defios.com/proto/cosl/v1"
)

func anthropic(alias string) *coslv1.ProviderConfig {
	return &coslv1.ProviderConfig{Alias: alias, BaseUrl: "https://api.anthropic.com", ApiPath: "/v1/messages"}
}

func local(alias string) *coslv1.ProviderConfig {
	return &coslv1.ProviderConfig{Alias: alias, BaseUrl: "http://127.0.0.1:8080", ApiPath: "/v1/chat/completions"}
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

// An update may change both base_url and api_path, even across shapes; a
// remove drops the alias.
func TestFoldSupersedesAndRemoves(t *testing.T) {
	rows := []cosl.Row{
		rowOf(t, 1, cosl.TypeProviderCreate, anthropic("b")),
		rowOf(t, 2, cosl.TypeProviderCreate, anthropic("a")),
		rowOf(t, 3, cosl.TypeProviderUpdate, local("a")),
		rowOf(t, 4, cosl.TypeProviderRemove, &coslv1.ProviderRemove{Alias: "b"}),
		rowOf(t, 5, cosl.TypeAppFreeform, &coslv1.AppFreeform{Text: "noise"}),
	}
	got, err := Fold(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Alias != "a" || got[0].BaseURL != "http://127.0.0.1:8080" || got[0].APIPath != "/v1/chat/completions" || got[0].ConfigID != [32]byte{3} {
		t.Fatalf("fold = %+v", got)
	}
	if got[0].Shape() != agent.ChatCompletions || got[0].URL() != "http://127.0.0.1:8080/v1/chat/completions" {
		t.Fatalf("shape %v, url %s", got[0].Shape(), got[0].URL())
	}
}

func TestValidate(t *testing.T) {
	good := []*coslv1.ProviderConfig{
		anthropic("anthropic"),
		local("local"),
		{Alias: "proxied", BaseUrl: "https://llm.example/anthropic/", ApiPath: "/v1/messages"},
		{Alias: "oai", BaseUrl: "https://api.openai.com", ApiPath: "/v1/chat/completions"},
	}
	for _, c := range good {
		if err := Validate(c); err != nil {
			t.Errorf("%s: %v", c.Alias, err)
		}
	}
	bad := map[string]*coslv1.ProviderConfig{
		"bad alias":       {Alias: "Bad Alias", BaseUrl: "https://x.example", ApiPath: "/v1/messages"},
		"empty alias":     {BaseUrl: "https://x.example", ApiPath: "/v1/messages"},
		"no base url":     {Alias: "x", ApiPath: "/v1/messages"},
		"no api path":     {Alias: "x", BaseUrl: "https://x.example"},
		"unknown shape":   {Alias: "x", BaseUrl: "https://x.example", ApiPath: "/v1/foo"},
		"decisions":       {Alias: "x", BaseUrl: "https://x.example", ApiPath: "/v1/decisions"},
		"bad url scheme":  {Alias: "x", BaseUrl: "ws://llm.example", ApiPath: "/v1/messages"},
		"url no host":     {Alias: "x", BaseUrl: "https://", ApiPath: "/v1/messages"},
		"url userinfo":    {Alias: "x", BaseUrl: "https://user:SECRET@llm.example", ApiPath: "/v1/messages"},
		"url query":       {Alias: "x", BaseUrl: "https://llm.example?key=SECRET", ApiPath: "/v1/messages"},
		"url fragment":    {Alias: "x", BaseUrl: "https://llm.example#frag", ApiPath: "/v1/messages"},
		"relative path":   {Alias: "x", BaseUrl: "https://x.example", ApiPath: "v1/messages"},
		"path with query": {Alias: "x", BaseUrl: "https://x.example", ApiPath: "/v1/messages?key=SECRET"},
		"path fragment":   {Alias: "x", BaseUrl: "https://x.example", ApiPath: "/v1/messages#x"},
	}
	for name, c := range bad {
		if err := Validate(c); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}

func TestFoldIgnoresRacesAndInvalidMessages(t *testing.T) {
	rows := []cosl.Row{
		rowOf(t, 1, cosl.TypeProviderCreate, anthropic("a")),
		rowOf(t, 2, cosl.TypeProviderCreate, local("a")),                                                                           // lost the create race
		rowOf(t, 3, cosl.TypeProviderUpdate, local("ghost")),                                                                       // nothing to supersede
		rowOf(t, 4, cosl.TypeProviderUpdate, &coslv1.ProviderConfig{Alias: "a", BaseUrl: "https://x.example", ApiPath: "/v1/foo"}), // no shape
		rowOf(t, 5, cosl.TypeProviderCreate, &coslv1.ProviderConfig{Alias: "x", BaseUrl: "https://x.example", ApiPath: "/v1/foo"}), // no shape
		rowOf(t, 6, cosl.TypeProviderRemove, &coslv1.ProviderRemove{Alias: "ghost"}),
		rowOf(t, 7, cosl.TypeProviderCreate, anthropic("Bad Alias")), // invalid
	}
	got, err := Fold(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].APIPath != "/v1/messages" || got[0].ConfigID != [32]byte{1} {
		t.Fatalf("fold = %+v", got)
	}
}

func TestRemovedAliasCanBeCreatedAgain(t *testing.T) {
	got, err := Fold([]cosl.Row{
		rowOf(t, 1, cosl.TypeProviderCreate, anthropic("a")),
		rowOf(t, 2, cosl.TypeProviderRemove, &coslv1.ProviderRemove{Alias: "a"}),
		rowOf(t, 3, cosl.TypeProviderCreate, local("a")),
	})
	if err != nil || len(got) != 1 || got[0].APIPath != "/v1/chat/completions" {
		t.Fatalf("fold = %+v, %v", got, err)
	}
}

// The writer refuses exactly what Fold would skip, so an operator gets an
// error rather than a silent no-op.
func TestCheckMirrorsTheFold(t *testing.T) {
	live := []Provider{{Alias: "a", BaseURL: "https://api.anthropic.com", APIPath: "/v1/messages"}}
	cases := []struct {
		name  string
		typ   uint32
		alias string
		want  error
	}{
		{"create new", cosl.TypeProviderCreate, "b", nil},
		{"create live", cosl.TypeProviderCreate, "a", ErrExists},
		{"update live", cosl.TypeProviderUpdate, "a", nil},
		{"update absent", cosl.TypeProviderUpdate, "b", ErrUnknown},
		{"remove live", cosl.TypeProviderRemove, "a", nil},
		{"remove absent", cosl.TypeProviderRemove, "b", ErrUnknown},
	}
	for _, c := range cases {
		err := Check(live, c.typ, c.alias)
		if (c.want == nil && err != nil) || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}

// A preset is a complete row; an alias that names one uses it, and flags
// override its fields.
func TestPresets(t *testing.T) {
	p, ok := Preset("anthropic")
	if !ok || p.BaseURL != "https://api.anthropic.com" || p.APIPath != "/v1/messages" {
		t.Fatalf("anthropic preset = %+v, %v", p, ok)
	}
	p, ok = Preset("openai")
	if !ok || p.BaseURL != "https://api.openai.com" || p.APIPath != "/v1/responses" {
		t.Fatalf("openai preset = %+v, %v", p, ok)
	}
	if _, ok := Preset("acme"); ok {
		t.Fatal("acme is a preset")
	}
}
