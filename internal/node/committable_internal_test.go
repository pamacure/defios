package node

import (
	"testing"
	"time"
)

func TestWritesCommitOnlyWithAReachableMajorityAndALeaderOrAnElectionUnderway(t *testing.T) {
	const grace = 10 * time.Second
	for _, c := range []struct {
		name              string
		reachable, roster int
		leader            bool
		leaderless        time.Duration
		want              bool
	}{
		{"leader present", 2, 2, true, 0, true},
		{"no leader inside the grace period", 2, 2, false, 3 * time.Second, true},
		{"no leader past the grace period", 2, 2, false, 11 * time.Second, false},
		{"no leader for exactly the grace period", 2, 2, false, grace, false},
		{"leader regained after a long gap", 3, 3, true, 0, true},
		{"no reachable majority, leader known", 1, 3, true, 0, false},
		{"no reachable majority, no leader", 1, 2, false, time.Second, false},
		{"no Roster", 0, 0, true, 0, false},
	} {
		if got := writesCanCommit(c.reachable, c.roster, c.leader, c.leaderless, grace); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTheLeaderlessClockRunsFromServingOrALostLeaderAndResetsWithOne(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	c := leaderClock{since: t0} // serving starts with no leader
	for _, step := range []struct {
		name   string
		at     int
		leader bool
		want   time.Duration
	}{
		{"electing since serving began", 3, false, 3 * time.Second},
		{"still electing", 12, false, 12 * time.Second},
		{"leader regained", 13, true, 0},
		{"leader lost", 20, false, 0},
		{"lost for a while", 26, false, 6 * time.Second},
		{"regained again", 27, true, 0},
	} {
		if got := c.observe(step.leader, at(step.at)); got != step.want {
			t.Errorf("%s: leaderless %v, want %v", step.name, got, step.want)
		}
	}
}
