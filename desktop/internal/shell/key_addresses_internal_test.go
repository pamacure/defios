package shell

import (
	"errors"
	"slices"
	"testing"

	"defios.com/internal/node"
	"defios.com/internal/signreq"
)

// The About panel shows the Key's addresses as the Node gives them, the
// ones key_address gives, each labelled by its Network; none with no
// Network of its type, and why when they could not be made.
func TestAKeysAddressesAreTheNodesLabelledByNetwork(t *testing.T) {
	var tk talk
	k := node.Key{Curve: "ecdsa", NetworkType: "bitcoin", Addresses: []signreq.NetworkAddress{
		{Network: "btc", Type: "bitcoin", Address: "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"},
		{Network: "tbtc", Type: "bitcoin", Address: "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"},
	}}
	want := []KeyAddress{
		{"Address on btc", "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"},
		{"Address on tbtc", "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"},
	}
	if d := tk.keyDetail(k); !slices.Equal(d.Addresses, want) || d.AddressError != "" {
		t.Errorf("Addresses = %v, %q, want %v", d.Addresses, d.AddressError, want)
	}

	k = node.Key{Curve: "ecdsa", NetworkType: "ethereum", Addresses: []signreq.NetworkAddress{}}
	if d := tk.keyDetail(k); d.Addresses == nil || len(d.Addresses) != 0 {
		t.Errorf("with no Network of its type, Addresses = %#v, want empty", d.Addresses)
	}

	k.AddressesErr = errors.New("config.toml: bad")
	if d := tk.keyDetail(k); d.AddressError != "config.toml: bad" {
		t.Errorf("AddressError = %q", d.AddressError)
	}
}
