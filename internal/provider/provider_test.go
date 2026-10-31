package provider

import (
	"errors"
	"strings"
	"testing"

	"defios.com/internal/agent"
)

func TestValidate(t *testing.T) {
	good := []Provider{
		{Alias: "anthropic", BaseURL: "https://api.anthropic.com", APIPath: "/v1/messages"},
		{Alias: "local", BaseURL: "http://127.0.0.1:8080", APIPath: "/v1/chat/completions"},
		{Alias: "proxied", BaseURL: "https://llm.example/anthropic/", APIPath: "/v1/messages"},
		{Alias: "oai", BaseURL: "https://api.openai.com", APIPath: "/v1/responses"},
	}
	for _, p := range good {
		if err := Validate(p); err != nil {
			t.Errorf("%s: %v", p.Alias, err)
		}
	}
	bad := map[string]Provider{
		"bad alias":       {Alias: "Bad Alias", BaseURL: "https://x.example", APIPath: "/v1/messages"},
		"empty alias":     {BaseURL: "https://x.example", APIPath: "/v1/messages"},
		"no base url":     {Alias: "x", APIPath: "/v1/messages"},
		"no api path":     {Alias: "x", BaseURL: "https://x.example"},
		"unknown shape":   {Alias: "x", BaseURL: "https://x.example", APIPath: "/v1/foo"},
		"decisions":       {Alias: "x", BaseURL: "https://x.example", APIPath: "/v1/decisions"},
		"bad url scheme":  {Alias: "x", BaseURL: "ws://llm.example", APIPath: "/v1/messages"},
		"url no host":     {Alias: "x", BaseURL: "https://", APIPath: "/v1/messages"},
		"url userinfo":    {Alias: "x", BaseURL: "https://user:SECRET@llm.example", APIPath: "/v1/messages"},
		"url query":       {Alias: "x", BaseURL: "https://llm.example?key=SECRET", APIPath: "/v1/messages"},
		"url fragment":    {Alias: "x", BaseURL: "https://llm.example#SECRET", APIPath: "/v1/messages"},
		"relative path":   {Alias: "x", BaseURL: "https://x.example", APIPath: "v1/messages"},
		"path with query": {Alias: "x", BaseURL: "https://x.example", APIPath: "/v1/messages?key=SECRET"},
		"path fragment":   {Alias: "x", BaseURL: "https://x.example", APIPath: "/v1/messages#SECRET"},
	}
	for name, p := range bad {
		err := Validate(p)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		} else if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: %v echoes what may be a credential", name, err)
		}
	}
}

// A Provider wrong in both base_url and api_path names both problems.
func TestValidateReportsBothFields(t *testing.T) {
	err := Validate(Provider{Alias: "x", APIPath: "/v1/foo"})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "base url is required") || !strings.Contains(err.Error(), `api path "/v1/foo" must end in one of`) {
		t.Fatalf("got %v", err)
	}
}

// A Conversation's table may leave out what the Group's supplies, but what
// it sets must be valid.
func TestValidateOverrideChecksOnlyWhatIsSet(t *testing.T) {
	for _, p := range []Provider{{Alias: "x"}, {Alias: "x", BaseURL: "https://x.example"}, {Alias: "x", APIPath: "/v1/responses"}} {
		if err := ValidateOverride(p); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	for _, p := range []Provider{{Alias: "x", BaseURL: "https://u:p@x.example"}, {Alias: "x", APIPath: "/v1/foo"}, {Alias: "X"}} {
		if err := ValidateOverride(p); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: got %v, want ErrInvalid", p, err)
		}
	}
}

func TestShapeAndURL(t *testing.T) {
	p := Provider{Alias: "a", BaseURL: "http://127.0.0.1:8080/", APIPath: "/v1/chat/completions"}
	if p.Shape() != agent.ChatCompletions || p.URL() != "http://127.0.0.1:8080/v1/chat/completions" {
		t.Fatalf("shape %v, url %s", p.Shape(), p.URL())
	}
}

// A preset is a complete Provider; an alias that names one uses it, and
// flags override its fields.
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
