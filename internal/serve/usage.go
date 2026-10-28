package serve

import (
	"context"
	"net/http"
	"os"
	"runtime"
	"time"

	"defios.com/internal/procstat"
)

// Usage is what serve reports about its own process for `node usage`.
// CLI verbs are short-lived clients of this socket, so serve's memory is
// the Node's. Durations are nanoseconds and sizes bytes; a field the
// platform cannot provide is omitted, not zero.
type Usage struct {
	Alias      string         `json:"alias"` // the Group this serve serves
	PID        int            `json:"pid"`
	Uptime     time.Duration  `json:"uptime_ns"`
	UserCPU    *time.Duration `json:"user_cpu_ns,omitempty"`
	SystemCPU  *time.Duration `json:"system_cpu_ns,omitempty"`
	Goroutines int            `json:"goroutines"`
	HeapInUse  uint64         `json:"heap_in_use_bytes"`
	Sys        uint64         `json:"go_sys_bytes"` // everything the Go runtime obtained from the OS
	NumGC      uint32         `json:"gc_count"`
	RSS        *uint64        `json:"rss_bytes,omitempty"`
	PeakRSS    *uint64        `json:"peak_rss_bytes,omitempty"`
}

func (s *server) usageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/usage", func(w http.ResponseWriter, r *http.Request) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		ps := procstat.Self()
		writeJSON(w, Usage{
			Alias:      s.opts.Alias,
			PID:        os.Getpid(),
			Uptime:     time.Since(s.started),
			UserCPU:    ps.UserCPU,
			SystemCPU:  ps.SystemCPU,
			Goroutines: runtime.NumGoroutine(),
			HeapInUse:  ms.HeapInuse,
			Sys:        ms.Sys,
			NumGC:      ms.NumGC,
			RSS:        ps.RSS,
			PeakRSS:    ps.PeakRSS,
		})
	})
}

// Usage asks the running serve what its process is using.
func (c Client) Usage(ctx context.Context) (Usage, error) {
	var u Usage
	err := c.do(ctx, "/v1/usage", nil, &u)
	return u, err
}
