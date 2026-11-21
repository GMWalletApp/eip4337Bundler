package bundler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

type Admission struct {
	Block      common.Hash  `json:"blockHash"`
	Prefund    *hexutil.Big `json:"prefund"`
	Deposit    *hexutil.Big `json:"paymasterDeposit,omitempty"`
	ValidAfter uint64       `json:"validAfter"`
	ValidUntil uint64       `json:"validUntil"`
}
type Gas struct {
	Pre          *hexutil.Big `json:"preVerificationGas"`
	Call         *hexutil.Big `json:"callGasLimit"`
	Verification *hexutil.Big `json:"verificationGasLimit"`
	Paymaster    *hexutil.Big `json:"paymasterVerificationGasLimit,omitempty"`
}

// Validator is the deployment's validation boundary. Implement account/paymaster
// simulation and ERC-7562 policy at the supplied block, including signature,
// aggregation, validity, prefund and stake checks. Transport errors are retryable.
// IMPL(integrator): implement this interface for the operator’s external validation policy.
type Validator interface {
	Validate(context.Context, Operation, common.Address, common.Hash) (Admission, error)
	Estimate(context.Context, Operation, common.Address) (Gas, error)
}

// Signer implementations sign only. Sending a transaction here would break crash recovery.
// IMPL(integrator): adapt the company KMS/HSM here. Return the signed envelope only.
type Signer interface {
	Sign(context.Context, *types.Transaction, common.Address) (*types.Transaction, error)
}

type RemoteValidation struct{ RPC *rpc.Client }

func validationError(err error) error {
	var r rpc.Error
	if errors.As(err, &r) && (r.ErrorCode() >= -32508 && r.ErrorCode() <= -32500 || r.ErrorCode() == -32602) {
		v := &RPCError{Code: r.ErrorCode(), Message: r.Error()}
		var data rpc.DataError
		if errors.As(err, &data) {
			v.Data, _ = json.Marshal(data.ErrorData())
		}
		return v
	}
	return err
}
func (v RemoteValidation) Validate(ctx context.Context, o Operation, ep common.Address, block common.Hash) (Admission, error) {
	var t Admission
	e := v.RPC.CallContext(ctx, &t, "gm_validateUserOperation", o, ep, block)
	if e != nil {
		return t, validationError(e)
	}
	if t.Block != block || t.Prefund == nil || t.Prefund.ToInt().Sign() <= 0 || o.Paymaster != nil && t.Deposit == nil {
		return t, errors.New("incomplete validation attestation")
	}
	return t, nil
}
func (v RemoteValidation) Estimate(ctx context.Context, o Operation, ep common.Address) (Gas, error) {
	var g Gas
	e := v.RPC.CallContext(ctx, &g, "eth_estimateUserOperationGas", o, ep)
	if e != nil {
		return g, validationError(e)
	}
	if g.Pre == nil || g.Call == nil || g.Verification == nil || o.Paymaster != nil && g.Paymaster == nil {
		return g, errors.New("incomplete gas estimate")
	}
	return g, nil
}

type RPCSigner struct{ RPC *rpc.Client }

func (s RPCSigner) Sign(ctx context.Context, tx *types.Transaction, from common.Address) (*types.Transaction, error) {
	args := map[string]any{"from": from, "to": tx.To(), "nonce": hexutil.Uint64(tx.Nonce()), "gas": hexutil.Uint64(tx.Gas()), "value": "0x0", "data": hexutil.Bytes(tx.Data()), "chainId": (*hexutil.Big)(tx.ChainId()), "maxFeePerGas": (*hexutil.Big)(tx.GasFeeCap()), "maxPriorityFeePerGas": (*hexutil.Big)(tx.GasTipCap()), "type": "0x2"}
	var reply struct {
		Raw hexutil.Bytes `json:"raw"`
	}
	var response json.RawMessage
	if e := s.RPC.CallContext(ctx, &response, "eth_signTransaction", args); e != nil {
		return nil, e
	}
	if len(response) > 0 && response[0] == '"' {
		if e := json.Unmarshal(response, &reply.Raw); e != nil {
			return nil, e
		}
	} else if e := json.Unmarshal(response, &reply); e != nil {
		return nil, e
	}
	var signed types.Transaction
	if e := signed.UnmarshalBinary(reply.Raw); e != nil {
		return nil, e
	}
	return &signed, nil
}

type BroadcastView struct {
	Active   string `json:"activeHash"`
	Task     string `json:"taskId"`
	Network  string `json:"network"`
	Hash     string `json:"txHash"`
	Queried  string `json:"queriedHash"`
	State    string `json:"taskState"`
	Outcome  string `json:"taskOutcome"`
	Reason   string `json:"reason"`
	Winner   string `json:"winningHash"`
	Attempts []struct {
		Hash  string `json:"hash"`
		State string `json:"state"`
	} `json:"attempts"`
}

// HTTPBroadcaster is the working GM Wallet integration. Alternative transports
// must preserve the lookup and idempotency semantics of the original hash.
type Broadcaster interface {
	Replace(context.Context, common.Hash, string, common.Hash) (BroadcastView, error)
	Nonce(context.Context, common.Address) (uint64, error)
	Lookup(context.Context, common.Hash) (BroadcastView, bool, error)
	Submit(context.Context, string, common.Hash) (BroadcastView, error)
}
type HTTPBroadcaster struct {
	URL, Token, Chain string
	HTTP              *http.Client
}
type BroadcastError struct {
	Status int
	Code   string
}

func (e *BroadcastError) Error() string {
	return fmt.Sprintf("Broadcaster HTTP %d %s", e.Status, e.Code)
}
func (b HTTPBroadcaster) request(ctx context.Context, method, path string, body []byte, out any) error {
	req, e := http.NewRequestWithContext(ctx, method, b.URL+path+"?chainId="+b.Chain, bytes.NewReader(body))
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+b.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, e := b.HTTP.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var f struct{ Error string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&f)
		return &BroadcastError{resp.StatusCode, f.Error}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}
func (b HTTPBroadcaster) Nonce(ctx context.Context, a common.Address) (uint64, error) {
	var r struct{ Nonce string }
	e := b.request(ctx, "GET", "/EVM/accounts/"+a.Hex()+"/nonce", nil, &r)
	if e != nil {
		return 0, e
	}
	return strconv.ParseUint(r.Nonce, 10, 64)
}
func (b HTTPBroadcaster) Lookup(ctx context.Context, h common.Hash) (BroadcastView, bool, error) {
	var r BroadcastView
	e := b.request(ctx, "GET", "/EVM/transactions/"+h.Hex(), nil, &r)
	var be *BroadcastError
	if errors.As(e, &be) && be.Status == 404 {
		return r, false, nil
	}
	if e == nil && (r.Queried != h.Hex() || r.Network != b.Chain || r.Task == "") {
		return r, false, errors.New("mismatched Broadcaster lookup")
	}
	return r, e == nil, e
}
func (b HTTPBroadcaster) Submit(ctx context.Context, raw string, h common.Hash) (BroadcastView, error) {
	var r BroadcastView
	body, _ := json.Marshal(map[string]string{"rawTransaction": raw})
	e := b.request(ctx, "POST", "/EVM/transactions", body, &r)
	if e == nil && (r.Hash != h.Hex() || r.Network != b.Chain || r.Task == "") {
		return r, errors.New("mismatched Broadcaster acknowledgement")
	}
	return r, e
}

type Config struct {
	ChainID                         *big.Int
	EntryPoint, Beneficiary, Sender common.Address
	CodeHash                        common.Hash
	StartBlock                      uint64
	Confirmations                   uint64
	MaxOps, MaxBytes, PoolSize      int
	MaxGas                          uint64
	MaxFee                          *big.Int
	Poll                            time.Duration
}
