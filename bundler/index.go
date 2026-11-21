package bundler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

type OpReceipt struct {
	Hash       common.Hash     `json:"userOpHash"`
	EntryPoint common.Address  `json:"entryPoint"`
	Sender     common.Address  `json:"sender"`
	Nonce      *hexutil.Big    `json:"nonce"`
	Paymaster  *common.Address `json:"paymaster,omitempty"`
	Cost       *hexutil.Big    `json:"actualGasCost"`
	Used       *hexutil.Big    `json:"actualGasUsed"`
	Success    bool            `json:"success"`
	Reason     hexutil.Bytes   `json:"reason,omitempty"`
	Logs       []*types.Log    `json:"logs"`
	Receipt    *types.Receipt  `json:"receipt"`
}

func field(b []byte) *hexutil.Big { return (*hexutil.Big)(new(big.Int).SetBytes(b)) }
func decodeEvent(ep common.Address, event types.Log, r *types.Receipt) (OpReceipt, error) {
	var o OpReceipt
	if r == nil || r.BlockNumber == nil || r.Status != 1 || event.Address != ep || len(event.Topics) != 4 || event.Topics[0] != operationEvent || event.Removed || len(event.Data) != 128 || r.TxHash != event.TxHash || r.BlockHash != event.BlockHash || r.BlockNumber.Uint64() != event.BlockNumber {
		return o, errors.New("inconsistent UserOperationEvent receipt")
	}
	flag := new(big.Int).SetBytes(event.Data[32:64])
	if flag.BitLen() > 1 {
		return o, errors.New("invalid event success bit")
	}
	o = OpReceipt{Hash: event.Topics[1], EntryPoint: ep, Sender: common.BytesToAddress(event.Topics[2].Bytes()), Nonce: field(event.Data[:32]), Success: flag.Sign() == 1, Cost: field(event.Data[64:96]), Used: field(event.Data[96:]), Logs: []*types.Log{}, Receipt: r}
	if event.Topics[3] != (common.Hash{}) {
		a := common.BytesToAddress(event.Topics[3].Bytes())
		o.Paymaster = &a
	}
	// EntryPoint marks the start of execution and the end of each operation.
	// Only the logs between two boundaries belong in this operation's receipt.
	for _, log := range r.Logs {
		if log == nil || log.Removed {
			return o, errors.New("incomplete transaction logs")
		}
		if log.Address == ep && len(log.Topics) > 0 {
			switch log.Topics[0] {
			case operationEvent:
				if log.Index == event.Index && len(log.Topics) == 4 && log.Topics[1] == o.Hash && string(log.Data) == string(event.Data) {
					return o, nil
				}
				o.Logs = []*types.Log{}
				o.Reason = nil
				continue
			case beforeExecution:
				o.Logs = []*types.Log{}
				continue
			case revertEvent:
				if len(log.Topics) >= 2 && log.Topics[1] == o.Hash && len(log.Data) >= 96 && new(big.Int).SetBytes(log.Data[32:64]).Cmp(big.NewInt(64)) == 0 {
					n := new(big.Int).SetBytes(log.Data[64:96])
					if n.IsUint64() && n.Uint64() <= uint64(len(log.Data)-96) {
						o.Reason = log.Data[96 : 96+n.Uint64()]
					}
				}
			}
		}
		o.Logs = append(o.Logs, log)
	}
	return o, errors.New("filter log missing from receipt")
}
func (s *Service) cursor(ctx context.Context) (int64, common.Hash, error) {
	var n int64
	var h string
	e := s.Store.db.QueryRowContext(ctx, "SELECT height,hash FROM blocks ORDER BY height DESC LIMIT 1").Scan(&n, &h)
	if errors.Is(e, sql.ErrNoRows) {
		return int64(s.Config.StartBlock) - 1, common.Hash{}, nil
	}
	return n, common.HexToHash(h), e
}
func (s *Service) caughtUp(ctx context.Context, h Header) error {
	n, b, e := s.cursor(ctx)
	if e != nil {
		return e
	}
	if b == (common.Hash{}) || !(n == h.Number.ToInt().Int64() && b == h.Hash || n+1 == h.Number.ToInt().Int64() && b == h.Parent) {
		return errors.New("canonical index catching up")
	}
	return nil
}
func (s *Service) index(ctx context.Context, l lease) error {
	head, e := s.Chain.Header(ctx, "latest")
	if e != nil {
		return e
	}
	for range 32 {
		n, prev, e := s.cursor(ctx)
		if e != nil {
			return e
		}
		// A lagging provider cannot prove that our last indexed block was orphaned.
		if n > head.Number.ToInt().Int64() {
			return errors.New("RPC behind index checkpoint")
		}
		if prev != (common.Hash{}) {
			canonical, e := s.Chain.Header(ctx, tag(n))
			if e != nil {
				return e
			}
			if canonical.Hash != prev {
				if e = s.rollback(ctx, l, n); e != nil {
					return e
				}
				continue
			}
		}
		if n >= head.Number.ToInt().Int64() {
			return nil
		}
		block, e := s.Chain.Header(ctx, tag(n+1))
		if e != nil {
			return e
		}
		if block.Number.ToInt().Int64() != n+1 || prev != (common.Hash{}) && block.Parent != prev {
			return errors.New("chain changed during index scan")
		}
		logs, e := s.Chain.Logs(ctx, s.Config.EntryPoint, block.Hash)
		if e != nil {
			return e
		}
		var results []OpReceipt
		cache := map[common.Hash]*types.Receipt{}
		for _, log := range logs {
			if log.BlockHash != block.Hash {
				return errors.New("provider returned a different block")
			}
			r := cache[log.TxHash]
			if r == nil {
				r, e = s.Chain.Receipt(ctx, log.TxHash)
				if e != nil {
					return e
				}
				cache[log.TxHash] = r
			}
			o, e := decodeEvent(s.Config.EntryPoint, log, r)
			if e != nil {
				return e
			}
			results = append(results, o)
		}
		check, e := s.Chain.Header(ctx, tag(n+1))
		if e != nil {
			return e
		}
		if check.Hash != block.Hash {
			return errors.New("block displaced during receipt lookups")
		}
		if e = s.Store.locked(ctx, l, func(tx *sql.Tx) error {
			if _, e := tx.ExecContext(ctx, "INSERT INTO blocks(height,hash,parent) VALUES($1,$2,$3)", n+1, block.Hash.Hex(), block.Parent.Hex()); e != nil {
				return e
			}
			for h, r := range cache {
				body, e := json.Marshal(r)
				if e != nil {
					return e
				}
				if _, e = tx.ExecContext(ctx, "INSERT INTO tx_receipts(hash,height,body) VALUES($1,$2,$3)", h.Hex(), n+1, string(body)); e != nil {
					return e
				}
			}
			for _, o := range results {
				txHash := o.Receipt.TxHash.Hex()
				o.Receipt = nil
				body, e := json.Marshal(o)
				if e != nil {
					return e
				}
				if _, e = tx.ExecContext(ctx, "INSERT INTO receipts(hash,body,height,tx_hash) VALUES($1,$2,$3,$4)", o.Hash.Hex(), string(body), n+1, txHash); e != nil {
					return e
				}
				// External bundles may include an older candidate or consume a local slot.
				if _, e = tx.ExecContext(ctx, "UPDATE operations SET stage='CONSUMED',reason='EXTERNAL_NONCE',height=$1,block_hash=$2 WHERE sender=$3 AND nonce=$4 AND hash<>$5 AND stage IN ('QUEUED','BUNDLED')", n+1, block.Hash.Hex(), o.Sender.Hex(), o.Nonce.String(), o.Hash.Hex()); e != nil {
					return e
				}
				if _, e = tx.ExecContext(ctx, "UPDATE operations SET stage='INCLUDED',reason='',height=$1,block_hash=$2 WHERE hash=$3", n+1, block.Hash.Hex(), o.Hash.Hex()); e != nil {
					return e
				}
			}
			_, e := tx.ExecContext(ctx, "UPDATE operations SET stage='FINAL' WHERE stage='INCLUDED' AND height<=$1", n+2-int64(s.Config.Confirmations))
			return e
		}); e != nil {
			return e
		}
	}
	return nil
}
func (s *Service) rollback(ctx context.Context, l lease, n int64) error {
	return s.Store.locked(ctx, l, func(tx *sql.Tx) error {
		// Restore the most recently admitted candidate per affected slot. The other
		// signed candidates keep their history and can still win on the new fork.
		if _, e := tx.ExecContext(ctx, "UPDATE operations SET stage='REPLACED',reason='REORG',bundle=NULL,height=NULL,block_hash=NULL WHERE height=$1", n); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `UPDATE operations SET stage='QUEUED',reason='',due=0 WHERE hash IN (
 SELECT hash FROM (SELECT hash,row_number() OVER(PARTITION BY sender,nonce ORDER BY queued DESC,hash DESC) AS seq
 FROM operations o WHERE reason='REORG' AND NOT EXISTS(SELECT 1 FROM operations v WHERE v.sender=o.sender AND v.nonce=o.nonce AND v.stage IN ('QUEUED','BUNDLED','INCLUDED','FINAL'))) ranked WHERE seq=1)`); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, "DELETE FROM receipts WHERE height=$1", n); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, "DELETE FROM tx_receipts WHERE height=$1", n); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "DELETE FROM blocks WHERE height=$1", n)
		return e
	})
}
