package signreq

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
)

// usdc and alice are an ERC-20 contract and a recipient, as an agent
// writes them: EIP-55 hex.
const (
	usdc  = "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"
	alice = "0x60d8c2D6A6B6d7e36D1b2f1E3b11a02f3B1309E5"
)

func buildEth(t *testing.T, intent string) ([]byte, error) {
	t.Helper()
	r, err := Build(Spec{Reason: "r", Transactions: []TxSpec{{Network: "eth", Ethereum: json.RawMessage(intent)}}}, nets)
	if err != nil {
		return nil, err
	}
	return r.Transactions[0].GetEthereum().Data, nil
}

// A call intent is encoded by the Node into the data an ERC-20 transfer
// carries: its selector, then the recipient and the amount, each a word.
func TestACallIntentEncodesItsData(t *testing.T) {
	data, err := buildEth(t, `{"to":"`+usdc+`","value":"0","call":{"signature":"transfer(address to, uint256 amount)","args":["`+alice+`","100000"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	want := "a9059cbb" +
		"00000000000000000000000060d8c2d6a6b6d7e36d1b2f1e3b11a02f3b1309e5" +
		"00000000000000000000000000000000000000000000000000000000000186a0"
	if got := hex.EncodeToString(data); got != want {
		t.Errorf("data = %s, want %s", got, want)
	}
}

// What the Node cannot encode faithfully is refused, naming the slip:
// call with data, a number that may be rounded, a mistyped address.
func TestAnIntentTheNodeCannotTrustIsRefused(t *testing.T) {
	transfer := `"call":{"signature":"transfer(address,uint256)","args":["` + alice + `","1"]}`
	for _, c := range []struct{ name, intent, says string }{
		{"call and data", `{"to":"` + usdc + `",` + transfer + `,"data":"qQWcuw=="}`, "not both"},
		{"a number arg", `{"to":"` + usdc + `","call":{"signature":"transfer(address,uint256)","args":["` + alice + `",100000]}}`, "as a string"},
		{"a nested number arg", `{"to":"` + usdc + `","call":{"signature":"f(uint256[])","args":[["1",2]]}}`, "as a string"},
		{"a bad checksum", `{"to":"` + strings.Replace(usdc, "A0b8", "a0B8", 1) + `"}`, "EIP-55"},
		{"a 19-byte to", `{"to":"` + usdc[:40] + `"}`, "20 bytes, got 19"},
		{"a 19-byte base64 to", `{"to":"AAAAAAAAAAAAAAAAAAAAAAAAAA=="}`, "20 bytes, got 19"},
		{"bad hex", `{"to":"` + usdc + `","data":"0xzz"}`, "not valid 0x hex"},
		{"a wrong arity", `{"to":"` + usdc + `","call":{"signature":"transfer(address,uint256)","args":["1"]}}`, "arguments"},
	} {
		_, err := buildEth(t, c.intent)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: err = %v, want ErrInvalid saying %q", c.name, err, c.says)
		}
	}
}

// 0x hex is read as hex, so it gives the same intent as its base64; a
// single-case address carries no checksum to check.
func TestHexBytesReadAsTheirBase64(t *testing.T) {
	asB64, err := Build(Spec{Reason: "r", Transactions: []TxSpec{{Network: "eth", Ethereum: json.RawMessage(`{"to":"oLhpkcYhizbB0Z1KLp6wzjYG60g=","data":"qQWcuw=="}`)}}}, nets)
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{usdc, strings.ToLower(usdc), "0x" + strings.ToUpper(usdc[2:]), "0X" + strings.ToLower(usdc[2:])} {
		asHex, err := Build(Spec{Reason: "r", Transactions: []TxSpec{{Network: "eth", Ethereum: json.RawMessage(`{"to":"` + to + `","data":"0xa9059cbb"}`)}}}, nets)
		if err != nil {
			t.Fatalf("%s: %v", to, err)
		}
		if !proto.Equal(asHex, asB64) {
			t.Errorf("%s gives %v, want %v", to, asHex, asB64)
		}
	}
}
