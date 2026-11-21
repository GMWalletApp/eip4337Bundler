package bundler

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
)

// Optional local burst profile. Reports each stage separately so fast receipt
// queries cannot inflate the measured rate of validated submissions.
func profile(t *testing.T, s *Service, url, token string, create func(int64, bool) Operation) {
	t.Helper()
	const count = 48
	const concurrency = 6
	ops := make([]Operation, count)
	for i := range ops {
		ops[i] = create(int64(100+i), false)
	}
	client, e := rpc.DialOptions(t.Context(), url+"/rpc", rpc.WithHeader("Authorization", "Bearer "+token))
	requireOK(t, e)
	defer client.Close()
	measure := func(name string, fn func(int) error) {
		jobs := make(chan int, count)
		for i := range count {
			jobs <- i
		}
		close(jobs)
		lat := make([]time.Duration, count)
		errs := make([]error, count)
		var workers sync.WaitGroup
		start := time.Now()
		for range concurrency {
			workers.Go(func() {
				for i := range jobs {
					begin := time.Now()
					errs[i] = fn(i)
					lat[i] = time.Since(begin)
				}
			})
		}
		workers.Wait()
		elapsed := time.Since(start)
		for _, e := range errs {
			requireOK(t, e)
		}
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		t.Logf("%s: %d ops, concurrency %d, %.2f ops/s, p50=%s p95=%s p99=%s", name, count, concurrency, count/elapsed.Seconds(), lat[23], lat[45], lat[47])
	}
	measure("durable admission", func(i int) error {
		var h common.Hash
		e := client.CallContext(t.Context(), &h, "eth_sendUserOperation", ops[i], s.Config.EntryPoint)
		if e == nil && h != ops[i].Hash(s.Config.EntryPoint, s.Config.ChainID) {
			return fmt.Errorf("admission hash mismatch")
		}
		return e
	})
	head, e := s.Chain.Header(t.Context(), "latest")
	requireOK(t, e)
	for i := 0; i < 10 && s.caughtUp(t.Context(), head) != nil; i++ {
		requireOK(t, s.cycle(t.Context(), "indexer", s.index))
	}
	start := time.Now()
	for i := 0; i < 160; i++ {
		requireOK(t, s.cycle(t.Context(), "indexer", s.index))
		requireOK(t, s.cycle(t.Context(), "build", s.build))
		requireOK(t, s.cycle(t.Context(), "delivery", s.deliver))
		var n int
		requireOK(t, s.Store.db.QueryRow("SELECT count(*) FROM operations WHERE stage IN ('INCLUDED','FINAL')").Scan(&n))
		if n >= count+2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("drain %d queued operations through broadcast and indexing: %s", count, time.Since(start))
	measure("canonical receipt", func(i int) error {
		var r *OpReceipt
		e := client.CallContext(t.Context(), &r, "eth_getUserOperationReceipt", ops[i].Hash(s.Config.EntryPoint, s.Config.ChainID))
		if e == nil && (r == nil || !r.Success) {
			return fmt.Errorf("missing successful receipt")
		}
		return e
	})
}
