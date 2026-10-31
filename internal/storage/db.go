// Package storage owns the SQLite database: opening with the qualified
// durability settings, migrations, the single writer queue, the read pool,
// and every SQL statement in the service.
package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"trevorblackman.dev/agent-board/internal/domain"
)

// DB wraps the writer connection and the read pool.
type DB struct {
	Path   string
	reads  *sql.DB
	writer *sql.DB
	Writer *Writer
}

// Options controls Open.
type Options struct {
	// ReadPool bounds the read connections. Zero means domain.ActiveReadsMax.
	ReadPool int
	// Exclusive opens only the writer connection and no read pool. Used by
	// offline operator commands.
	Exclusive bool
	// Create initializes an empty board when the file does not exist.
	Create bool
}

func dsn(path string) string {
	return "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(1000)&_time_format=sqlite"
}

// Open opens the board database and applies migrations.
func Open(ctx context.Context, path string, opt Options) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if !opt.Create {
			return nil, fmt.Errorf("database %s does not exist", path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, err
		}
	}
	w, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	if err := w.PingContext(ctx); err != nil {
		w.Close()
		return nil, err
	}
	db := &DB{Path: path, writer: w}
	if err := db.migrate(ctx); err != nil {
		w.Close()
		return nil, err
	}
	if !opt.Exclusive {
		n := opt.ReadPool
		if n <= 0 {
			n = domain.ActiveReadsMax
		}
		r, err := sql.Open("sqlite", dsn(path))
		if err != nil {
			w.Close()
			return nil, err
		}
		r.SetMaxOpenConns(n)
		r.SetMaxIdleConns(n)
		r.SetConnMaxLifetime(0)
		db.reads = r
	}
	db.Writer = newWriter(w)
	return db, nil
}

// Close stops the writer queue and closes both pools.
func (db *DB) Close() error {
	if db.Writer != nil {
		db.Writer.Close()
	}
	var first error
	if db.reads != nil {
		if err := db.reads.Close(); err != nil {
			first = err
		}
	}
	if err := db.writer.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

// Reads exposes the read pool.
func (db *DB) Reads() *sql.DB { return db.reads }

// WriterDB exposes the single writer connection pool (max one connection).
func (db *DB) WriterDB() *sql.DB { return db.writer }

func checksum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (db *DB) migrate(ctx context.Context) error {
	var hasMig int
	err := db.writer.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&hasMig)
	if err != nil {
		return err
	}
	tx, err := db.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	applied := map[int]string{}
	if hasMig == 1 {
		rows, err := tx.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var v int
			var c string
			if err := rows.Scan(&v, &c); err != nil {
				rows.Close()
				return err
			}
			applied[v] = c
		}
		rows.Close()
	}
	for i, m := range migrations {
		v := i + 1
		sum := checksum(m)
		if c, ok := applied[v]; ok {
			if c != sum {
				return fmt.Errorf("migration %d checksum mismatch: database has %s, binary has %s; refusing to start", v, c[:12], sum[:12])
			}
			continue
		}
		if v == 1 && hasMig == 1 {
			return fmt.Errorf("database has a schema_migrations table without version 1")
		}
		for _, stmt := range splitStatements(m) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migration %d: %w", v, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, checksum, applied_at) VALUES (?,?,?)`, v, sum, domain.FormatTime(time.Now())); err != nil {
			return err
		}
	}
	for v := range applied {
		if v > len(migrations) {
			return fmt.Errorf("database schema version %d is newer than this binary supports (%d)", v, len(migrations))
		}
	}
	// Board state bootstrap.
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM board_state`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		inst, err := domain.NewUUID()
		if err != nil {
			return err
		}
		gen, err := domain.NewUUID()
		if err != nil {
			return err
		}
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO board_state(id, instance_id, generation_id, schema_version, search_profile_version, content_revision, view_time_hwm_us, cursor_secret, maintenance_state, created_at) VALUES (1,?,?,?,?,0,0,?,'normal',?)`,
			inst, gen, SchemaVersion, SearchProfileVersion, secret, domain.FormatTime(time.Now())); err != nil {
			return err
		}
	} else {
		var sv, sp int
		if err := tx.QueryRowContext(ctx, `SELECT schema_version, search_profile_version FROM board_state WHERE id=1`).Scan(&sv, &sp); err != nil {
			return err
		}
		if sv != SchemaVersion {
			if _, err := tx.ExecContext(ctx, `UPDATE board_state SET schema_version=? WHERE id=1`, SchemaVersion); err != nil {
				return err
			}
		}
		if sp != SearchProfileVersion {
			return fmt.Errorf("search profile version %d in database differs from %d in binary; run rebuild-search in maintenance", sp, SearchProfileVersion)
		}
	}
	return tx.Commit()
}

func splitStatements(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ";\n") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// BoardState is the singleton row.
type BoardState struct {
	InstanceID           string
	GenerationID         string
	SchemaVersion        int
	SearchProfileVersion int
	ContentRevision      int64
	ViewTimeHWMUS        int64
	CursorSecret         []byte
	MaintenanceState     string
}

// Querier is satisfied by *sql.DB, *sql.Tx and *sql.Conn.
type Querier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// LoadState reads board_state through q.
func LoadState(ctx context.Context, q Querier) (*BoardState, error) {
	s := &BoardState{}
	err := q.QueryRowContext(ctx, `SELECT instance_id, generation_id, schema_version, search_profile_version, content_revision, view_time_hwm_us, cursor_secret, maintenance_state FROM board_state WHERE id=1`).
		Scan(&s.InstanceID, &s.GenerationID, &s.SchemaVersion, &s.SearchProfileVersion, &s.ContentRevision, &s.ViewTimeHWMUS, &s.CursorSecret, &s.MaintenanceState)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// MaxSequence returns the highest committed entry sequence (0 when empty).
func MaxSequence(ctx context.Context, q Querier) (int64, error) {
	var n sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT max(sequence) FROM entries`).Scan(&n); err != nil {
		return 0, err
	}
	return n.Int64, nil
}

// IsUniqueViolation reports whether err is a SQLite UNIQUE constraint failure.
func IsUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// IsForeignKeyViolation reports whether err is a SQLite FOREIGN KEY failure.
func IsForeignKeyViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

// IsBusy reports whether err is SQLITE_BUSY after the busy timeout.
func IsBusy(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "database is locked") || strings.Contains(err.Error(), "(5)"))
}

// Now returns the server timestamp used for records.
func Now() time.Time { return time.Now().UTC() }
