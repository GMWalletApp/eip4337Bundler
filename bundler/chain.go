package bundler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

type Header struct {
	Hash     common.Hash    `json:"hash"`
	Parent   common.Hash    `json:"parentHash"`
	Number   *hexutil.Big   `json:"number"`
	Time     hexutil.Uint64 `json:"timestamp"`
	BaseFee  *hexutil.Big   `json:"baseFeePerGas"`
	GasLimit hexutil.Uint64 `json:"gasLimit"`
}
type Chain interface {
	Operation(context.Context, *OpReceipt, *big.Int) (*Operation, error)
	Header(context.Context, string) (Header, error)
	Price(context.Context) (*big.Int, error)
	Simulate(context.Context, Header, map[string]any) (uint64, error)
	Logs(context.Context, common.Address, common.Hash) ([]types.Log, error)
	Receipt(context.Context, common.Hash) (*types.Receipt, error)
}
type EVM struct{ RPC Caller }

func (e EVM) Verify(ctx context.Context, c Config) error {
	var id hexutil.Big
	if err := e.RPC.CallContext(ctx, &id, "eth_chainId"); err != nil {
		return err
	}
	if id.ToInt().Cmp(c.ChainID) != 0 {
		return errors.New("execution RPC chain mismatch")
	}
	var code hexutil.Bytes
	if err := e.RPC.CallContext(ctx, &code, "eth_getCode", c.EntryPoint, "latest"); err != nil {
		return err
	}
	if len(code) == 0 || crypto.Keccak256Hash(code) != c.CodeHash {
		return errors.New("EntryPoint runtime hash mismatch")
	}
	return nil
}
func (e EVM) Header(ctx context.Context, tag string) (Header, error) {
	var h *Header
	err := e.RPC.CallContext(ctx, &h, "eth_getBlockByNumber", tag, false)
	if err != nil {
		return Header{}, err
	}
	if h == nil || h.Number == nil || !h.Number.ToInt().IsInt64() || h.Hash == (common.Hash{}) {
		return Header{}, errors.New("block header unavailable")
	}
	return *h, nil
}
func (e EVM) Price(ctx context.Context) (*big.Int, error) {
	var p hexutil.Big
	err := e.RPC.CallContext(ctx, &p, "eth_maxPriorityFeePerGas")
	return p.ToInt(), err
}
func (e EVM) Simulate(ctx context.Context, h Header, args map[string]any) (uint64, error) {
	tag := map[string]any{"blockHash": h.Hash, "requireCanonical": true}
	var gas hexutil.Uint64
	err := e.RPC.CallContext(ctx, &gas, "eth_estimateGas", args, tag)
	if err != nil {
		return 0, decodeRevert(err)
	}
	if gas == 0 {
		return 0, errors.New("empty bundle gas estimate")
	}
	var trace callFrame
	if err = e.RPC.CallContext(ctx, &trace, "debug_traceCall", args, tag, map[string]any{"tracer": "callTracer", "timeout": "5s", "tracerConfig": map[string]bool{"onlyTopCall": true}}); err != nil {
		return 0, err
	}
	if trace.Error != "" {
		return 0, decodeRevertData(trace.Output)
	}
	if trace.To == nil || trace.GasUsed == 0 {
		return 0, errors.New("incomplete final bundle simulation")
	}
	return uint64(gas), nil
}

type failedOp struct {
	Index  int
	Reason string
}

func (e *failedOp) Error() string { return e.Reason }
func decodeRevert(err error) error {
	var re rpc.DataError
	if !errors.As(err, &re) {
		return err
	}
	data, ok := re.ErrorData().(string)
	if !ok {
		return err
	}
	raw, e := hexutil.Decode(data)
	if e != nil || len(raw) < 100 {
		return err
	}
	return decodeRevertData(raw)
}

func decodeRevertData(raw []byte) error {
	err := errors.New("unrecognized EntryPoint revert")
	if len(raw) < 100 {
		return err
	}
	for _, sig := range []string{"FailedOp(uint256,string)", "FailedOpWithRevert(uint256,string,bytes)"} {
		if string(raw[:4]) != string(crypto.Keccak256([]byte(sig))[:4]) {
			continue
		}
		idx := new(big.Int).SetBytes(raw[4:36])
		offset := new(big.Int).SetBytes(raw[36:68])
		if !idx.IsInt64() || !offset.IsUint64() || offset.Uint64() > uint64(len(raw)-36) {
			return err
		}
		start := 4 + int(offset.Uint64())
		n := new(big.Int).SetBytes(raw[start : start+32])
		if !n.IsUint64() || n.Uint64() > uint64(len(raw)-start-32) {
			return err
		}
		return &failedOp{int(idx.Int64()), string(raw[start+32 : start+32+int(n.Uint64())])}
	}
	return err
}

var operationEvent = crypto.Keccak256Hash([]byte("UserOperationEvent(bytes32,address,address,uint256,bool,uint256,uint256)"))
var beforeExecution = crypto.Keccak256Hash([]byte("BeforeExecution()"))
var revertEvent = crypto.Keccak256Hash([]byte("UserOperationRevertReason(bytes32,address,uint256,bytes)"))

func (e EVM) Logs(ctx context.Context, ep common.Address, b common.Hash) ([]types.Log, error) {
	var logs []types.Log
	err := e.RPC.CallContext(ctx, &logs, "eth_getLogs", map[string]any{"address": ep, "blockHash": b, "topics": []any{operationEvent}})
	return logs, err
}
func (e EVM) Receipt(ctx context.Context, h common.Hash) (*types.Receipt, error) {
	var r *types.Receipt
	err := e.RPC.CallContext(ctx, &r, "eth_getTransactionReceipt", h)
	if err == nil && (r == nil || r.TxHash != h) {
		return nil, errors.New("receipt unavailable")
	}
	return r, err
}
func submissionError(reason string) *RPCError {
	code := -32500
	switch {
	case strings.HasPrefix(reason, "AA24"), strings.HasPrefix(reason, "AA34"):
		code = -32507
	case strings.HasPrefix(reason, "AA22"), strings.HasPrefix(reason, "AA32"):
		code = -32503
	case strings.HasPrefix(reason, "AA3"):
		code = -32501
	}
	return &RPCError{Code: code, Message: reason}
}
func tag(n int64) string { return fmt.Sprintf("0x%x", n) }

// Recover an operation included by another bundler from the canonical outer transaction.
func (e EVM) Operation(ctx context.Context, r *OpReceipt, chain *big.Int) (*Operation, error) {
	var tx *struct {
		Hash, BlockHash common.Hash
		To              *common.Address
		Input           hexutil.Bytes
	}
	if err := e.RPC.CallContext(ctx, &tx, "eth_getTransactionByHash", r.Receipt.TxHash); err != nil {
		return nil, err
	}
	if tx == nil || tx.Hash != r.Receipt.TxHash || tx.BlockHash != r.Receipt.BlockHash {
		return nil, errors.New("canonical transaction unavailable")
	}
	method := contract.Methods["handleOps"]
	if tx.To == nil || *tx.To != r.EntryPoint || len(tx.Input) < 4 || !bytes.Equal(tx.Input[:4], method.ID) {
		return nil, nil
	}
	args, err := method.Inputs.Unpack(tx.Input[4:])
	if err != nil {
		return nil, err
	}
	ops := abi.ConvertType(args[0], new([]packedOp)).(*[]packedOp)
	for _, p := range *ops {
		o, e := envelope(p)
		if e != nil {
			return nil, e
		}
		if o.Hash(r.EntryPoint, chain) == r.Hash {
			return &o, nil
		}
	}
	return nil, errors.New("operation missing from canonical bundle")
}
