package bundler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// ManagedValidation is a private pool for operator-reviewed validation code.
// Review the storage/opcode policy of each admitted implementation before
// adding its hash. A public pool can plug its full ERC-7562 tracer into Validator.
type ManagedValidation struct {
	node     Caller
	config   Config
	contract abi.ABI
	code     hexutil.Bytes
	approved map[common.Hash]bool
}

const simulationArtifactSHA = "051f532f91f121b5a43915a62b5f098112ed4afb0449cb7a5795d186be1789f1"

func LoadManagedValidation(node Caller, c Config, path string, approved []common.Hash) (*ManagedValidation, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != simulationArtifactSHA {
		return nil, errors.New("expected EntryPointSimulations.json from @account-abstraction/contracts@0.7.0")
	}
	var a struct {
		ABI              json.RawMessage
		DeployedBytecode hexutil.Bytes
	}
	if e = json.Unmarshal(b, &a); e != nil {
		return nil, e
	}
	parsed, e := abi.JSON(bytes.NewReader(a.ABI))
	if e != nil {
		return nil, e
	}
	m := &ManagedValidation{node, c, parsed, a.DeployedBytecode, map[common.Hash]bool{}}
	for _, h := range approved {
		m.approved[h] = true
	}
	if len(approved) == 0 {
		return nil, errors.New("managed validation requires approved runtime hashes")
	}
	return m, nil
}

type callFrame struct {
	From          common.Address
	To            *common.Address
	Type          string
	Input, Output hexutil.Bytes
	GasUsed       hexutil.Uint64
	Error         string
	Calls         []callFrame
}

func (m *ManagedValidation) trace(ctx context.Context, o Operation, h common.Hash, method string) (callFrame, error) {
	var f callFrame
	args := []any{o.pack()}
	if method == "simulateHandleOp" {
		args = append(args, common.Address{}, []byte{})
	}
	data, e := m.contract.Pack(method, args...)
	if e != nil {
		return f, e
	}
	call := map[string]any{"from": m.config.Sender, "to": m.config.EntryPoint, "data": hexutil.Bytes(data), "gas": hexutil.Uint64(m.config.MaxGas)}
	opt := map[string]any{"tracer": "callTracer", "timeout": "5s", "stateOverrides": map[string]any{m.config.EntryPoint.Hex(): map[string]any{"code": m.code}}}
	if e = m.node.CallContext(ctx, &f, "debug_traceCall", call, map[string]any{"blockHash": h, "requireCanonical": true}, opt); e != nil {
		return f, e
	}
	if f.Error != "" {
		if reason := revertMessage(f.Output); reason != "" {
			return f, submissionError(reason)
		}
		return f, &RPCError{Code: -32500, Message: "EntryPoint simulation reverted", Data: mustJSON(hexutil.Encode(f.Output))}
	}
	if len(f.Output) == 0 {
		return f, errors.New("execution RPC does not support simulation state overrides")
	}
	return f, nil
}
func mustJSON(v any) json.RawMessage {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}
func revertMessage(data []byte) string {
	if len(data) < 4 {
		return ""
	}
	if s, e := abi.UnpackRevert(data); e == nil {
		return s
	}
	var f *failedOp
	if errors.As(decodeRevertData(data), &f) {
		return f.Reason
	}
	return ""
}

type stake struct {
	Stake           *big.Int
	UnstakeDelaySec *big.Int
}
type retInfo struct {
	PreOpGas, Prefund, AccountValidationData, PaymasterValidationData *big.Int
	PaymasterContext                                                  []byte
}
type validationResult struct {
	ReturnInfo                             retInfo
	SenderInfo, FactoryInfo, PaymasterInfo stake
	AggregatorInfo                         struct {
		Aggregator common.Address
		StakeInfo  stake
	}
}

func (m *ManagedValidation) checkCode(ctx context.Context, f callFrame, o Operation, h common.Hash, seen map[common.Address]bool) error {
	if f.To == nil {
		return errors.New("missing validation call target")
	}
	a := *f.To
	if !seen[a] {
		seen[a] = true
		// The exact EntryPoint and its first CREATE child (SenderCreator) are part of the pinned deployment.
		precompile := a.Big().IsUint64() && a.Big().Uint64() > 0 && a.Big().Uint64() <= 10
		if !precompile && a != m.config.EntryPoint && a != crypto.CreateAddress(m.config.EntryPoint, 1) {
			var code hexutil.Bytes
			if e := m.node.CallContext(ctx, &code, "eth_getCode", a, map[string]any{"blockHash": h, "requireCanonical": true}); e != nil {
				return e
			}
			// An audited factory may create this operation's sender in the simulation.
			if !(a == o.Sender && o.Factory != nil && len(code) == 0) && (len(code) == 0 || !m.approved[crypto.Keccak256Hash(code)]) {
				return &RPCError{Code: -32502, Message: "unapproved validation contract " + a.Hex()}
			}
		}
	}
	for _, child := range f.Calls {
		if e := m.checkCode(ctx, child, o, h, seen); e != nil {
			return e
		}
	}
	return nil
}
func validity(word *big.Int) (common.Address, uint64, uint64) {
	n := new(big.Int).Rsh(new(big.Int).Set(word), 160)
	until := n.Uint64() & ((1 << 48) - 1)
	after := n.Rsh(n, 48).Uint64()
	return common.BytesToAddress(word.Bytes()), after, until
}
func (m *ManagedValidation) Validate(ctx context.Context, o Operation, ep common.Address, h common.Hash) (Admission, error) {
	var t Admission
	payload, e := calldata([]Operation{o}, m.config.Beneficiary)
	if e != nil {
		return t, e
	}
	if o.PreVerificationGas.ToInt().Cmp(new(big.Int).SetUint64(minPreGas(payload))) < 0 {
		return t, invalid("preVerificationGas below calldata and transaction overhead")
	}
	f, e := m.trace(ctx, o, h, "simulateValidation")
	if e != nil {
		return t, e
	}
	seen := map[common.Address]bool{}
	if e = m.checkCode(ctx, f, o, h, seen); e != nil {
		return t, e
	}
	if !seen[o.Sender] || o.Paymaster != nil && !seen[*o.Paymaster] || o.Factory != nil && !seen[*o.Factory] {
		return t, errors.New("incomplete validation trace")
	}
	out, e := m.contract.Unpack("simulateValidation", f.Output)
	if e != nil {
		return t, e
	}
	info := abi.ConvertType(out[0], new(validationResult)).(*validationResult).ReturnInfo
	t.Block = h
	t.Prefund = (*hexutil.Big)(info.Prefund)
	for _, word := range []*big.Int{info.AccountValidationData, info.PaymasterValidationData} {
		a, after, until := validity(word)
		if a == common.BytesToAddress([]byte{1}) {
			return t, &RPCError{Code: -32507, Message: "validation signature failed"}
		}
		if a != (common.Address{}) {
			return t, &RPCError{Code: -32506, Message: "aggregator unsupported"}
		}
		t.ValidAfter = max(t.ValidAfter, after)
		if until != 0 && (t.ValidUntil == 0 || until < t.ValidUntil) {
			t.ValidUntil = until
		}
	}
	if o.Paymaster != nil {
		data := append(crypto.Keccak256([]byte("balanceOf(address)"))[:4], common.LeftPadBytes(o.Paymaster.Bytes(), 32)...)
		var balance hexutil.Bytes
		if e = m.node.CallContext(ctx, &balance, "eth_call", map[string]any{"to": ep, "data": hexutil.Bytes(data)}, map[string]any{"blockHash": h, "requireCanonical": true}); e != nil {
			return t, e
		}
		if len(balance) != 32 {
			return t, errors.New("invalid deposit response")
		}
		t.Deposit = field(balance)
	}
	return t, nil
}
func quantity(n uint64) *hexutil.Big { return (*hexutil.Big)(new(big.Int).SetUint64(n)) }
func (m *ManagedValidation) Estimate(ctx context.Context, o Operation, ep common.Address) (Gas, error) {
	var g Gas
	head, e := (EVM{m.node}).Header(ctx, "latest")
	if e != nil {
		return g, e
	}
	if o.Signature == nil {
		empty := hexutil.Bytes{}
		o.Signature = &empty
	}
	maxPart := m.config.MaxGas / 4
	for _, p := range []**hexutil.Big{&o.VerificationGasLimit, &o.CallGasLimit} {
		if *p == nil {
			*p = quantity(maxPart)
		}
	}
	if o.Paymaster != nil {
		for _, p := range []**hexutil.Big{&o.PaymasterVerificationGasLimit, &o.PaymasterPostOpGasLimit} {
			if *p == nil {
				*p = quantity(maxPart)
			}
		}
	}
	if o.MaxFeePerGas == nil {
		o.MaxFeePerGas = quantity(0)
	}
	if o.MaxPriorityFeePerGas == nil {
		o.MaxPriorityFeePerGas = quantity(0)
	}
	if o.PreVerificationGas == nil {
		o.PreVerificationGas = quantity(0)
	}
	data, e := calldata([]Operation{o}, m.config.Beneficiary)
	if e != nil {
		return g, e
	}
	// Conservative single-operation L1 calldata allowance, including fixed overhead.
	g.Pre = quantity(uint64(len(data))*16 + 42000)
	o.PreVerificationGas = g.Pre
	f, e := m.trace(ctx, o, head.Hash, "simulateHandleOp")
	if e != nil {
		return g, e
	}
	out, e := m.contract.Unpack("simulateHandleOp", f.Output)
	if e != nil {
		return g, e
	}
	type execution struct {
		PreOpGas, Paid, AccountValidationData, PaymasterValidationData *big.Int
		TargetSuccess                                                  bool
		TargetResult                                                   []byte
	}
	info := abi.ConvertType(out[0], new(execution)).(*execution)
	if !info.PreOpGas.IsUint64() || info.PreOpGas.Uint64() < g.Pre.ToInt().Uint64() || info.PreOpGas.Uint64() > m.config.MaxGas {
		return g, errors.New("unexpected validation gas")
	}
	verification := info.PreOpGas.Uint64() - g.Pre.ToInt().Uint64()
	g.Verification = quantity(verification + verification/4 + 10000)
	var callGas uint64
	found := false
	var walk func(callFrame) error
	walk = func(f callFrame) error {
		if f.From == ep && f.To != nil && *f.To == o.Sender && bytes.Equal(f.Input, *o.CallData) {
			found = true
			if f.Error != "" {
				return &RPCError{Code: -32500, Message: "account execution reverted", Data: mustJSON(hexutil.Encode(f.Output))}
			}
			callGas = max(callGas, uint64(f.GasUsed))
		}
		for _, child := range f.Calls {
			if e := walk(child); e != nil {
				return e
			}
		}
		return nil
	}
	if e = walk(f); e != nil {
		return g, e
	}
	if len(*o.CallData) > 0 && !found {
		return g, errors.New("account execution missing from trace")
	}
	g.Call = quantity(callGas + callGas/4 + 10000)
	if o.Paymaster != nil {
		g.Paymaster = g.Verification
	}
	return g, nil
}

// A single operation must cover the calldata and intrinsic transaction cost
// even when the queue is too small to amortize them across a bundle.
func minPreGas(data []byte) uint64 {
	total := uint64(21000)
	for _, b := range data {
		if b == 0 {
			total += 4
		} else {
			total += 16
		}
	}
	return total
}
