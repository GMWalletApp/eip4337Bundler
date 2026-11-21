package bundler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

type Service struct {
	stats       metrics
	Config      Config
	Store       *Store
	Chain       Chain
	Validator   Validator
	Signer      Signer
	Broadcast   Broadcaster
	wake        chan struct{}
	simulations chan struct{}
}

func New(c Config, store *Store, chain Chain, v Validator, s Signer, b Broadcaster) (*Service, error) {
	if store == nil || chain == nil || v == nil || s == nil || b == nil || c.ChainID == nil || c.ChainID.Sign() <= 0 || c.ChainID.BitLen() > 256 || c.EntryPoint == (common.Address{}) || c.Sender == (common.Address{}) || c.Beneficiary == (common.Address{}) || c.MaxFee == nil || c.MaxFee.Sign() <= 0 || c.MaxGas < 21000 || c.MaxGas > 1<<48 || c.MaxOps < 1 || c.MaxOps > 256 || c.MaxBytes < 512 || c.MaxBytes > 4<<20 || c.PoolSize < c.MaxOps || c.Poll < 50*time.Millisecond || c.Confirmations < 1 || c.Confirmations > 4096 || c.StartBlock > 1<<60 {
		return nil, errors.New("invalid bundler configuration")
	}
	return &Service{Config: c, Store: store, Chain: chain, Validator: v, Signer: s, Broadcast: b, wake: make(chan struct{}, 1), simulations: make(chan struct{}, 16)}, nil
}
func (s *Service) validate(ctx context.Context, o Operation, h Header) (Admission, error) {
	t, e := s.Validator.Validate(ctx, o, s.Config.EntryPoint, h.Hash)
	if e != nil {
		return t, e
	}
	if t.Block != h.Hash || t.Prefund == nil || t.Prefund.ToInt().Sign() <= 0 || o.Paymaster != nil && t.Deposit == nil {
		return t, errors.New("validator missing block or funding evidence")
	}
	if t.ValidAfter > uint64(h.Time) || t.ValidUntil != 0 && t.ValidUntil < uint64(h.Time)+15 {
		return t, &RPCError{Code: -32503, Message: "operation validity window excludes proposed inclusion"}
	}
	return t, nil
}
func (s *Service) admit(ctx context.Context, o Operation) (string, error) {
	h := o.Hash(s.Config.EntryPoint, s.Config.ChainID).Hex()
	// Only byte-identical accepted requests can skip signature validation; signatures are excluded from the operation hash.
	body, e := json.Marshal(o)
	if e != nil {
		return "", e
	}
	var stored, stage string
	err := s.Store.db.QueryRowContext(ctx, "SELECT body,stage FROM operations WHERE hash=$1", h).Scan(&stored, &stage)
	if err == nil && stored == string(body) {
		return h, duplicateStatus(stage)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	head, e := s.Chain.Header(ctx, "latest")
	if e != nil {
		return "", e
	}
	t, e := s.validate(ctx, o, head)
	if e != nil {
		return "", e
	}
	return h, s.Store.admit(ctx, h, o, t, s.Config.PoolSize)
}
func (s *Service) cycle(ctx context.Context, name string, fn func(context.Context, lease) error) error {
	l, got, e := s.Store.claim(ctx, name)
	if e != nil || !got {
		return e
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		s.Store.release(c, l)
	}()
	step, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	return fn(step, l)
}
func (s *Service) Run(ctx context.Context) {
	var group sync.WaitGroup
	jobs := map[string]func(context.Context, lease) error{"build": s.build, "delivery": s.deliver, "indexer": s.index}
	for name, fn := range jobs {
		group.Go(func() {
			ticker := time.NewTicker(s.Config.Poll)
			defer ticker.Stop()
			var wake <-chan struct{}
			if name == "delivery" {
				wake = s.wake
			}
			for ctx.Err() == nil {
				if e := s.cycle(ctx, name, fn); e != nil && ctx.Err() == nil && !errors.Is(e, errFenced) {
					s.stats.workerErrors.Add(1)
					slog.Warn("worker retry", "worker", name, "error", e)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				case <-wake:
				}
			}
		})
	}
	group.Wait()
}
func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Service) deferOp(ctx context.Context, l lease, r record, stage, reason string, delay time.Duration) error {
	return s.Store.locked(ctx, l, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "UPDATE operations SET stage=$1,reason=$2,due=$3 WHERE hash=$4 AND stage='QUEUED'", stage, reason, now()+delay.Milliseconds(), r.hash)
		return e
	})
}
func (s *Service) build(ctx context.Context, l lease) error {
	c := s.Config
	rows, e := s.Store.candidates(ctx, c.MaxOps)
	if e != nil || len(rows) == 0 {
		return e
	}
	head, e := s.Chain.Header(ctx, "latest")
	if e != nil {
		return e
	}
	if e = s.caughtUp(ctx, head); e != nil {
		return e
	}
	if e = s.Store.locked(ctx, l, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE operations SET stage='EXPIRED',reason='VALIDITY_EXPIRED' WHERE stage='QUEUED' AND valid_until>0 AND valid_until<=$1", uint64(head.Time))
		return err
	}); e != nil {
		return e
	}
	if head.BaseFee == nil {
		return errors.New("EIP-1559 block required")
	}
	tip, e := s.Chain.Price(ctx)
	if e != nil {
		return e
	}
	fee := new(big.Int).Add(head.BaseFee.ToInt(), tip)
	if fee.Cmp(c.MaxFee) > 0 {
		return errors.New("network fee above signer budget")
	}
	var batch []record
	var ops []Operation
	var total uint64
	gasLimit := min(c.MaxGas, uint64(head.GasLimit))
	var data []byte
	checks := make([]error, len(rows))
	jobs := make(chan int, len(rows))
	for i := range rows {
		jobs <- i
	}
	close(jobs)
	var checksDone sync.WaitGroup
	for range min(4, len(rows)) {
		checksDone.Go(func() {
			for i := range jobs {
				o := rows[i].op
				if o.MaxFeePerGas.ToInt().Cmp(fee) >= 0 && o.MaxPriorityFeePerGas.ToInt().Cmp(tip) >= 0 {
					_, checks[i] = s.validate(ctx, o, head)
				}
			}
		})
	}
	checksDone.Wait()
	for i, r := range rows {
		if r.op.MaxFeePerGas.ToInt().Cmp(fee) < 0 || r.op.MaxPriorityFeePerGas.ToInt().Cmp(tip) < 0 {
			if e = s.deferOp(ctx, l, r, "QUEUED", "FEE_TOO_LOW", 5*time.Second); e != nil {
				return e
			}
			continue
		}
		if e = checks[i]; e != nil {
			var fault *RPCError
			if !errors.As(e, &fault) {
				// A slow or unavailable validation backend must not discard the other results.
				if err := s.deferOp(ctx, l, r, "QUEUED", "VALIDATION_UNAVAILABLE", 3*time.Second); err != nil {
					return err
				}
				s.stats.workerErrors.Add(1)
				continue
			}
			if e = s.deferOp(ctx, l, r, "REJECTED", fault.Message, 0); e != nil {
				return e
			}
			continue
		}
		budget := new(big.Int).Set(r.op.PreVerificationGas.ToInt())
		for _, n := range []*hexutil.Big{r.op.CallGasLimit, r.op.VerificationGasLimit, r.op.PaymasterVerificationGasLimit, r.op.PaymasterPostOpGasLimit} {
			if n != nil {
				budget.Add(budget, n.ToInt())
			}
		}
		candidate, e := calldata(append(ops, r.op), c.Beneficiary)
		if e != nil {
			return e
		}
		if !budget.IsUint64() || budget.Uint64() > gasLimit || len(candidate) > c.MaxBytes || budget.Uint64() > gasLimit-total {
			if len(ops) == 0 {
				if e = s.deferOp(ctx, l, r, "REJECTED", "BUNDLE_LIMIT", 0); e != nil {
					return e
				}
				continue
			}
			break
		}
		total += budget.Uint64()
		batch = append(batch, r)
		ops = append(ops, r.op)
		data = candidate
	}
	if len(batch) == 0 {
		return nil
	}
	nonce, e := s.Broadcast.Nonce(ctx, c.Sender)
	if e != nil {
		return e
	}
	// Several signed bundles can be waiting for a Broadcaster acknowledgement.
	// Keep the adviser's lowest free nonce unless it is already reserved locally.
	for {
		var count int
		e = s.Store.db.QueryRowContext(ctx, "SELECT count(*) FROM bundles WHERE nonce=$1 AND stage IN ('SIGNED','SUBMITTED')", fmt.Sprint(nonce)).Scan(&count)
		if e != nil {
			return e
		}
		if count == 0 {
			break
		}
		if nonce == ^uint64(0) {
			return errors.New("nonce exhausted")
		}
		nonce++
	}
	args := map[string]any{"from": c.Sender, "to": c.EntryPoint, "data": hexutil.Bytes(data), "gas": hexutil.Uint64(gasLimit), "maxFeePerGas": (*hexutil.Big)(fee), "maxPriorityFeePerGas": (*hexutil.Big)(tip)}
	gas, e := s.Chain.Simulate(ctx, head, args)
	if e != nil {
		var failed *failedOp
		if errors.As(e, &failed) && failed.Index >= 0 && failed.Index < len(batch) {
			return s.deferOp(ctx, l, batch[failed.Index], "QUEUED", failed.Reason, 10*time.Second)
		}
		return e
	}
	if gas == 0 || gas > gasLimit {
		return errors.New("invalid bundle gas estimate")
	}
	gas = min(gasLimit, gas+gas/10)
	// After a reorg or an expired outer task, the same nonce and calldata
	// may be eligible again. Give a fresh submission a distinct hash so
	// Broadcaster cannot return an already-closed idempotent acknowledgement.
	var priorCount uint64
	if e = s.Store.db.QueryRowContext(ctx, "SELECT count(*) FROM bundles WHERE nonce=$1", fmt.Sprint(nonce)).Scan(&priorCount); e != nil {
		return e
	}
	var hash, raw string
	for offset := uint64(0); offset <= priorCount; offset++ {
		if offset > gasLimit-gas {
			return errors.New("no distinct gas budget available for recovered bundle")
		}
		unsigned := types.NewTx(&types.DynamicFeeTx{ChainID: c.ChainID, Nonce: nonce, Gas: gas + offset, GasFeeCap: fee, GasTipCap: tip, To: &c.EntryPoint, Data: data})
		signed, encoded, err := s.sign(ctx, unsigned)
		if err != nil {
			return err
		}
		hash, raw = signed.Hash().Hex(), encoded
		var known int
		if err = s.Store.db.QueryRowContext(ctx, "SELECT count(*) FROM bundles WHERE hash=$1", hash).Scan(&known); err != nil {
			return err
		}
		if known == 0 {
			break
		}
	}
	e = s.Store.locked(ctx, l, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, "INSERT INTO bundles(hash,raw,nonce,stage,due) VALUES($1,$2,$3,'SIGNED',$4)", hash, raw, fmt.Sprint(nonce), now()); e != nil {
			return e
		}
		for _, r := range batch {
			if e := changed(tx.ExecContext(ctx, "UPDATE operations SET stage='BUNDLED',reason='',bundle=$1 WHERE hash=$2 AND stage='QUEUED'", hash, r.hash)); e != nil {
				return e
			}
			if _, e := tx.ExecContext(ctx, "INSERT INTO members(bundle,op) VALUES($1,$2)", hash, r.hash); e != nil {
				return e
			}
		}
		return nil
	})
	if e == nil {
		s.signal()
	}
	return e
}
func (s *Service) deliver(ctx context.Context, l lease) error {
	var hash, raw, stage string
	e := s.Store.db.QueryRowContext(ctx, "SELECT hash,raw,stage FROM bundles WHERE stage IN ('SIGNED','SUBMITTED') AND due<=$1 ORDER BY due,hash LIMIT 1", now()).Scan(&hash, &raw, &stage)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	// Advance the retry clock before I/O so a failing request cannot starve other bundles.
	e = s.Store.locked(ctx, l, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "UPDATE bundles SET due=$1 WHERE hash=$2", now()+3000, hash)
		return e
	})
	if e != nil {
		return e
	}
	h := common.HexToHash(hash)
	view, exists, e := s.Broadcast.Lookup(ctx, h)
	if e != nil {
		return e
	}
	if !exists {
		view, e = s.Broadcast.Submit(ctx, raw, h)
		if e != nil {
			var be *BroadcastError
			if errors.As(e, &be) && be.Status == 409 && (be.Code == "NONCE_CONFLICT" || be.Code == "NONCE_CONSUMED") {
				return s.releaseBundle(ctx, l, hash, "CONFLICT", be.Code)
			}
			return e
		}
	}
	if view.State != "CLOSED" && view.Reason == "REPLACEMENT_REQUIRED" {
		return s.replaceOuter(ctx, l, hash, raw, view)
	}
	if view.State == "CLOSED" && view.Outcome != "INCLUDED" {
		return s.releaseBundle(ctx, l, hash, "CLOSED", view.Reason)
	}
	encoded, e := json.Marshal(view)
	if e != nil {
		return e
	}
	return s.Store.locked(ctx, l, func(tx *sql.Tx) error {
		next := "SUBMITTED"
		if view.Outcome == "INCLUDED" && view.State == "CLOSED" {
			next = "MINED"
		}
		if _, e := tx.ExecContext(ctx, "UPDATE bundles SET stage=$1,task=$2,observation=$3 WHERE hash=$4", next, view.Task, string(encoded), hash); e != nil {
			return e
		}
		if next == "MINED" { // The indexer may already have advanced past this transaction.
			_, e := tx.ExecContext(ctx, "UPDATE operations SET stage='QUEUED',bundle=NULL,due=$1,reason='REVALIDATE_AFTER_INCLUSION' WHERE bundle=$2 AND stage='BUNDLED'", now()+3000, hash)
			return e
		}
		return nil
	})
}
func (s *Service) releaseBundle(ctx context.Context, l lease, hash, stage, reason string) error {
	return s.Store.locked(ctx, l, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, "UPDATE bundles SET stage=$1,observation=$2 WHERE hash=$3", stage, reason, hash); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "UPDATE operations SET stage='QUEUED',bundle=NULL,reason=$1,due=$2 WHERE bundle=$3 AND stage='BUNDLED'", reason, now()+3000, hash)
		return e
	})
}
