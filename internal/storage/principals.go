package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
)

// Principal is an authenticated API credential.
type Principal struct {
	ID         string
	Label      string
	Permission string // read or write
	CreatedAt  string
	RevokedAt  *string
}

// CanWrite reports whether the principal may create content.
func (p *Principal) CanWrite() bool { return p.Permission == "write" }

// TokenPrefix marks board tokens so they are recognizable in config files.
const TokenPrefix = "ab_"

// HashToken returns the stored hash for a bearer token.
func HashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

// CreateToken provisions a principal and returns the one time plaintext token.
func (db *DB) CreateToken(ctx context.Context, label, permission string) (*Principal, string, error) {
	if permission != "read" && permission != "write" {
		return nil, "", errors.New("permission must be read or write")
	}
	if err := domain.ValidateBounded("label", label, 1, 64); err != nil {
		return nil, "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	tok := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	id, err := domain.NewUUID()
	if err != nil {
		return nil, "", err
	}
	p := &Principal{ID: id, Label: label, Permission: permission, CreatedAt: domain.FormatTime(time.Now())}
	err = db.Writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO api_principals(id, label, token_hash, permission, created_at) VALUES (?,?,?,?,?)`,
			p.ID, p.Label, HashToken(tok), p.Permission, p.CreatedAt)
		if IsUniqueViolation(err) {
			return fmt.Errorf("a principal labeled %q already exists", label)
		}
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return p, tok, nil
}

// ListPrincipals returns every principal without secrets.
func (db *DB) ListPrincipals(ctx context.Context) ([]Principal, error) {
	rows, err := db.reads.QueryContext(ctx, `SELECT id, label, permission, created_at, revoked_at FROM api_principals ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Principal
	for rows.Next() {
		var p Principal
		var rev sql.NullString
		if err := rows.Scan(&p.ID, &p.Label, &p.Permission, &p.CreatedAt, &rev); err != nil {
			return nil, err
		}
		if rev.Valid {
			p.RevokedAt = &rev.String
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RevokePrincipal marks a principal revoked by ID or label.
func (db *DB) RevokePrincipal(ctx context.Context, idOrLabel string) error {
	return db.Writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE api_principals SET revoked_at=? WHERE revoked_at IS NULL AND (id=? OR label=?)`, domain.FormatTime(time.Now()), idOrLabel, idOrLabel)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return errors.New("no active principal matched")
		}
		return nil
	})
}

// RevokeAllPrincipals is used after a restore so no pre backup token survives.
func (db *DB) RevokeAllPrincipals(ctx context.Context) (int64, error) {
	var n int64
	err := db.Writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE api_principals SET revoked_at=? WHERE revoked_at IS NULL`, domain.FormatTime(time.Now()))
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

// Authenticate resolves a bearer token to its active principal.
func (db *DB) Authenticate(ctx context.Context, token string) (*Principal, error) {
	if !strings.HasPrefix(token, TokenPrefix) || len(token) > domain.BearerMaxLen {
		return nil, sql.ErrNoRows
	}
	var p Principal
	err := db.reads.QueryRowContext(ctx, `SELECT id, label, permission, created_at FROM api_principals WHERE token_hash=? AND revoked_at IS NULL`, HashToken(token)).
		Scan(&p.ID, &p.Label, &p.Permission, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}
