package bundler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type latency struct {
	bucket [7]atomic.Uint64
	micros atomic.Uint64
}

var limits = [6]int64{1000, 5000, 10000, 50000, 250000, 1000000}

func (h *latency) observe(d time.Duration) {
	us := max(d.Microseconds(), 0)
	h.micros.Add(uint64(us))
	i := 0
	for i < len(limits) && us > limits[i] {
		i++
	}
	h.bucket[i].Add(1)
}
func (h *latency) write(b *strings.Builder, name string) {
	var count uint64
	for i := range h.bucket {
		count += h.bucket[i].Load()
		le := "+Inf"
		if i < len(limits) {
			le = fmt.Sprintf("%g", float64(limits[i])/1e6)
		}
		fmt.Fprintf(b, "%s_bucket{le=%q} %d\n", name, le, count)
	}
	fmt.Fprintf(b, "%s_sum %g\n%s_count %d\n", name, float64(h.micros.Load())/1e6, name, count)
}

type metrics struct {
	admit, lookup latency
	workerErrors  atomic.Uint64
}

func (s *Service) metricsHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	rows, e := s.Store.db.QueryContext(ctx, "SELECT stage,count(*) FROM operations GROUP BY stage")
	if e != nil {
		http.Error(w, "storage unavailable", 503)
		return
	}
	var out strings.Builder
	for rows.Next() {
		var stage string
		var n int
		if e = rows.Scan(&stage, &n); e != nil {
			break
		}
		fmt.Fprintf(&out, "bundler_operations{state=%q} %d\n", stage, n)
	}
	err := rows.Err()
	rows.Close()
	if e != nil || err != nil {
		http.Error(w, "storage unavailable", 503)
		return
	}
	var oldest int64
	if e = s.Store.db.QueryRowContext(ctx, "SELECT COALESCE(min(queued),$1) FROM operations WHERE stage='QUEUED'", now()).Scan(&oldest); e != nil {
		http.Error(w, "storage unavailable", 503)
		return
	}
	height, _, e := s.cursor(ctx)
	if e != nil {
		http.Error(w, "storage unavailable", 503)
		return
	}
	fmt.Fprintf(&out, "bundler_oldest_queued_seconds %g\nbundler_index_height %d\nbundler_worker_errors_total %d\n", float64(max(now()-oldest, 0))/1000, height, s.stats.workerErrors.Load())
	s.stats.admit.write(&out, "bundler_admission_seconds")
	s.stats.lookup.write(&out, "bundler_receipt_seconds")
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprint(w, out.String())
}
