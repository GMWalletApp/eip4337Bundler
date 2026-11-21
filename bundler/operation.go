// Package bundler implements the ERC-4337 v0.7 wallet API and durable bundle workers.
package bundler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

type Operation struct {
	Sender                        common.Address  `json:"sender"`
	Nonce                         *hexutil.Big    `json:"nonce"`
	Factory                       *common.Address `json:"factory,omitempty"`
	FactoryData                   *hexutil.Bytes  `json:"factoryData,omitempty"`
	CallData                      *hexutil.Bytes  `json:"callData"`
	CallGasLimit                  *hexutil.Big    `json:"callGasLimit,omitempty"`
	VerificationGasLimit          *hexutil.Big    `json:"verificationGasLimit,omitempty"`
	PreVerificationGas            *hexutil.Big    `json:"preVerificationGas,omitempty"`
	MaxFeePerGas                  *hexutil.Big    `json:"maxFeePerGas,omitempty"`
	MaxPriorityFeePerGas          *hexutil.Big    `json:"maxPriorityFeePerGas,omitempty"`
	Paymaster                     *common.Address `json:"paymaster,omitempty"`
	PaymasterVerificationGasLimit *hexutil.Big    `json:"paymasterVerificationGasLimit,omitempty"`
	PaymasterPostOpGasLimit       *hexutil.Big    `json:"paymasterPostOpGasLimit,omitempty"`
	PaymasterData                 *hexutil.Bytes  `json:"paymasterData,omitempty"`
	Signature                     *hexutil.Bytes  `json:"signature"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string  { return fmt.Sprintf("%d: %s", e.Code, e.Message) }
func invalid(message string) error { return &RPCError{Code: -32602, Message: message} }
func (op Operation) check(estimate bool) error {
	if op.Sender == (common.Address{}) || op.Nonce == nil || op.CallData == nil || (!estimate && op.Signature == nil) {
		return invalid("sender, nonce, callData and signed submission fields required")
	}
	if (op.Factory == nil) != (op.FactoryData == nil) || op.Factory != nil && *op.Factory == (common.Address{}) {
		return invalid("invalid factory fields")
	}
	if (op.Paymaster == nil) != (op.PaymasterData == nil) || op.Paymaster != nil && *op.Paymaster == (common.Address{}) {
		return invalid("invalid paymaster fields")
	}
	limits := []*hexutil.Big{op.VerificationGasLimit, op.CallGasLimit, op.MaxFeePerGas, op.MaxPriorityFeePerGas}
	if op.Paymaster != nil {
		limits = append(limits, op.PaymasterVerificationGasLimit, op.PaymasterPostOpGasLimit)
	} else if op.PaymasterVerificationGasLimit != nil || op.PaymasterPostOpGasLimit != nil {
		return invalid("paymaster gas limits require paymaster")
	}
	for _, n := range limits {
		if n == nil {
			if !estimate {
				return invalid("missing gas limit or fee")
			}
			continue
		}
		if n.ToInt().BitLen() > 128 {
			return invalid("packed quantity exceeds 128 bits")
		}
	}
	if !estimate && (op.PreVerificationGas == nil || op.VerificationGasLimit.ToInt().Sign() == 0 || op.MaxFeePerGas.ToInt().Sign() == 0 || op.MaxPriorityFeePerGas.ToInt().Cmp(op.MaxFeePerGas.ToInt()) > 0) {
		return invalid("invalid gas limits or fee cap")
	}
	return nil
}

type packedOp struct {
	Sender             common.Address
	Nonce              *big.Int
	InitCode           []byte
	CallData           []byte
	AccountGasLimits   [32]byte
	PreVerificationGas *big.Int
	GasFees            [32]byte
	PaymasterAndData   []byte
	Signature          []byte
}

func pair(hi, lo *hexutil.Big) (word [32]byte) {
	hi.ToInt().FillBytes(word[:16])
	lo.ToInt().FillBytes(word[16:])
	return
}
func (op Operation) pack() packedOp {
	p := packedOp{Sender: op.Sender, Nonce: op.Nonce.ToInt(), CallData: *op.CallData, AccountGasLimits: pair(op.VerificationGasLimit, op.CallGasLimit), PreVerificationGas: op.PreVerificationGas.ToInt(), GasFees: pair(op.MaxPriorityFeePerGas, op.MaxFeePerGas), Signature: *op.Signature}
	if op.Factory != nil {
		p.InitCode = append(p.InitCode, op.Factory.Bytes()...)
		p.InitCode = append(p.InitCode, *op.FactoryData...)
	}
	if op.Paymaster != nil {
		gas := pair(op.PaymasterVerificationGasLimit, op.PaymasterPostOpGasLimit)
		p.PaymasterAndData = append(p.PaymasterAndData, op.Paymaster.Bytes()...)
		p.PaymasterAndData = append(p.PaymasterAndData, gas[:]...)
		p.PaymasterAndData = append(p.PaymasterAndData, *op.PaymasterData...)
	}
	return p
}

// v0.7 hashes ABI words and excludes the signature; later EntryPoints have different signing domains.
func (op Operation) Hash(entry common.Address, chain *big.Int) common.Hash {
	p := op.pack()
	word := func(n *big.Int) []byte { return n.FillBytes(make([]byte, 32)) }
	addr := func(a common.Address) []byte { return common.LeftPadBytes(a.Bytes(), 32) }
	hash := crypto.Keccak256(addr(p.Sender), word(p.Nonce), crypto.Keccak256(p.InitCode), crypto.Keccak256(p.CallData), p.AccountGasLimits[:], word(p.PreVerificationGas), p.GasFees[:], crypto.Keccak256(p.PaymasterAndData))
	return crypto.Keccak256Hash(hash, addr(entry), word(chain))
}

const entryABI = `[{"type":"function","name":"handleOps","inputs":[{"name":"ops","type":"tuple[]","components":[{"name":"sender","type":"address"},{"name":"nonce","type":"uint256"},{"name":"initCode","type":"bytes"},{"name":"callData","type":"bytes"},{"name":"accountGasLimits","type":"bytes32"},{"name":"preVerificationGas","type":"uint256"},{"name":"gasFees","type":"bytes32"},{"name":"paymasterAndData","type":"bytes"},{"name":"signature","type":"bytes"}]},{"name":"beneficiary","type":"address"}],"outputs":[]}]`

var contract = func() abi.ABI {
	a, e := abi.JSON(strings.NewReader(entryABI))
	if e != nil {
		panic(e)
	}
	return a
}()

func calldata(ops []Operation, beneficiary common.Address) ([]byte, error) {
	packed := make([]packedOp, len(ops))
	for i := range ops {
		packed[i] = ops[i].pack()
	}
	return contract.Pack("handleOps", packed, beneficiary)
}
func decodeOp(raw json.RawMessage, estimate bool) (Operation, error) {
	var op Operation
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&op); e != nil {
		return op, invalid(e.Error())
	}
	return op, op.check(estimate)
}
func bumpEnough(old, next *hexutil.Big) bool {
	floor := new(big.Int).Mul(old.ToInt(), big.NewInt(110))
	floor.Add(floor, big.NewInt(99)).Div(floor, big.NewInt(100))
	return next.ToInt().Cmp(floor) >= 0 && next.ToInt().Cmp(old.ToInt()) > 0
}

var errFenced = errors.New("worker lease superseded")

func envelope(p packedOp) (Operation, error) {
	bytesField := func(b []byte) *hexutil.Bytes { v := hexutil.Bytes(b); return &v }
	o := Operation{Sender: p.Sender, Nonce: (*hexutil.Big)(p.Nonce), CallData: bytesField(p.CallData), Signature: bytesField(p.Signature), PreVerificationGas: (*hexutil.Big)(p.PreVerificationGas),
		VerificationGasLimit: field(p.AccountGasLimits[:16]), CallGasLimit: field(p.AccountGasLimits[16:]), MaxPriorityFeePerGas: field(p.GasFees[:16]), MaxFeePerGas: field(p.GasFees[16:])}
	if len(p.InitCode) > 0 {
		if len(p.InitCode) < 20 {
			return o, invalid("truncated initCode")
		}
		a := common.BytesToAddress(p.InitCode[:20])
		o.Factory = &a
		o.FactoryData = bytesField(p.InitCode[20:])
	}
	if len(p.PaymasterAndData) > 0 {
		if len(p.PaymasterAndData) < 52 {
			return o, invalid("truncated paymasterAndData")
		}
		a := common.BytesToAddress(p.PaymasterAndData[:20])
		o.Paymaster = &a
		o.PaymasterVerificationGasLimit = field(p.PaymasterAndData[20:36])
		o.PaymasterPostOpGasLimit = field(p.PaymasterAndData[36:52])
		o.PaymasterData = bytesField(p.PaymasterAndData[52:])
	}
	return o, o.check(false)
}
