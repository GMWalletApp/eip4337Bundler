package bundler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

type request struct {
	Version string            `json:"jsonrpc"`
	ID      json.RawMessage   `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
}

func respond(w http.ResponseWriter, id json.RawMessage, result any, e error) {
	body := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	if e != nil {
		var f *RPCError
		if !errors.As(e, &f) {
			f = &RPCError{Code: -32000, Message: "upstream or storage temporarily unavailable"}
		}
		delete(body, "result")
		body["error"] = f
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(body)
}
func (s *Service) Handler(tokens []string, secret string) http.Handler {
	mux := http.NewServeMux()
	limits := make([]chan struct{}, len(tokens))
	for i := range limits {
		limits[i] = make(chan struct{}, 8)
	}
	authenticate := func(r *http.Request) chan struct{} {
		for i, key := range tokens {
			if key != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+key)) == 1 {
				return limits[i]
			}
		}
		return nil
	}
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		if authenticate(r) == nil {
			http.Error(w, "unauthorized", 401)
			return
		}
		s.metricsHTTP(w, r)
	})
	mux.HandleFunc("POST /rpc", func(w http.ResponseWriter, r *http.Request) {
		quota := authenticate(r)
		if quota == nil {
			http.Error(w, "unauthorized", 401)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req request
		d := json.NewDecoder(r.Body)
		if e := d.Decode(&req); e != nil || d.Decode(new(any)) != io.EOF {
			respond(w, nil, nil, &RPCError{Code: -32700, Message: "invalid JSON"})
			return
		}
		if req.Version != "2.0" || req.Method == "" {
			respond(w, req.ID, nil, &RPCError{Code: -32600, Message: "invalid request"})
			return
		}
		if req.Method == "eth_sendUserOperation" || req.Method == "eth_estimateUserOperationGas" {
			select {
			case quota <- struct{}{}:
				defer func() { <-quota }()
			default:
				w.Header().Set("Retry-After", "1")
				http.Error(w, "client concurrency limit", 429)
				return
			}
			select {
			case s.simulations <- struct{}{}:
				defer func() { <-s.simulations }()
			default:
				w.Header().Set("Retry-After", "1")
				http.Error(w, "simulation capacity exhausted", 429)
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		start := time.Now()
		v, e := s.call(ctx, req)
		switch req.Method {
		case "eth_sendUserOperation":
			s.stats.admit.observe(time.Since(start))
		case "eth_getUserOperationReceipt":
			s.stats.lookup.observe(time.Since(start))
		}
		respond(w, req.ID, v, e)
	})
	mux.HandleFunc("POST /webhooks/broadcaster", func(w http.ResponseWriter, r *http.Request) {
		body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		sig, decode := hex.DecodeString(r.Header.Get("X-Broadcaster-Signature"))
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		if e != nil || secret == "" || decode != nil || !hmac.Equal(sig, mac.Sum(nil)) {
			http.Error(w, "invalid signature", 401)
			return
		}
		var event struct {
			ID   string `json:"eventId"`
			Data struct {
				Network string `json:"network"`
				Active  string `json:"activeHash"`
				Task    string `json:"taskId"`
			}
		}
		if json.Unmarshal(body, &event) != nil || event.ID == "" || event.Data.Network != s.Config.ChainID.String() {
			http.Error(w, "invalid event", 400)
			return
		}
		// Callbacks are hints. Replays and out-of-order events never change chain facts.
		_, e = s.Store.db.ExecContext(r.Context(), "UPDATE bundles SET due=0 WHERE stage IN ('SIGNED','SUBMITTED') AND (hash=$1 OR task=$2 AND task<>'')", event.Data.Active, event.Data.Task)
		if e != nil {
			http.Error(w, "storage unavailable", 503)
			return
		}
		s.signal()
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		h, e := s.Chain.Header(ctx, "latest")
		if e != nil || s.caughtUp(ctx, h) != nil {
			http.Error(w, "catching up", 503)
			return
		}
		w.WriteHeader(204)
	})
	return mux
}
func (s *Service) call(ctx context.Context, r request) (any, error) {
	switch r.Method {
	case "eth_chainId", "eth_supportedEntryPoints":
		if len(r.Params) != 0 {
			return nil, invalid("expected empty params")
		}
		if r.Method == "eth_chainId" {
			return hexutil.EncodeBig(s.Config.ChainID), nil
		}
		return []common.Address{s.Config.EntryPoint}, nil
	case "eth_sendUserOperation", "eth_estimateUserOperationGas":
		if len(r.Params) != 2 {
			return nil, invalid("expected operation and EntryPoint")
		}
		var ep common.Address
		if json.Unmarshal(r.Params[1], &ep) != nil || ep != s.Config.EntryPoint {
			return nil, invalid("unsupported EntryPoint")
		}
		estimate := r.Method == "eth_estimateUserOperationGas"
		op, e := decodeOp(r.Params[0], estimate)
		if e != nil {
			return nil, e
		}
		if estimate {
			return s.Validator.Estimate(ctx, op, ep)
		}
		return s.admit(ctx, op)
	case "eth_getUserOperationByHash", "eth_getUserOperationReceipt", "gm_getUserOperationStatus":
		var hash common.Hash
		if len(r.Params) != 1 || json.Unmarshal(r.Params[0], &hash) != nil {
			return nil, invalid("expected operation hash")
		}
		if r.Method == "gm_getUserOperationStatus" {
			return s.status(ctx, hash)
		}
		return s.lookup(ctx, hash, r.Method == "eth_getUserOperationReceipt")
	default:
		return nil, &RPCError{Code: -32601, Message: "method not found"}
	}
}
func (s *Service) lookup(ctx context.Context, hash common.Hash, receiptOnly bool) (any, error) {
	head, e := s.Chain.Header(ctx, "latest")
	if e != nil {
		return nil, e
	}
	if e = s.caughtUp(ctx, head); e != nil {
		return nil, e
	}
	var raw string
	var outer string
	var mined *OpReceipt
	e = s.Store.db.QueryRowContext(ctx, "SELECT o.body,t.body FROM receipts o JOIN tx_receipts t ON o.tx_hash=t.hash WHERE o.hash=$1", hash.Hex()).Scan(&raw, &outer)
	if e == nil {
		var v OpReceipt
		if e = json.Unmarshal([]byte(raw), &v); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(outer), &v.Receipt); e != nil {
			return nil, e
		}
		if v.Receipt == nil || v.Receipt.BlockNumber == nil {
			return nil, errors.New("invalid stored receipt")
		}
		h, e := s.Chain.Header(ctx, hexutil.EncodeBig(v.Receipt.BlockNumber))
		if e != nil {
			return nil, e
		}
		if h.Hash == v.Receipt.BlockHash {
			mined = &v
		}
	} else if !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	if receiptOnly {
		return mined, nil
	}
	var op Operation
	var stage string
	e = s.Store.db.QueryRowContext(ctx, "SELECT body,stage FROM operations WHERE hash=$1", hash.Hex()).Scan(&raw, &stage)
	if errors.Is(e, sql.ErrNoRows) {
		if mined == nil {
			return nil, nil
		}
		foreign, err := s.Chain.Operation(ctx, mined, s.Config.ChainID)
		if err != nil || foreign == nil {
			return nil, err
		}
		op = *foreign
	} else if e != nil {
		return nil, e
	} else if e = json.Unmarshal([]byte(raw), &op); e != nil {
		return nil, e
	}
	if mined == nil && stage != "QUEUED" && stage != "BUNDLED" {
		return nil, nil
	}
	result := map[string]any{"userOperation": op, "entryPoint": s.Config.EntryPoint, "blockNumber": nil, "blockHash": nil, "transactionHash": nil}
	if mined != nil {
		result["blockNumber"] = hexutil.EncodeBig(mined.Receipt.BlockNumber)
		result["blockHash"] = mined.Receipt.BlockHash
		result["transactionHash"] = mined.Receipt.TxHash
	}
	return result, nil
}

func (s *Service) status(ctx context.Context, hash common.Hash) (any, error) {
	var state, reason, bundle, delivery, task, observation string
	e := s.Store.db.QueryRowContext(ctx, `SELECT o.stage,o.reason,COALESCE(o.bundle,''),COALESCE(b.stage,''),COALESCE(b.task,''),COALESCE(b.observation,'') FROM operations o LEFT JOIN bundles b ON o.bundle=b.hash WHERE o.hash=$1`, hash.Hex()).Scan(&state, &reason, &bundle, &delivery, &task, &observation)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	result := map[string]any{"userOpHash": hash, "state": state}
	if reason != "" {
		result["reason"] = reason
	}
	if bundle != "" {
		result["bundleHash"] = bundle
		result["broadcastState"] = delivery
	}
	if task != "" {
		result["broadcasterTaskId"] = task
	}
	var view BroadcastView
	if observation != "" && json.Unmarshal([]byte(observation), &view) == nil && view.Task != "" {
		result["broadcast"] = view
	}
	return result, nil
}
