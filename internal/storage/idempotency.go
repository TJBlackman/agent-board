package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
)

// Receipt is the stored replayable response of a mutation.
type Receipt struct {
	Status     int
	Location   string
	Body       []byte
	ResourceID string
}

// ErrIdempotencyConflict means the key was reused with a different request.
var ErrIdempotencyConflict = errors.New("idempotency_conflict")

// ErrInstanceChanged means the client's expected board instance differs.
var ErrInstanceChanged = errors.New("board_instance_changed")

// RequestHash hashes the canonical typed request with method and path.
func RequestHash(method, path string, canonical any) []byte {
	b, _ := json.Marshal(canonical)
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(b)
	return h.Sum(nil)
}

// checkInstance compares the expected instance ID inside the transaction.
func checkInstance(ctx context.Context, tx *sql.Tx, expected string) (*BoardState, error) {
	st, err := LoadState(ctx, tx)
	if err != nil {
		return nil, err
	}
	if st.InstanceID != expected {
		return nil, ErrInstanceChanged
	}
	return st, nil
}

// lookupReceipt returns a stored receipt for the key, or nil. It enforces
// the request hash match and removes expired records.
func lookupReceipt(ctx context.Context, tx *sql.Tx, principalID, key, method, path string, hash []byte, now time.Time) (*Receipt, error) {
	var r Receipt
	var m, p, expires string
	var loc, res sql.NullString
	var storedHash []byte
	err := tx.QueryRowContext(ctx, `SELECT method, path, request_hash, status, location, response, resource_id, expires_at FROM idempotency_records WHERE principal_id=? AND key=?`, principalID, key).
		Scan(&m, &p, &storedHash, &r.Status, &loc, &r.Body, &res, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if expires <= domain.FormatTime(now) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency_records WHERE principal_id=? AND key=?`, principalID, key); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if m != method || p != path || string(storedHash) != string(hash) {
		return nil, ErrIdempotencyConflict
	}
	r.Location = loc.String
	r.ResourceID = res.String
	return &r, nil
}

func storeReceipt(ctx context.Context, tx *sql.Tx, principalID, key, method, path string, hash []byte, expectedInstance string, r *Receipt, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO idempotency_records(principal_id, key, method, path, request_hash, status, location, response, resource_id, expected_instance_id, created_at, expires_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		principalID, key, method, path, hash, r.Status, r.Location, r.Body, r.ResourceID, expectedInstance, domain.FormatTime(now), domain.FormatTime(now.Add(domain.IdempotencyRetention)))
	return err
}

// Mutation describes an idempotent write submitted to the writer queue.
type Mutation struct {
	PrincipalID      string
	Key              string
	Method           string
	Path             string
	Hash             []byte
	ExpectedInstance string
	// Apply performs the write and returns the receipt. It runs only when no
	// matching receipt exists. ContentChange makes the revision advance.
	Apply func(ctx context.Context, tx *sql.Tx, st *BoardState, now time.Time) (*Receipt, error)
}

// RunMutation executes m through the writer queue with full idempotency
// semantics. The second result reports whether the receipt was replayed.
func (db *DB) RunMutation(ctx context.Context, m Mutation) (*Receipt, bool, error) {
	var out *Receipt
	replayed := false
	err := db.Writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := Now()
		st, err := checkInstance(ctx, tx, m.ExpectedInstance)
		if err != nil {
			return err
		}
		if r, err := lookupReceipt(ctx, tx, m.PrincipalID, m.Key, m.Method, m.Path, m.Hash, now); err != nil {
			return err
		} else if r != nil {
			out = r
			replayed = true
			return nil
		}
		r, err := m.Apply(ctx, tx, st, now)
		if err != nil {
			return err
		}
		if err := storeReceipt(ctx, tx, m.PrincipalID, m.Key, m.Method, m.Path, m.Hash, m.ExpectedInstance, r, now); err != nil {
			return err
		}
		out = r
		return nil
	})
	return out, replayed, err
}

// bumpRevision advances the content revision inside a content transaction.
func bumpRevision(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE board_state SET content_revision = content_revision + 1 WHERE id=1`)
	return err
}
