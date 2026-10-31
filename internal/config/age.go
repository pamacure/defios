package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Age is how old talk must be for Pruning to destroy its payload (ADR
// 0024): a whole number of days, "90d", or a Go duration, "36h". Zero is
// no age, an archival Node's prune policy.
type Age time.Duration

// ParseAge reads an Age, which must be positive.
func ParseAge(s string) (Age, error) {
	var d time.Duration
	var err error
	if n, ok := strings.CutSuffix(s, "d"); ok && n != "" && strings.Trim(n, "0123456789") == "" {
		days, err := strconv.ParseInt(n, 10, 64)
		if err != nil || days > math.MaxInt64/int64(24*time.Hour) {
			return 0, fmt.Errorf("age %q is too long", s)
		}
		d = time.Duration(days) * 24 * time.Hour
	} else if d, err = time.ParseDuration(s); err != nil {
		return 0, fmt.Errorf("age %q is neither whole days like \"90d\" nor a Go duration like \"36h\"", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("age %q must be positive", s)
	}
	return Age(d), nil
}

// String is a in whole days if it is that, otherwise as a Go duration
// without its trailing zero units: "36h", not "36h0m0s".
func (a Age) String() string {
	d := time.Duration(a)
	if d != 0 && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// MarshalText writes a as ParseAge reads it.
func (a Age) MarshalText() ([]byte, error) { return []byte(a.String()), nil }

// UnmarshalText reads an Age as ParseAge does.
func (a *Age) UnmarshalText(b []byte) error {
	v, err := ParseAge(string(b))
	if err != nil {
		return err
	}
	*a = v
	return nil
}
