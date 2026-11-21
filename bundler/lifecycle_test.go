package bundler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func example(n uint64) Operation {
	data := hexutil.Bytes{1, 2}
	sig := hexutil.Bytes{3}
	return Operation{Sender: common.HexToAddress("0x1234"), Nonce: quantity(n), CallData: &data, CallGasLimit: quantity(100000), VerificationGasLimit: quantity(100000), PreVerificationGas: quantity(65000), MaxFeePerGas: quantity(100), MaxPriorityFeePerGas: quantity(10), Signature: &sig}
}
func testDB(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ops.db") + "?_pragma=busy_timeout(5000)&_txlock=immediate"
	db, e := Open(t.Context(), "sqlite", path, "test")
	requireOK(t, e)
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}
func TestV07PackedEnvelope(t *testing.T) {
	op := example(3)
	addr := common.HexToAddress("0x4321")
	op.Factory = &addr
	op.FactoryData = op.CallData
	op.Paymaster = &addr
	op.PaymasterData = op.CallData
	op.PaymasterVerificationGasLimit = quantity(12)
	op.PaymasterPostOpGasLimit = quantity(13)
	restored, e := envelope(op.pack())
	requireOK(t, e)
	if string(mustJSON(op)) != string(mustJSON(restored)) {
		t.Fatal("packed operation did not round-trip")
	}
	data, e := calldata([]Operation{op}, addr)
	requireOK(t, e)
	if hex.EncodeToString(data[:4]) != "765e827f" {
		t.Fatal("incorrect v0.7 selector")
	}
	h := op.Hash(addr, big.NewInt(1))
	sig := hexutil.Bytes{9}
	op.Signature = &sig
	if op.Hash(addr, big.NewInt(1)) != h || op.Hash(addr, big.NewInt(10)) == h {
		t.Fatal("hash domain or signature handling incorrect")
	}
	raw := strings.Replace(string(mustJSON(op)), "\"nonce\":\"0x3\"", "\"nonce\":\"0x03\"", 1)
	if _, e = decodeOp(json.RawMessage(raw), false); e == nil {
		t.Fatal("accepted noncanonical quantity")
	}
}
func TestConcurrentAdmissionAndPaymasterBudget(t *testing.T) {
	s, path := testDB(t)
	s2, e := Open(t.Context(), "sqlite", path, "test")
	requireOK(t, e)
	defer s2.Close()
	tkt := Admission{Prefund: quantity(80), Deposit: quantity(100)}
	ep := common.HexToAddress("0x789")
	id := big.NewInt(1)
	a := example(0)
	a.Paymaster = &ep
	a.PaymasterData = a.CallData
	a.PaymasterPostOpGasLimit = quantity(10)
	a.PaymasterVerificationGasLimit = quantity(20)
	other := a
	other.Nonce = quantity(1)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, it := range []Operation{a, other} {
		wg.Go(func() { db := []*Store{s, s2}[i]; results <- db.admit(t.Context(), it.Hash(ep, id).Hex(), it, tkt, 16) })
	}
	wg.Wait()
	close(results)
	accepted, refused := 0, 0
	for err := range results {
		if err == nil {
			accepted++
		} else {
			var f *RPCError
			if !errors.As(err, &f) || f.Code != -32508 {
				t.Fatal(err)
			}
			refused++
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatal(accepted, refused)
	}
	var body string
	requireOK(t, s.db.QueryRow("SELECT body FROM operations").Scan(&body))
	var old Operation
	requireOK(t, json.Unmarshal([]byte(body), &old))
	improved := old
	improved.MaxFeePerGas = quantity(109)
	improved.MaxPriorityFeePerGas = quantity(12)
	if e = s.admit(t.Context(), improved.Hash(ep, id).Hex(), improved, tkt, 16); e == nil {
		t.Fatal("underpriced replacement")
	}
	improved.MaxFeePerGas = quantity(110)
	requireOK(t, s.admit(t.Context(), improved.Hash(ep, id).Hex(), improved, tkt, 16))
	for range 2 {
		requireOK(t, s.admit(t.Context(), improved.Hash(ep, id).Hex(), improved, tkt, 16))
	}
	var stage string
	requireOK(t, s.db.QueryRow("SELECT stage FROM operations WHERE hash=$1", old.Hash(ep, id).Hex()).Scan(&stage))
	if stage != "REPLACED" {
		t.Fatal(stage)
	}
}
func TestExpiredLeaseFencesFormerWorker(t *testing.T) {
	db, path := testDB(t)
	peer, e := Open(t.Context(), "sqlite", path, "test")
	requireOK(t, e)
	defer peer.Close()
	one, ok, e := db.claim(t.Context(), "build")
	requireOK(t, e)
	if !ok {
		t.Fatal("first claim failed")
	}
	_, ok, e = peer.claim(t.Context(), "build")
	requireOK(t, e)
	if ok {
		t.Fatal("simultaneous lease")
	}
	_, e = db.db.Exec("UPDATE leases SET until_ms=0 WHERE name='build'")
	requireOK(t, e)
	two, ok, e := peer.claim(t.Context(), "build")
	requireOK(t, e)
	if !ok {
		t.Fatal("lease not recoverable")
	}
	err := db.tx(t.Context(), func(tx *sql.Tx) error { return one.fence(t.Context(), tx) })
	if !errors.Is(err, errFenced) {
		t.Fatal(err)
	}
	db.release(t.Context(), one)
	requireOK(t, peer.tx(t.Context(), func(tx *sql.Tx) error { return two.fence(t.Context(), tx) }))
}
func TestWebhookOnlyWakesKnownTask(t *testing.T) {
	db, _ := testDB(t)
	s := Service{Store: db, Config: Config{ChainID: big.NewInt(1)}, wake: make(chan struct{}, 1)}
	_, e := db.db.Exec("INSERT INTO bundles(hash,raw,nonce,stage,due) VALUES('abc','00','0','SIGNED',500)")
	requireOK(t, e)
	hook := s.Handler([]string{"client"}, "test secret")
	body := `{"eventId":"1","data":{"network":"1","activeHash":"abc","taskState":"CLOSED","taskOutcome":"INCLUDED"}}`
	mac := hmac.New(sha256.New, []byte("test secret"))
	mac.Write([]byte(body))
	for _, c := range []struct {
		sig  string
		want int
	}{{"00", 401}, {hex.EncodeToString(mac.Sum(nil)), 204}, {hex.EncodeToString(mac.Sum(nil)), 204}} {
		r := httptest.NewRequest("POST", "/webhooks/broadcaster", strings.NewReader(body))
		r.Header.Set("X-Broadcaster-Signature", c.sig)
		w := httptest.NewRecorder()
		hook.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Fatal(w.Code)
		}
	}
	var stage string
	var due int64
	requireOK(t, db.db.QueryRow("SELECT stage,due FROM bundles").Scan(&stage, &due))
	if stage != "SIGNED" || due != 0 {
		t.Fatal(stage, due)
	}
}

type corruptSigner struct{ Signer }

func (s corruptSigner) Sign(ctx context.Context, tx *types.Transaction, from common.Address) (*types.Transaction, error) {
	return s.Signer.Sign(ctx, types.NewTx(&types.DynamicFeeTx{ChainID: tx.ChainId(), Nonce: tx.Nonce() + 1, To: tx.To(), Gas: tx.Gas(), GasFeeCap: tx.GasFeeCap(), GasTipCap: tx.GasTipCap()}), from)
}
func TestSignerCannotChangeNonce(t *testing.T) {
	key, e := crypto.GenerateKey()
	requireOK(t, e)
	a := crypto.PubkeyToAddress(key.PublicKey)
	cfg := Config{Sender: a, ChainID: big.NewInt(1)}
	s := Service{Config: cfg, Signer: corruptSigner{testSigner{key}}}
	tx := types.NewTx(&types.DynamicFeeTx{ChainID: cfg.ChainID, To: &a, Gas: 22000, GasFeeCap: big.NewInt(2), GasTipCap: big.NewInt(1)})
	if _, _, e = s.sign(t.Context(), tx); e == nil {
		t.Fatal("signed different intent")
	}
}
func TestProviderChecksResponseHash(t *testing.T) {
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"network":"1","txHash":"wrong","taskId":"id"}`))
	}))
	defer h.Close()
	b := HTTPBroadcaster{URL: h.URL, Chain: "1", HTTP: h.Client()}
	if _, e := b.Submit(t.Context(), "0x00", common.Hash{}); e == nil {
		t.Fatal("wrong tx hash accepted")
	}
}

type fakeCaller func(context.Context, any, string, ...any) error

func (f fakeCaller) CallContext(ctx context.Context, out any, method string, args ...any) error {
	return f(ctx, out, method, args...)
}
func TestFailoverRequiresVerifiedChain(t *testing.T) {
	cfg := Config{ChainID: big.NewInt(1), CodeHash: crypto.Keccak256Hash([]byte{1})}
	calls := 0
	endpoint := func(chain uint64, offline bool) Caller {
		return fakeCaller(func(ctx context.Context, out any, method string, args ...any) error {
			switch method {
			case "eth_chainId":
				*out.(*hexutil.Big) = *quantity(chain)
			case "eth_getCode":
				*out.(*hexutil.Bytes) = hexutil.Bytes{1}
			default:
				if chain != 1 {
					t.Fatal("accepted wrong network")
				}
				if offline {
					return errors.New("transport unavailable")
				}
				calls++
				*out.(*string) = "ok"
			}
			return nil
		})
	}
	pool := NewReadPool(cfg, []Caller{endpoint(1, true), endpoint(2, false), endpoint(1, false)})
	var got string
	requireOK(t, pool.CallContext(t.Context(), &got, "test"))
	requireOK(t, pool.CallContext(t.Context(), &got, "test"))
	if got != "ok" || calls != 2 {
		t.Fatal(got, calls)
	}
}

type replacementChain struct{ Chain }

func (replacementChain) Header(context.Context, string) (Header, error) {
	return Header{BaseFee: quantity(25)}, nil
}
func (replacementChain) Price(context.Context) (*big.Int, error) {
	return big.NewInt(2), nil
}
func (replacementChain) Simulate(context.Context, Header, map[string]any) (uint64, error) {
	return 22_000, nil
}

// The first delivery can reach Broadcaster even if the response is lost.
// A new worker must reuse the saved successor, including its fee and signature.
func TestFeeReplacementRecoversExactSignedBytes(t *testing.T) {
	store, dbPath := testDB(t)
	key, err := crypto.GenerateKey()
	requireOK(t, err)
	cfg := Config{ChainID: big.NewInt(1), Sender: crypto.PubkeyToAddress(key.PublicKey), MaxFee: big.NewInt(100)}
	cfg.EntryPoint = common.HexToAddress("0x1234")
	s := Service{Config: cfg, Chain: replacementChain{}, Store: store, Signer: testSigner{key}}
	rootTx, raw, err := s.sign(t.Context(), types.NewTx(&types.DynamicFeeTx{
		ChainID: cfg.ChainID, Nonce: 42, To: &cfg.EntryPoint, Gas: 50_000, GasFeeCap: big.NewInt(20), GasTipCap: big.NewInt(1),
	}))
	requireOK(t, err)
	root := rootTx.Hash().Hex()
	var first string
	var deliveries int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/EVM/transactions/"+root+"/replacement" || r.Method != "POST" {
			t.Error("wrong replacement route", r.Method, r.URL)
		}
		var req struct {
			Raw string `json:"rawTransaction"`
		}
		requireOK(t, json.NewDecoder(r.Body).Decode(&req))
		var successor types.Transaction
		bytes, err := hexutil.Decode(req.Raw)
		requireOK(t, err)
		requireOK(t, successor.UnmarshalBinary(bytes))
		if successor.Nonce() != 42 || successor.GasFeeCap().Cmp(big.NewInt(27)) != 0 || successor.GasTipCap().Cmp(big.NewInt(2)) != 0 {
			t.Error("invalid fee replacement")
		}
		deliveries++
		if deliveries == 1 {
			first = req.Raw
			w.WriteHeader(503)
			return
		}
		if first != req.Raw {
			t.Error("retry changed signed transaction")
		}
		_ = json.NewEncoder(w).Encode(BroadcastView{Network: "1", Task: "one", Hash: successor.Hash().Hex()})
	}))
	defer api.Close()
	s.Broadcast = HTTPBroadcaster{URL: api.URL, Chain: "1", HTTP: api.Client()}
	run := func(s *Service) error {
		return s.cycle(t.Context(), "delivery", func(ctx context.Context, l lease) error {
			return s.replaceOuter(ctx, l, root, raw, BroadcastView{Active: root})
		})
	}
	if err = run(&s); err == nil {
		t.Fatal("expected lost acknowledgement")
	}
	peer, err := Open(t.Context(), "sqlite", dbPath, "test")
	requireOK(t, err)
	defer peer.Close()
	requireOK(t, run(&Service{Store: peer, Config: cfg, Broadcast: s.Broadcast}))
	if deliveries != 2 {
		t.Fatal("missing replacement retry")
	}
}
