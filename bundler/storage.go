package bundler

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings (name TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS leases (name TEXT PRIMARY KEY, token TEXT NOT NULL DEFAULT '', until_ms BIGINT NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS bundles (hash TEXT PRIMARY KEY, raw TEXT NOT NULL, nonce TEXT NOT NULL, stage TEXT NOT NULL, task TEXT NOT NULL DEFAULT '', observation TEXT NOT NULL DEFAULT '', due BIGINT NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS bundle_nonce_live ON bundles(nonce) WHERE stage IN ('SIGNED','SUBMITTED');
CREATE INDEX IF NOT EXISTS bundle_due ON bundles(stage,due);
CREATE TABLE IF NOT EXISTS operations (hash TEXT PRIMARY KEY, sender TEXT NOT NULL, nonce TEXT NOT NULL, body TEXT NOT NULL, stage TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', bundle TEXT, queued BIGINT NOT NULL, due BIGINT NOT NULL DEFAULT 0, height BIGINT, block_hash TEXT, paymaster TEXT NOT NULL, prefund TEXT NOT NULL, valid_until BIGINT NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX IF NOT EXISTS live_slot ON operations(sender,nonce) WHERE stage IN ('QUEUED','BUNDLED','INCLUDED','FINAL');
CREATE INDEX IF NOT EXISTS ops_paymaster ON operations(paymaster) WHERE stage IN ('QUEUED','BUNDLED');
CREATE INDEX IF NOT EXISTS ops_queue ON operations(stage,due,queued);
CREATE INDEX IF NOT EXISTS ops_height ON operations(height);
CREATE INDEX IF NOT EXISTS ops_bundle ON operations(bundle);
CREATE TABLE IF NOT EXISTS receipts (hash TEXT PRIMARY KEY, body TEXT NOT NULL, height BIGINT NOT NULL, tx_hash TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS tx_receipts (hash TEXT PRIMARY KEY, height BIGINT NOT NULL, body TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS tx_receipts_height ON tx_receipts(height);
CREATE INDEX IF NOT EXISTS receipts_height ON receipts(height);
CREATE TABLE IF NOT EXISTS blocks (height BIGINT PRIMARY KEY, hash TEXT NOT NULL, parent TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS replacements (root TEXT NOT NULL,parent TEXT NOT NULL,hash TEXT PRIMARY KEY,raw TEXT NOT NULL, UNIQUE(root,parent));
CREATE TABLE IF NOT EXISTS members (bundle TEXT NOT NULL, op TEXT NOT NULL, PRIMARY KEY(bundle,op));
`

type Store struct {
	db       *sql.DB
	clockSQL string
}

func Open(ctx context.Context, driver, dsn, domain string) (*Store, error) {
	if driver != "pgx" && driver != "sqlite" {
		return nil, errors.New("database driver must be pgx or sqlite")
	}
	db, e := sql.Open(driver, dsn)
	if e != nil {
		return nil, e
	}
	if driver == "sqlite" {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(24)
	}
	clockSQL := "CAST(unixepoch('subsec')*1000 AS INTEGER)"
	if driver == "pgx" {
		clockSQL = "CAST(EXTRACT(EPOCH FROM clock_timestamp())*1000 AS bigint)"
	}
	s := &Store{db, clockSQL}
	e = s.tx(ctx, func(tx *sql.Tx) error {
		if driver == "pgx" {
			if _, e := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(4337007)"); e != nil {
				return e
			}
		}
		if _, e := tx.ExecContext(ctx, schema); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, "INSERT INTO settings(name,value) VALUES('domain',$1) ON CONFLICT DO NOTHING", domain); e != nil {
			return e
		}
		var saved string
		if e := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE name='domain'").Scan(&saved); e != nil {
			return e
		}
		if saved != domain {
			return errors.New("database belongs to another network, signer or EntryPoint")
		}
		_, e := tx.ExecContext(ctx, "INSERT INTO leases(name) VALUES('build'),('delivery'),('indexer'),('pool') ON CONFLICT DO NOTHING")
		return e
	})
	if e != nil {
		db.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = fn(tx); e != nil {
		return e
	}
	return tx.Commit()
}
func now() int64 { return time.Now().UnixMilli() }
func changed(r sql.Result, e error) error {
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return errFenced
	}
	return nil
}

type lease struct{ name, token, clockSQL string }

func (s *Store) claim(ctx context.Context, name string) (lease, bool, error) {
	var id [16]byte
	if _, e := rand.Read(id[:]); e != nil {
		return lease{}, false, e
	}
	l := lease{name, hex.EncodeToString(id[:]), s.clockSQL}
	r, e := s.db.ExecContext(ctx, "UPDATE leases SET token=$1,until_ms="+s.clockSQL+"+60000 WHERE name=$2 AND until_ms<="+s.clockSQL, l.token, name)
	if e != nil {
		return l, false, e
	}
	n, e := r.RowsAffected()
	return l, n == 1, e
}

// A worker that outlives its lease may finish an RPC call but cannot commit its result.
func (l lease) fence(ctx context.Context, tx *sql.Tx) error {
	return changed(tx.ExecContext(ctx, "UPDATE leases SET token=token WHERE name=$1 AND token=$2 AND until_ms>"+l.clockSQL, l.name, l.token))
}
func (s *Store) locked(ctx context.Context, l lease, fn func(*sql.Tx) error) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if e := l.fence(ctx, tx); e != nil {
			return e
		}
		return fn(tx)
	})
}
func (s *Store) release(ctx context.Context, l lease) {
	_, _ = s.db.ExecContext(ctx, "UPDATE leases SET until_ms=0 WHERE name=$1 AND token=$2", l.name, l.token)
}

type record struct {
	hash, body string
	op         Operation
}

func (s *Store) candidates(ctx context.Context, limit int) ([]record, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT hash,body FROM operations WHERE stage='QUEUED' AND due<=$1 ORDER BY due,queued,hash LIMIT $2", now(), limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []record
	for rows.Next() {
		var r record
		if e := rows.Scan(&r.hash, &r.body); e != nil {
			return nil, e
		}
		if e := json.Unmarshal([]byte(r.body), &r.op); e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) admit(ctx context.Context, hash string, op Operation, ticket Admission, capacity int) error {
	body, e := json.Marshal(op)
	if e != nil {
		return e
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		// Serialize short pool mutations across replicas, including paymaster reservations.
		if _, e := tx.ExecContext(ctx, "UPDATE leases SET token=token WHERE name='pool'"); e != nil {
			return e
		}
		var existing string
		e := tx.QueryRowContext(ctx, "SELECT stage FROM operations WHERE hash=$1", hash).Scan(&existing)
		if e == nil {
			return duplicateStatus(existing)
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		var oldHash, oldBody, stage string
		e = tx.QueryRowContext(ctx, "SELECT hash,body,stage FROM operations WHERE sender=$1 AND nonce=$2 AND stage IN ('QUEUED','BUNDLED','INCLUDED','FINAL')", op.Sender.Hex(), op.Nonce.String()).Scan(&oldHash, &oldBody, &stage)
		if e == nil {
			var old Operation
			if e = json.Unmarshal([]byte(oldBody), &old); e != nil {
				return e
			}
			if stage != "QUEUED" || !bumpEnough(old.MaxFeePerGas, op.MaxFeePerGas) || !bumpEnough(old.MaxPriorityFeePerGas, op.MaxPriorityFeePerGas) {
				return invalid("nonce occupied; replacement requires a queued operation and a 10% increase in both fees")
			}
			if e = changed(tx.ExecContext(ctx, "UPDATE operations SET stage='REPLACED',reason=$1 WHERE hash=$2 AND stage='QUEUED'", hash, oldHash)); e != nil {
				return e
			}
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		var size int
		if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM operations WHERE stage IN ('QUEUED','BUNDLED')").Scan(&size); e != nil {
			return e
		}
		if size >= capacity {
			return &RPCError{Code: -32005, Message: "operation pool full"}
		}
		pm := ""
		if op.Paymaster != nil {
			pm = op.Paymaster.Hex()
			remaining := ticket.Deposit.ToInt()
			remaining = new(big.Int).Set(remaining)
			remaining.Sub(remaining, ticket.Prefund.ToInt())
			rows, e := tx.QueryContext(ctx, "SELECT prefund FROM operations WHERE paymaster=$1 AND stage IN ('QUEUED','BUNDLED')", pm)
			if e != nil {
				return e
			}
			for rows.Next() {
				var raw string
				if e = rows.Scan(&raw); e != nil {
					rows.Close()
					return e
				}
				n, ok := new(big.Int).SetString(raw, 10)
				if !ok {
					rows.Close()
					return fmt.Errorf("invalid stored prefund")
				}
				remaining.Sub(remaining, n)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return e
			}
			if remaining.Sign() < 0 {
				return &RPCError{Code: -32508, Message: "paymaster deposit reserved by pending operations"}
			}
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO operations(hash,sender,nonce,body,stage,queued,paymaster,prefund,valid_until) VALUES($1,$2,$3,$4,'QUEUED',$5,$6,$7,$8)", hash, op.Sender.Hex(), op.Nonce.String(), string(body), now(), pm, ticket.Prefund.ToInt().String(), ticket.ValidUntil)
		return e
	})
}

func duplicateStatus(stage string) error {
	switch stage {
	case "QUEUED", "BUNDLED", "INCLUDED", "FINAL":
		return nil
	default:
		return &RPCError{Code: -32500, Message: "operation is " + stage}
	}
}
