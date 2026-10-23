package signreq

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	coslv1 "defios.com/proto/cosl/v1"
)

// Vectors were produced independently with foundry's `cast to-rlp` + `cast keccak`
// over 0x02 || rlp([chainId, nonce, tip, maxFee, gas, to, value, data, []]).
const max256 = "115792089237316195423570985008687907853269984665640564039457584007913129639935"

func TestEthereumDigestVectors(t *testing.T) {
	to := bytes.Repeat([]byte{0x11}, 20)
	cases := []struct {
		name    string
		chainID uint64
		in      *coslv1.EthereumIntent
		unsign  string
		digest  string
	}{
		{"calldata", 1, &coslv1.EthereumIntent{
			To: to, Value: "1000", Data: []byte{0xde, 0xad, 0xbe, 0xef}, Nonce: 7, GasLimit: 21000,
			MaxFeePerGas: "30000000000", MaxPriorityFeePerGas: "1",
		}, "02ea0107018506fc23ac008252089411111111111111111111111111111111111111118203e884deadbeefc0",
			"8db393826c8c42dae799ad59f713a3d67410bc1ce6428e7e5fadc4dd7ed61415"},
		{"zero value, tip and data", 11155111, &coslv1.EthereumIntent{
			To: to, GasLimit: 21000, MaxFeePerGas: "2000000000",
		}, "02e683aa36a7808084773594008252089411111111111111111111111111111111111111118080c0",
			"79e07b55fb354f74796d75fd14aa84664f9df97832b81b4d32118451c150641c"},
		{"long calldata", 1, &coslv1.EthereumIntent{
			To: to, GasLimit: 21000, MaxFeePerGas: "1000000000", Data: bytes.Repeat([]byte{0xab}, 100),
		}, "", "098d39e9bbb78224a9b2259b715d249d909645f0b00dd28bce58529c69999072"},
		{"maximal values, single-byte data", 1<<64 - 1, &coslv1.EthereumIntent{
			To: to, Value: max256, Data: []byte{0x7f}, Nonce: 1<<64 - 1, GasLimit: 1<<64 - 1, MaxFeePerGas: max256,
		}, "02f87588ffffffffffffffff88ffffffffffffffff80a0" + strings.Repeat("ff", 32) + "88ffffffffffffffff94" + strings.Repeat("11", 20) + "a0" + strings.Repeat("ff", 32) + "7fc0",
			"9415a8f87183f8e3317d1dde777ee2ee2c942763558b4dfa8490cf7258a7cdaf"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			unsigned, digest, err := EthereumDigest(c.chainID, c.in)
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(unsigned); c.unsign != "" && got != c.unsign {
				t.Errorf("unsigned tx\n got %s\nwant %s", got, c.unsign)
			}
			if got := hex.EncodeToString(digest[:]); got != c.digest {
				t.Errorf("digest\n got %s\nwant %s", got, c.digest)
			}
		})
	}
}

func TestEthereumDigestRejectsBadAmount(t *testing.T) {
	_, _, err := EthereumDigest(1, &coslv1.EthereumIntent{To: make([]byte, 20), Value: "x", GasLimit: 1, MaxFeePerGas: "1"})
	if err == nil {
		t.Fatal("expected error")
	}
}
