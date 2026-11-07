package trigger_test

import (
	"testing"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/trigger"
)

// A Schedule's firing is due at a check that comes within slack of it,
// once: a firing the serve was not there to see, or saw at an earlier
// check, is never made up (ADR 0032).
func TestAFiringIsDueOnceAndOnlyLive(t *testing.T) {
	everyHour := config.Schedule{Cron: "0 * * * *", TZ: "UTC", Jobs: []string{"j"}}
	at := func(h, m, s int) time.Time { return time.Date(2026, 11, 7, h, m, s, 0, time.UTC) }
	const slack = time.Minute
	for _, c := range []struct {
		name      string
		last, now time.Time
		want      time.Time // zero: nothing due
	}{
		{"before it", at(9, 58, 0), at(9, 59, 0), time.Time{}},
		{"on time", at(9, 59, 0), at(10, 0, 0), at(10, 0, 0)},
		{"a moment late", at(9, 59, 0), at(10, 0, 2), at(10, 0, 0)},
		{"seen at the last check", at(10, 0, 0), at(10, 0, 30), time.Time{}},
		{"missed while not serving", at(9, 0, 30), at(10, 5, 0), time.Time{}},
		// Back after hours away, only a firing within slack is live.
		{"back just after one", at(6, 0, 30), at(10, 0, 20), at(10, 0, 0)},
	} {
		got, ok := trigger.Due(everyHour, c.last, c.now, slack)
		if ok != !c.want.IsZero() || !got.Equal(c.want) {
			t.Errorf("%s: due %v at %s, want %s", c.name, ok, got, c.want)
		}
	}
	once := config.Schedule{At: "2026-11-07T10:00:00Z", Jobs: []string{"j"}}
	if got, ok := trigger.Due(once, at(9, 59, 30), at(10, 0, 0), slack); !ok || !got.Equal(at(10, 0, 0)) {
		t.Errorf("at: due %v at %s, want 10:00", ok, got)
	}
	if _, ok := trigger.Due(once, at(10, 0, 0), at(10, 0, 30), slack); ok {
		t.Error("at: due again after it fired")
	}
}

// Several firings within slack of one check run once, as the latest.
func TestFiringsBunchedAtOneCheckRunOnce(t *testing.T) {
	everyMinute := config.Schedule{Cron: "* * * * *", TZ: "UTC", Jobs: []string{"j"}}
	last := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	got, ok := trigger.Due(everyMinute, last, last.Add(150*time.Second), 3*time.Minute)
	if want := last.Add(2 * time.Minute); !ok || !got.Equal(want) {
		t.Errorf("due %v at %s, want once at %s", ok, got, want)
	}
}
