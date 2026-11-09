package serve_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/signreq"
)

func generatorConfigFile(n *node, groupAlias string) string {
	return n.paths.ConversationConfigFile(groupAlias, "ecdsa-"+hex.EncodeToString(generator)[:12])
}

// sign fill and sign propose read the alias from the config.toml of the
// Group serve is serving, afresh each time, so an edit needs no restart.
func TestSignResolvesAKeyAliasOfTheServedGroupWithNoRestart(t *testing.T) {
	a, _, conv := fillPair(t, `["say"]`)
	ctx := context.Background()
	var txs []signreq.TxSpec
	if err := json.Unmarshal([]byte(partialEth), &txs); err != nil {
		t.Fatal(err)
	}

	writeConfig(t, generatorConfigFile(a, "another-group"), "alias = \"Treasury\"\n")
	if _, err := a.client().SignFill(ctx, "Treasury", txs); !errors.Is(err, key.ErrUnknown) {
		t.Fatalf("an alias of another Group: err = %v", err)
	}

	file := generatorConfigFile(a, group.ImplicitAlias)
	writeConfig(t, file, "alias = \"Treasury\"\n")
	res, err := a.client().SignFill(ctx, "tREASURY", txs)
	if err != nil {
		t.Fatalf("sign fill by alias: %v", err)
	}
	v, err := a.client().SignPropose(ctx, "Treasury", signreq.Spec{Reason: "pay", Transactions: res.Transactions})
	if err != nil {
		t.Fatalf("sign propose by alias: %v", err)
	}
	d := waitSign(t, a, v.MsgID, func(signreq.Detail) bool { return true })
	if want := hex.EncodeToString(conv); d.Conversation != want {
		t.Errorf("proposed in Conversation %s, want %s", d.Conversation, want)
	}

	writeConfig(t, file, "alias = \"Savings\"\n")
	if _, err := a.client().SignFill(ctx, "Treasury", txs); !errors.Is(err, key.ErrUnknown) {
		t.Errorf("the old alias: err = %v", err)
	}
	if _, err := a.client().SignFill(ctx, "Savings", txs); err != nil {
		t.Errorf("the new alias: %v", err)
	}
}

// An alias on two Keys names neither, and a config.toml that did not read
// might hold the alias, so each fails sign propose and sign fill alike.
func TestSignRefusesAnAmbiguousOrUnreadableAlias(t *testing.T) {
	a, b, gid, _ := agentPair(t, "auditor")
	ctx := context.Background()
	other := strings.Repeat("22", 33)
	otherKey, err := hex.DecodeString(other)
	if err != nil {
		t.Fatal(err)
	}
	waitWorkspace(t, a, mintPublicKey(t, a, b, gid, generator))
	otherName := mintPublicKey(t, a, b, gid, otherKey)
	waitWorkspace(t, a, otherName)
	otherFile := a.paths.ConversationConfigFile(group.ImplicitAlias, otherName)

	writeConfig(t, generatorConfigFile(a, group.ImplicitAlias), "alias = \"twin\"\n")
	writeConfig(t, otherFile, "alias = \"TWIN\"\n")
	for name, call := range map[string]func(string) error{
		"propose": func(q string) error {
			_, err := a.client().SignPropose(ctx, q, signreq.Spec{Reason: "pay"})
			return err
		},
		"fill": func(q string) error {
			_, err := a.client().SignFill(ctx, q, nil)
			return err
		},
	} {
		err := call("twin")
		if !errors.Is(err, key.ErrAmbiguous) || !strings.Contains(err.Error(), other) {
			t.Errorf("%s: alias on two Keys: err = %v", name, err)
		}
		if err := call("nobody"); !errors.Is(err, key.ErrUnknown) {
			t.Errorf("%s: unknown alias: err = %v", name, err)
		}
	}

	writeConfig(t, otherFile, "alias = \n")
	if _, err := a.client().SignFill(ctx, "twin", nil); err == nil || !strings.Contains(err.Error(), otherFile) {
		t.Errorf("a broken config.toml: err = %v, want it named", err)
	}
}
