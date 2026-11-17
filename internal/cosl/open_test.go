package cosl_test

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"defios.com/internal/cosl"
)

// Serve and a reader may open a new replica at once: each Open succeeds,
// whichever sets its triggers up first.
func TestConcurrentOpensOfANewReplicaAllSucceed(t *testing.T) {
	for i := range 20 {
		path := filepath.Join(t.TempDir(), fmt.Sprint("cosl", i, ".sqlite"))
		errs := make(chan error, 4)
		var wg sync.WaitGroup
		for range cap(errs) {
			wg.Go(func() {
				s, err := cosl.Open(path)
				if err == nil {
					err = s.Close()
				}
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("open %d: %v", i, err)
			}
		}
	}
}
