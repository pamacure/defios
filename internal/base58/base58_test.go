package base58_test

import (
	"bytes"
	"encoding/hex"
	"testing"

	"defios.com/internal/base58"
)

func TestRoundTripsKnownVectors(t *testing.T) {
	for _, c := range []struct{ hex, b58 string }{
		{"", ""},
		{"00", "1"},
		{"0000", "11"},
		{"61", "2g"},
		{"626262", "a3gV"},
		{"00eb15231dfceb60925886b67d065299925915aeb172c06647", "1NS17iag9jJgTHD1VXjvLCEnZuQ3rJDE9L"},
		{"0000000000000000000000000000000000000000000000000000000000000000", "11111111111111111111111111111111"},
	} {
		raw, _ := hex.DecodeString(c.hex)
		if got := base58.Encode(raw); got != c.b58 {
			t.Errorf("Encode(%s) = %q, want %q", c.hex, got, c.b58)
		}
		back, err := base58.Decode(c.b58)
		if err != nil || !bytes.Equal(back, raw) {
			t.Errorf("Decode(%q) = %x, %v", c.b58, back, err)
		}
	}
}

func TestDecodeRejectsBadInput(t *testing.T) {
	if _, err := base58.Decode("0OIl"); err == nil {
		t.Error("characters outside the alphabet decoded")
	}
	if _, err := base58.DecodeN("2g", 32); err == nil {
		t.Error("wrong length decoded")
	}
}
