package bundler

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

func requireOK(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func port(t *testing.T) string {
	t.Helper()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	requireOK(t, e)
	defer ln.Close()
	return ln.Addr().String()
}
func startTool(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	requireOK(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Log(logs.String())
		}
	})
}

type artifact struct {
	ABI      json.RawMessage
	Bytecode hexutil.Bytes
}

func artifactAt(t *testing.T, dir, name string) (abi.ABI, []byte) {
	t.Helper()
	data, e := os.ReadFile(filepath.Join(dir, name+".json"))
	requireOK(t, e)
	var f artifact
	requireOK(t, json.Unmarshal(data, &f))
	a, e := abi.JSON(bytes.NewReader(f.ABI))
	requireOK(t, e)
	return a, f.Bytecode
}

type testSigner struct{ key *ecdsa.PrivateKey }

func (s testSigner) Sign(_ context.Context, t *types.Transaction, _ common.Address) (*types.Transaction, error) {
	return types.SignTx(t, types.LatestSignerForChainID(t.ChainId()), s.key)
}
func localPost(t *testing.T, url, token string, v any) (int, []byte) {
	t.Helper()
	req, e := http.NewRequest("POST", url, bytes.NewReader(mustJSON(v)))
	requireOK(t, e)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, e := http.DefaultClient.Do(req)
	requireOK(t, e)
	defer resp.Body.Close()
	raw, e := io.ReadAll(resp.Body)
	requireOK(t, e)
	return resp.StatusCode, raw
}

// This opt-in test starts disposable loopback processes, deploys real v0.7
// bytecode, signs two operations and verifies independent canonical receipts.
func TestAnvilBroadcaster(t *testing.T) {
	anvil, bc, dir := os.Getenv("TEST_ANVIL"), os.Getenv("TEST_BROADCASTER"), os.Getenv("TEST_AA_ARTIFACTS")
	if anvil == "" || bc == "" || dir == "" {
		t.Skip("set TEST_ANVIL, TEST_BROADCASTER and TEST_AA_ARTIFACTS")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	address := port(t)
	_, p, _ := net.SplitHostPort(address)
	startTool(t, exec.Command(anvil, "--port", p, "--chain-id", "999", "--silent"))
	url := "http://" + address
	client, e := rpc.DialContext(ctx, url)
	requireOK(t, e)
	defer client.Close()
	ec := ethclient.NewClient(client)
	for i := 0; ; i++ {
		_, e = ec.ChainID(ctx)
		if e == nil {
			break
		}
		if i > 60 {
			t.Fatal(e)
		}
		time.Sleep(100 * time.Millisecond)
	}
	chain := big.NewInt(999)
	key, e := crypto.GenerateKey()
	requireOK(t, e)
	sign := testSigner{key}
	from := crypto.PubkeyToAddress(key.PublicKey)
	fund := func(a common.Address) {
		t.Helper()
		requireOK(t, client.CallContext(ctx, nil, "anvil_setBalance", a, "0x10000000000000000000000"))
	}
	fund(from)
	signTx := func(to *common.Address, data []byte) *types.Transaction {
		t.Helper()
		n, e := ec.PendingNonceAt(ctx, from)
		requireOK(t, e)
		tx, e := sign.Sign(ctx, types.NewTx(&types.DynamicFeeTx{ChainID: chain, Nonce: n, To: to, Gas: 12000000, GasFeeCap: big.NewInt(4000000000), GasTipCap: big.NewInt(1000000000), Data: data}), from)
		requireOK(t, e)
		return tx
	}
	send := func(to *common.Address, data []byte) *types.Receipt {
		t.Helper()
		tx := signTx(to, data)
		requireOK(t, ec.SendTransaction(ctx, tx))
		var r *types.Receipt
		var e error
		for i := 0; i < 100; i++ {
			r, e = ec.TransactionReceipt(ctx, tx.Hash())
			if e == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		requireOK(t, e)
		if r.Status != 1 {
			t.Fatalf("setup transaction reverted: %s", tx.Hash())
		}
		return r
	}
	epABI, bytecode := artifactAt(t, dir, "EntryPoint")
	ep := send(nil, bytecode).ContractAddress
	factoryABI, bytecode := artifactAt(t, dir, "SimpleAccountFactory")
	args, e := factoryABI.Pack("", ep)
	requireOK(t, e)
	factory := send(nil, append(bytecode, args...)).ContractAddress
	accountABI, _ := artifactAt(t, dir, "SimpleAccount")
	call := func(to common.Address, data []byte) []byte {
		t.Helper()
		var out hexutil.Bytes
		requireOK(t, client.CallContext(ctx, &out, "eth_call", map[string]any{"to": to, "data": hexutil.Bytes(data)}, "latest"))
		return out
	}
	buildOp := func(salt int64, fail bool) Operation {
		t.Helper()
		args, e := factoryABI.Pack("getAddress", from, big.NewInt(salt))
		requireOK(t, e)
		sender := common.BytesToAddress(call(factory, args))
		args, e = factoryABI.Pack("createAccount", from, big.NewInt(salt))
		requireOK(t, e)
		send(&factory, args)
		fund(sender)
		dest := from
		inner := []byte{}
		if fail {
			dest = ep
			inner = []byte{0xde, 0xad, 0xbe, 0xef}
		}
		args, e = accountABI.Pack("execute", dest, big.NewInt(1), inner)
		requireOK(t, e)
		cal := hexutil.Bytes(args)
		stub := hexutil.Bytes(make([]byte, 65))
		op := Operation{Sender: sender, Nonce: quantity(0), CallData: &cal, CallGasLimit: quantity(150000), VerificationGasLimit: quantity(250000), PreVerificationGas: quantity(65000), MaxFeePerGas: quantity(4000000000), MaxPriorityFeePerGas: quantity(2000000000), Signature: &stub}
		hash := op.Hash(ep, chain)
		onchain, e := epABI.Pack("getUserOpHash", op.pack())
		requireOK(t, e)
		if common.BytesToHash(call(ep, onchain)) != hash {
			t.Fatal("v0.7 hash differs from deployed EntryPoint")
		}
		sig, e := crypto.Sign(accounts.TextHash(hash[:]), key)
		requireOK(t, e)
		sig[64] += 27
		stub = hexutil.Bytes(sig)
		return op
	}
	a, bop := buildOp(1, false), buildOp(2, true)
	// Sponsor the reverting operation. EntryPoint still charges its paymaster.
	pmABI, pmCode := artifactAt(t, dir, "VerifyingPaymaster")
	pmArgs, e := pmABI.Pack("", ep, from)
	requireOK(t, e)
	pm := send(nil, append(pmCode, pmArgs...)).ContractAddress
	deposit, e := epABI.Pack("depositTo", pm)
	requireOK(t, e)
	nonce, e := ec.PendingNonceAt(ctx, from)
	requireOK(t, e)
	depTx, e := sign.Sign(ctx, types.NewTx(&types.DynamicFeeTx{ChainID: chain, Nonce: nonce, To: &ep, Value: big.NewInt(1000000000000000000), Gas: 200000, GasFeeCap: big.NewInt(4000000000), GasTipCap: big.NewInt(1000000000), Data: deposit}), from)
	requireOK(t, e)
	requireOK(t, ec.SendTransaction(ctx, depTx))
	requireOK(t, client.CallContext(ctx, nil, "evm_mine"))
	pmData := hexutil.Bytes(make([]byte, 64))
	bop.Paymaster = &pm
	bop.PaymasterData = &pmData
	bop.PaymasterVerificationGasLimit = quantity(150000)
	bop.PaymasterPostOpGasLimit = quantity(60000)
	pHash, e := pmABI.Pack("getHash", bop.pack(), big.NewInt(0), big.NewInt(0))
	requireOK(t, e)
	signature := func(hash []byte) hexutil.Bytes {
		sig, e := crypto.Sign(accounts.TextHash(hash), key)
		requireOK(t, e)
		sig[64] += 27
		return sig
	}
	pmData = append(pmData, signature(call(pm, pHash))...)
	signedOp := signature(bop.Hash(ep, chain).Bytes())
	bop.Signature = &signedOp
	listen := port(t)
	root := t.TempDir()
	business := "bundler-local-business-token-123456"
	admin := "bundler-local-admin-token-123456789"
	config := map[string]any{"listen": listen, "workers": 2, "networks": map[string]any{"999": map[string]any{"family": "evm"}}, "clients": []any{map[string]any{"name": "bundler", "token": business}, map[string]any{"name": "ops", "token": admin, "admin": true}}}
	file := filepath.Join(root, "broadcast.json")
	requireOK(t, os.WriteFile(file, mustJSON(config), 0600))
	cmd := exec.Command(bc, "-config", file, "-db", filepath.Join(root, "broadcast.db"))
	cmd.Env = append(os.Environ(), "BROADCASTER_STORAGE_KEY="+base64.StdEncoding.EncodeToString(make([]byte, 32)))
	startTool(t, cmd)
	for i := 0; ; i++ {
		conn, e := net.DialTimeout("tcp", listen, 100*time.Millisecond)
		if e == nil {
			conn.Close()
			break
		}
		if i > 80 {
			t.Fatal("Broadcaster did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	base := "http://" + listen
	probe, e := signTx(&from, nil).MarshalBinary()
	requireOK(t, e)
	status, raw := localPost(t, base+"/rpc-providers", admin, map[string]any{"chainId": "999", "providerId": "local", "url": url, "evmProbes": []string{hexutil.Encode(probe)}})
	if status != 201 {
		t.Fatalf("admission %d %s", status, raw)
	}
	code, e := ec.CodeAt(ctx, ep, nil)
	requireOK(t, e)
	start, e := ec.BlockNumber(ctx)
	requireOK(t, e)
	c := Config{ChainID: chain, EntryPoint: ep, Sender: from, Beneficiary: from, CodeHash: crypto.Keccak256Hash(code), StartBlock: start, Confirmations: 2, MaxGas: 10000000, MaxFee: big.NewInt(4000000000), MaxBytes: 32768, MaxOps: 16, PoolSize: 100, Poll: 200 * time.Millisecond}
	code, e = ec.CodeAt(ctx, a.Sender, nil)
	requireOK(t, e)
	approved := []common.Hash{crypto.Keccak256Hash(code)}
	args, e = factoryABI.Pack("accountImplementation")
	requireOK(t, e)
	code, e = ec.CodeAt(ctx, common.BytesToAddress(call(factory, args)), nil)
	requireOK(t, e)
	approved = append(approved, crypto.Keccak256Hash(code))
	code, e = ec.CodeAt(ctx, pm, nil)
	requireOK(t, e)
	approved = append(approved, crypto.Keccak256Hash(code))
	v, e := LoadManagedValidation(client, c, filepath.Join(dir, "EntryPointSimulations.json"), approved)
	requireOK(t, e)
	driver, dsn := "sqlite", filepath.Join(root, "bundler.db")
	if pg := os.Getenv("TEST_PG_DSN"); pg != "" {
		driver, dsn = "pgx", isolatedSchema(t, pg)
	}
	db, e := Open(ctx, driver, dsn, ep.Hex())
	requireOK(t, e)
	defer db.Close()
	broad := HTTPBroadcaster{URL: base, Token: business, Chain: "999", HTTP: http.DefaultClient}
	S, e := New(c, db, EVM{client}, v, sign, broad)
	requireOK(t, e)
	api := httptest.NewServer(S.Handler([]string{business}, "unused"))
	defer api.Close()
	wallet := func(method string, params ...any) json.RawMessage {
		t.Helper()
		st, r := localPost(t, api.URL+"/rpc", business, map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		var answer struct {
			Result json.RawMessage
			Error  *RPCError
		}
		requireOK(t, json.Unmarshal(r, &answer))
		if st != 200 || answer.Error != nil {
			t.Fatalf("%s: %s", method, r)
		}
		return answer.Result
	}
	wallet("eth_estimateUserOperationGas", a, ep)
	for _, op := range []Operation{a, bop, a} {
		wallet("eth_sendUserOperation", op, ep)
	}
	requireOK(t, S.cycle(ctx, "indexer", S.index))
	requireOK(t, S.cycle(ctx, "build", S.build))
	var bundles, members int
	requireOK(t, db.db.QueryRow("SELECT count(*) FROM bundles").Scan(&bundles))
	requireOK(t, db.db.QueryRow("SELECT count(*) FROM members").Scan(&members))
	if bundles != 1 || members != 2 {
		t.Fatalf("bundles=%d members=%d", bundles, members)
	}
	// A second process opens the same store after signing, before the first send.
	db2, e := Open(ctx, driver, dsn, ep.Hex())
	requireOK(t, e)
	defer db2.Close()
	resumed, e := New(c, db2, EVM{client}, v, sign, broad)
	requireOK(t, e)
	var snapshot string
	requireOK(t, client.CallContext(ctx, &snapshot, "evm_snapshot"))
	lost := &loseFirstAcknowledgement{Broadcaster: broad}
	resumed.Broadcast = lost
	if e = resumed.cycle(ctx, "delivery", resumed.deliver); !errors.Is(e, io.ErrUnexpectedEOF) {
		t.Fatalf("expected lost acknowledgement, got %v", e)
	}
	_, e = db.db.Exec("UPDATE bundles SET due=0")
	requireOK(t, e)
	requireOK(t, resumed.cycle(ctx, "delivery", resumed.deliver))
	if lost.posts != 1 {
		t.Fatal("duplicate broadcast after lost response")
	}
	for i := 0; i < 80; i++ {
		requireOK(t, resumed.cycle(ctx, "indexer", resumed.index))
		var n int
		requireOK(t, db.db.QueryRow("SELECT count(*) FROM receipts").Scan(&n))
		if n == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, item := range []struct {
		o  Operation
		ok bool
	}{{a, true}, {bop, false}} {
		raw := wallet("eth_getUserOperationReceipt", item.o.Hash(ep, chain))
		var r *OpReceipt
		requireOK(t, json.Unmarshal(raw, &r))
		if r == nil || r.Success != item.ok || r.Cost.ToInt().Sign() == 0 {
			t.Fatalf("incorrect receipt: %s", raw)
		}
	}
	var hash string
	requireOK(t, db.db.QueryRow("SELECT hash FROM bundles").Scan(&hash))
	for i := 0; i < 80; i++ {
		view, exists, e := broad.Lookup(ctx, common.HexToHash(hash))
		requireOK(t, e)
		if exists && view.Outcome == "INCLUDED" {
			_, e = db.db.Exec("UPDATE bundles SET due=0 WHERE hash=$1", hash)
			requireOK(t, e)
			requireOK(t, resumed.cycle(ctx, "delivery", resumed.deliver))
			var ok bool
			requireOK(t, client.CallContext(ctx, &ok, "evm_revert", snapshot))
			if !ok {
				t.Fatal("snapshot rewind failed")
			}
			requireOK(t, client.CallContext(ctx, nil, "evm_mine"))
			requireOK(t, resumed.cycle(ctx, "indexer", resumed.index))
			if string(wallet("eth_getUserOperationReceipt", a.Hash(ep, chain))) != "null" {
				t.Fatal("orphaned receipt still visible")
			}
			requireOK(t, resumed.cycle(ctx, "build", resumed.build))
			requireOK(t, resumed.cycle(ctx, "delivery", resumed.deliver))
			for j := 0; j < 80; j++ {
				requireOK(t, resumed.cycle(ctx, "indexer", resumed.index))
				var count int
				requireOK(t, db.db.QueryRow("SELECT count(*) FROM receipts").Scan(&count))
				if count == 2 {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if string(wallet("eth_getUserOperationReceipt", a.Hash(ep, chain))) == "null" {
				t.Fatal("orphaned operation was not recovered on the new fork")
			}
			if os.Getenv("TEST_PROFILE") == "1" {
				profile(t, S, api.URL, business, buildOp)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("Broadcaster did not report inclusion")
}

type loseFirstAcknowledgement struct {
	Broadcaster
	posts int
}

func (b *loseFirstAcknowledgement) Submit(ctx context.Context, raw string, h common.Hash) (BroadcastView, error) {
	v, e := b.Broadcaster.Submit(ctx, raw, h)
	b.posts++
	if e == nil && b.posts == 1 {
		return v, io.ErrUnexpectedEOF
	}
	return v, e
}

// Use an isolated schema so repeated HA tests never touch existing tables.
func isolatedSchema(t *testing.T, dsn string) string {
	t.Helper()
	u, e := url.Parse(dsn)
	requireOK(t, e)
	admin, e := sql.Open("pgx", dsn)
	requireOK(t, e)
	schema := fmt.Sprintf("bundler_test_%d", time.Now().UnixNano())
	_, e = admin.Exec("CREATE SCHEMA " + schema)
	requireOK(t, e)
	t.Cleanup(func() {
		_, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
