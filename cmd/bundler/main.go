package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	b "github.com/GMWalletApp/eip4337Bundler/bundler"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
)

type settings struct {
	BackupExecutionEnvs    []string
	Listen                 string
	DBDriver               string
	DBEnv                  string
	ChainID                string
	EntryPoint             string
	CodeHash               string
	Sender                 string
	Beneficiary            string
	MaxFeeWei              string
	ExecutionEnv           string
	ValidationEnv          string
	SignerEnv              string
	BroadcasterEnv         string
	BroadcastTokenEnv      string
	WebhookSecretEnv       string
	SimulationArtifact     string
	StartBlock             *uint64
	Confirmations          uint64
	MaxGas                 uint64
	MaxOps                 int
	MaxBytes               int
	PoolSize               int
	PollMillis             int
	ClientTokenEnvs        []string
	ApprovedValidationCode []common.Hash
}

func secret(name string) (string, error) {
	v := os.Getenv(name)
	if name == "" || v == "" {
		return "", fmt.Errorf("required environment variable %q is unset", name)
	}
	return v, nil
}
func endpoint(name string) (string, error) {
	v, e := secret(name)
	if e != nil {
		return "", e
	}
	u, e := url.Parse(v)
	if e != nil || u.Host == "" || u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return "", fmt.Errorf("invalid HTTPS or loopback endpoint in %s", name)
	}
	return strings.TrimRight(v, "/"), nil
}
func run(ctx context.Context, path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	var c settings
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return e
	}
	chain, ok := new(big.Int).SetString(c.ChainID, 10)
	if !ok {
		return errors.New("invalid chainId")
	}
	fee, ok := new(big.Int).SetString(c.MaxFeeWei, 10)
	if !ok {
		return errors.New("invalid fee budget")
	}
	var code common.Hash
	if code.UnmarshalText([]byte(c.CodeHash)) != nil || code == (common.Hash{}) || !common.IsHexAddress(c.EntryPoint) || !common.IsHexAddress(c.Sender) || !common.IsHexAddress(c.Beneficiary) || c.StartBlock == nil {
		return errors.New("invalid chain deployment")
	}
	cfg := b.Config{ChainID: chain, EntryPoint: common.HexToAddress(c.EntryPoint), Beneficiary: common.HexToAddress(c.Beneficiary), Sender: common.HexToAddress(c.Sender), CodeHash: code, StartBlock: *c.StartBlock, Confirmations: c.Confirmations, MaxOps: c.MaxOps, MaxBytes: c.MaxBytes, PoolSize: c.PoolSize, MaxGas: c.MaxGas, MaxFee: fee, Poll: time.Duration(c.PollMillis) * time.Millisecond}
	var clients []*rpc.Client
	defer func() {
		for _, cl := range clients {
			cl.Close()
		}
	}()
	for _, name := range []string{c.ExecutionEnv, c.SignerEnv, c.ValidationEnv} {
		if name == "" && len(clients) == 2 {
			break
		}
		u, e := endpoint(name)
		if e != nil {
			return e
		}
		cl, e := rpc.DialContext(ctx, u)
		if e != nil {
			return fmt.Errorf("cannot connect to %s", name)
		}
		clients = append(clients, cl)
	}
	readers := []b.Caller{clients[0]}
	for _, name := range c.BackupExecutionEnvs {
		url, err := endpoint(name)
		if err != nil {
			return err
		}
		client, err := rpc.DialContext(ctx, url)
		if err != nil {
			return fmt.Errorf("cannot connect to %s", name)
		}
		clients = append(clients, client)
		readers = append(readers, client)
	}
	pool := b.NewReadPool(cfg, readers)
	evm := b.EVM{RPC: pool}
	check, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, e = evm.Header(check, "latest"); e != nil {
		return fmt.Errorf("no verified execution reader: %w", e)
	}
	u, e := endpoint(c.BroadcasterEnv)
	if e != nil {
		return e
	}
	token, e := secret(c.BroadcastTokenEnv)
	if e != nil {
		return e
	}
	hook, e := secret(c.WebhookSecretEnv)
	if e != nil {
		return e
	}
	if len(hook) < 24 {
		return errors.New("webhook secret too short")
	}
	broad := b.HTTPBroadcaster{URL: u, Token: token, Chain: chain.String(), HTTP: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	var tokens []string
	for _, name := range c.ClientTokenEnvs {
		v, e := secret(name)
		if e != nil {
			return e
		}
		if len(v) < 24 {
			return errors.New("client token too short")
		}
		tokens = append(tokens, v)
	}
	if len(tokens) == 0 {
		return errors.New("configure at least one client")
	}
	dsn, e := secret(c.DBEnv)
	if e != nil {
		return e
	}
	domain := fmt.Sprintf("%s/%s/%s/%s/%s/%d", chain, cfg.EntryPoint, cfg.CodeHash, cfg.Sender, cfg.Beneficiary, cfg.StartBlock)
	db, e := b.Open(ctx, c.DBDriver, dsn, domain)
	if e != nil {
		return e
	}
	defer db.Close()
	var validator b.Validator
	if c.ValidationEnv != "" {
		validator = b.RemoteValidation{RPC: clients[2]}
	} else {
		validator, e = b.LoadManagedValidation(pool, cfg, c.SimulationArtifact, c.ApprovedValidationCode)
		if e != nil {
			return e
		}
	}
	svc, e := b.New(cfg, db, evm, validator, b.RPCSigner{RPC: clients[1]}, broad)
	if e != nil {
		return e
	}
	workerCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); svc.Run(workerCtx) }()
	defer func() { stop(); <-done }()
	if c.Listen == "" {
		c.Listen = "127.0.0.1:4337"
	}
	srv := http.Server{Addr: c.Listen, Handler: svc.Handler(tokens, hook), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 25 * time.Second, IdleTimeout: 60 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case e := <-errCh:
		return e
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
func main() {
	cfg := flag.String("config", "config.json", "deployment settings")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if e := run(ctx, *cfg); e != nil {
		slog.Error("bundler stopped", "error", e)
		os.Exit(1)
	}
}
