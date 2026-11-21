package bundler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/rpc"
)

type Caller interface {
	CallContext(context.Context, any, string, ...any) error
}

type reader struct {
	caller  Caller
	mu      sync.Mutex
	checked time.Time
}
type ReadPool struct {
	config Config
	nodes  []*reader
	active atomic.Uint64
}

func NewReadPool(c Config, clients []Caller) *ReadPool {
	p := &ReadPool{config: c}
	for _, client := range clients {
		p.nodes = append(p.nodes, &reader{caller: client})
	}
	return p
}
func (p *ReadPool) verify(ctx context.Context, n *reader) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if time.Since(n.checked) < time.Minute {
		return nil
	}
	if e := (EVM{n.caller}).Verify(ctx, p.config); e != nil {
		return e
	}
	n.checked = time.Now()
	return nil
}

// Only read and simulation calls reach this pool. Signed bytes take the
// separately authenticated Broadcaster path. A JSON-RPC revert is a result.
func (p *ReadPool) CallContext(ctx context.Context, out any, method string, args ...any) error {
	if len(p.nodes) == 0 {
		return errors.New("no execution readers configured")
	}
	start := p.active.Load()
	var last error
	for i := range p.nodes {
		n := p.nodes[(start+uint64(i))%uint64(len(p.nodes))]
		step, cancel := context.WithTimeout(ctx, 6*time.Second)
		e := p.verify(step, n)
		if e == nil {
			e = n.caller.CallContext(step, out, method, args...)
		}
		cancel()
		if e == nil {
			p.active.Store((start + uint64(i)) % uint64(len(p.nodes)))
			return nil
		}
		last = e
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var revert rpc.DataError
		if errors.As(e, &revert) && revert.ErrorData() != nil {
			return e
		}
	}
	return last
}
