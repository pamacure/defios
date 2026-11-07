package cron_test

import (
	"testing"
	"time"

	"defios.com/internal/cron"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// A five-field spec fires at the next minute its fields all allow, read in
// its zone.
func TestNextIsTheNextMatchingMinute(t *testing.T) {
	paris := mustLoad(t, "Europe/Paris")
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, paris)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct{ spec, after, want string }{
		{"0 9 * * 1-5", "2026-11-06 09:00", "2026-11-09 09:00"}, // Friday 9:00 → Monday
		{"0 9 * * 1-5", "2026-11-06 08:59", "2026-11-06 09:00"},
		{"*/15 * * * *", "2026-11-06 10:07", "2026-11-06 10:15"},
		{"30 23 31 * *", "2026-11-01 00:00", "2026-12-31 23:30"}, // November has no 31st
		{"0 0 29 2 *", "2026-03-01 00:00", "2028-02-29 00:00"},
		{"0 12 * jan,JUL *", "2026-08-01 00:00", "2027-01-01 12:00"},
		{"0 8 * * sun", "2026-11-06 00:00", "2026-11-08 08:00"},
		{"0 8 * * 7", "2026-11-06 00:00", "2026-11-08 08:00"}, // 7 is Sunday too
		{"5-10/5 1 * * *", "2026-11-06 01:05", "2026-11-06 01:10"},
		// Day of month and day of week both set: either matches.
		{"0 0 10 * 5", "2026-11-07 00:00", "2026-11-10 00:00"},
		{"0 0 1 * 1", "2026-11-07 00:00", "2026-11-09 00:00"},
		// A day field starting "*" is unset, as in classic cron: odd-dated Mondays.
		{"0 0 */2 * 1", "2026-11-01 00:00", "2026-11-09 00:00"},
		// A time the clocks skip does not happen; 2:30 on 29 March 2026 in Paris.
		{"30 2 * * *", "2026-03-28 03:00", "2026-03-30 02:30"},
	} {
		s, err := cron.Parse(c.spec)
		if err != nil {
			t.Fatalf("%q: %v", c.spec, err)
		}
		got := s.Next(at(c.after), paris)
		if want := at(c.want); !got.Equal(want) {
			t.Errorf("%q after %s: %s, want %s", c.spec, c.after, got.In(paris), want)
		}
	}
}

// A time the clocks pass twice fires once.
func TestNextFiresOnceInARepeatedHour(t *testing.T) {
	paris := mustLoad(t, "Europe/Paris")
	s, err := cron.Parse("30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// 25 October 2026: 3:00 CEST goes back to 2:00 CET.
	first := s.Next(time.Date(2026, 10, 25, 0, 0, 0, 0, paris), paris)
	if want := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC); !first.Equal(want) {
		t.Fatalf("first %s, want %s", first.UTC(), want)
	}
	if next := s.Next(first, paris); !next.Equal(time.Date(2026, 10, 26, 1, 30, 0, 0, time.UTC)) {
		t.Errorf("after the first, %s, want the next day's", next.UTC())
	}
}

// A spec that can never fire has no next firing.
func TestNextIsZeroForASpecThatNeverFires(t *testing.T) {
	s, err := cron.Parse("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Next(time.Now(), time.UTC); !got.IsZero() {
		t.Errorf("next %s, want none", got)
	}
}

func TestParseRefusesMalformedSpecs(t *testing.T) {
	for _, spec := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *",
		"* * 32 * *", "* * * 13 * ", "* * * * 8", "5-1 * * * *", "*/0 * * * *",
		"a * * * *", "1,,2 * * * *", "* * * foo *", "@daily",
	} {
		if _, err := cron.Parse(spec); err == nil {
			t.Errorf("%q parsed", spec)
		}
	}
}
