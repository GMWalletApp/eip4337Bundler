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

func (b HTTPBroadcaster) Replace(ctx context.Context, parent common.Hash, raw string, expected common.Hash) (BroadcastView, error) {
	var v BroadcastView
	body, _ := json.Marshal(map[string]string{"rawTransaction": raw})
	e := b.request(ctx, "POST", "/EVM/transactions/"+parent.Hex()+"/replacement", body, &v)
	if e == nil && (v.Task == "" || v.Hash != expected.Hex() || v.Network != b.Chain) {
		return v, errors.New("mismatched replacement acknowledgement")
	}
	return v, e
}
func bumped(n *big.Int) *big.Int {
	v := new(big.Int).Mul(n, big.NewInt(9))
	v.Div(v, big.NewInt(8))
	return v.Add(v, big.NewInt(1))
}
func (s *Service) sign(ctx context.Context, unsigned *types.Transaction) (*types.Transaction, string, error) {
	signed, e := s.Signer.Sign(ctx, unsigned, s.Config.Sender)
	if e != nil {
		return nil, "", e
	}
	signer := types.LatestSignerForChainID(s.Config.ChainID)
	if signed == nil || signed.Type() != types.DynamicFeeTxType || signed.ChainId().Cmp(s.Config.ChainID) != 0 || signer.Hash(signed) != signer.Hash(unsigned) {
		return nil, "", errors.New("signer changed authorized transaction")
	}
	sender, e := types.Sender(signer, signed)
	if e != nil || sender != s.Config.Sender {
		return nil, "", errors.New("unexpected bundle signer")
	}
	raw, e := signed.MarshalBinary()
	return signed, hexutil.Encode(raw), e
}

// Persist a successor before making the replacement request. The root task is
// always queried first, so a late inclusion of any predecessor can still win.
func (s *Service) replaceOuter(ctx context.Context, l lease, root, original string, v BroadcastView) error {
	var prior types.Transaction
	parent := v.Active
	if parent == "" {
		return errors.New("missing active broadcast hash")
	}
	var hash, raw string
	err := s.Store.db.QueryRowContext(ctx, "SELECT hash,raw FROM replacements WHERE root=$1 AND parent=$2", root, parent).Scan(&hash, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		prev := original
		if parent != root {
			err = s.Store.db.QueryRowContext(ctx, "SELECT raw FROM replacements WHERE root=$1 AND hash=$2", root, parent).Scan(&prev)
			if err != nil {
				return err
			}
		}
		bytes, e := hexutil.Decode(prev)
		if e != nil {
			return e
		}
		if e = prior.UnmarshalBinary(bytes); e != nil {
			return e
		}
		head, e := s.Chain.Header(ctx, "latest")
		if e != nil {
			return e
		}
		if head.BaseFee == nil {
			return errors.New("missing base fee")
		}
		suggested, e := s.Chain.Price(ctx)
		if e != nil {
			return e
		}
		tip := bumped(prior.GasTipCap())
		if tip.Cmp(suggested) < 0 {
			tip = suggested
		}
		fee := bumped(prior.GasFeeCap())
		market := new(big.Int).Add(head.BaseFee.ToInt(), tip)
		if fee.Cmp(market) < 0 {
			fee = market
		}
		if fee.Cmp(s.Config.MaxFee) > 0 {
			return nil
		}
		rows, e := s.Store.db.QueryContext(ctx, "SELECT o.body FROM members m JOIN operations o ON o.hash=m.op WHERE m.bundle=$1", root)
		if e != nil {
			return e
		}
		for rows.Next() {
			var body []byte
			var op Operation
			if e = rows.Scan(&body); e == nil {
				e = json.Unmarshal(body, &op)
			}
			if e != nil {
				rows.Close()
				return e
			}
			if fee.Cmp(op.MaxFeePerGas.ToInt()) > 0 || tip.Cmp(op.MaxPriorityFeePerGas.ToInt()) > 0 {
				rows.Close()
				return nil
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		args := map[string]any{"from": s.Config.Sender, "to": prior.To(), "gas": hexutil.Uint64(prior.Gas()), "data": hexutil.Bytes(prior.Data()), "maxFeePerGas": (*hexutil.Big)(fee), "maxPriorityFeePerGas": (*hexutil.Big)(tip)}
		if _, e = s.Chain.Simulate(ctx, head, args); e != nil {
			return e
		}
		signed, encoded, e := s.sign(ctx, types.NewTx(&types.DynamicFeeTx{ChainID: s.Config.ChainID, Nonce: prior.Nonce(), To: prior.To(), Data: prior.Data(), Gas: prior.Gas(), GasTipCap: tip, GasFeeCap: fee}))
		if e != nil {
			return e
		}
		hash, raw = signed.Hash().Hex(), encoded
		if e = s.Store.locked(ctx, l, func(tx *sql.Tx) error {
			_, e := tx.ExecContext(ctx, "INSERT INTO replacements(root,parent,hash,raw) VALUES($1,$2,$3,$4)", root, parent, hash, raw)
			return e
		}); e != nil {
			return e
		}
	} else if err != nil {
		return err
	}
	_, err = s.Broadcast.Replace(ctx, common.HexToHash(parent), raw, common.HexToHash(hash))
	return err
}
