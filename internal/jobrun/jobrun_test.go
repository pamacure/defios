package jobrun_test

import (
	"errors"
	"testing"
	"time"

	"defios.com/internal/jobrun"
	"defios.com/internal/trigger"
)

// firing is n Job Runs of one firing of Hook greeted, all pending.
func firing(jobs ...string) []jobrun.Run {
	var out []jobrun.Run
	for _, j := range jobs {
		out = append(out, jobrun.Run{Trigger: "greeted", Event: trigger.Event{Type: "app.freeform", Trigger: "greeted"}, Job: j, Kind: "exec", State: jobrun.Pending, Fired: time.Now()})
	}
	return out
}

// A firing's Job Runs are numbered in order from 1, each naming its
// firing by the first's ID, and are read back newest first.
func TestAFiringsJobRunsAreKeptInOrder(t *testing.T) {
	dir := t.TempDir()
	s, err := jobrun.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first := firing("a", "b")
	if err := s.Add(first); err != nil {
		t.Fatal(err)
	}
	if first[0].ID != 1 || first[1].ID != 2 || first[0].Firing != 1 || first[1].Firing != 1 {
		t.Fatalf("ids %+v", first)
	}
	second := firing("c")
	if err := s.Add(second); err != nil {
		t.Fatal(err)
	}
	second[0].State = jobrun.OK
	second[0].Output = trigger.Output{Exec: &trigger.ExecOutput{Stdout: "hi\n"}}
	if err := s.Put(second[0]); err != nil {
		t.Fatal(err)
	}
	runs, err := jobrun.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 || runs[0].ID != 3 || runs[2].ID != 1 {
		t.Fatalf("listed %d runs, want 3 newest first", len(runs))
	}
	got, err := jobrun.Get(dir, 3)
	if err != nil || got.State != jobrun.OK || got.Firing != 3 || got.Exec == nil || got.Exec.Stdout != "hi\n" {
		t.Fatalf("Get(3) = %+v, %v", got, err)
	}
	if _, err := jobrun.Get(dir, 9); !errors.Is(err, jobrun.ErrUnknown) {
		t.Errorf("Get(9): %v, want ErrUnknown", err)
	}
}

// Job Runs outlast the Store that wrote them: one opened again numbers on
// from the last, and ends what was left pending or running, as the serve
// that ran them stopped first.
func TestJobRunsOutlastTheirStore(t *testing.T) {
	dir := t.TempDir()
	s, _ := jobrun.Open(dir)
	runs := firing("a", "b", "c")
	s.Add(runs)
	runs[0].State = jobrun.OK
	runs[1].State = jobrun.Running
	s.Put(runs[0])
	s.Put(runs[1])

	s, err := jobrun.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := jobrun.List(dir)
	want := []string{jobrun.NotRun, jobrun.Failed, jobrun.OK} // newest first
	for i, r := range got {
		if r.State != want[i] {
			t.Errorf("run %d is %s, want %s", r.ID, r.State, want[i])
		}
	}
	if got[1].Error == "" {
		t.Error("the run cut short says not why")
	}
	next := firing("d")
	s.Add(next)
	if next[0].ID != 4 {
		t.Errorf("next id %d, want 4", next[0].ID)
	}
}

// A Group keeps its last 200 Job Runs, dropping the oldest first.
func TestAGroupKeepsItsLast200JobRuns(t *testing.T) {
	dir := t.TempDir()
	s, _ := jobrun.Open(dir)
	for range 205 {
		if err := s.Add(firing("a")); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := jobrun.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 200 || runs[0].ID != 205 || runs[199].ID != 6 {
		t.Fatalf("kept %d runs, %d to %d; want 200, 205 to 6", len(runs), runs[0].ID, runs[len(runs)-1].ID)
	}
}

// A Job Run dropped while its Job still ran stays dropped as it ends.
func TestADroppedJobRunStaysDropped(t *testing.T) {
	dir := t.TempDir()
	s, _ := jobrun.Open(dir)
	long := firing("long")
	s.Add(long)
	for range jobrun.Keep {
		s.Add(firing("a"))
	}
	long[0].State = jobrun.OK
	if err := s.Put(long[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := jobrun.Get(dir, long[0].ID); !errors.Is(err, jobrun.ErrUnknown) {
		t.Errorf("Get(%d): %v, want it dropped", long[0].ID, err)
	}
	if runs, _ := jobrun.List(dir); len(runs) != jobrun.Keep {
		t.Errorf("kept %d runs", len(runs))
	}
}
