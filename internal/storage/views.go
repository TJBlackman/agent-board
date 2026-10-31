package storage

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
)

// Errors specific to counted opens.
var (
	ErrOpenEventConflict = errors.New("open_event_conflict")
	ErrClockUnreliable   = errors.New("clock_unreliable")
	ErrGenerationChanged = errors.New("board_generation_changed")
	ErrCounterOverflow   = errors.New("counter_overflow")
)

// OpenRequest is the statistics transaction for one tracked full open.
type OpenRequest struct {
	PrincipalID      string
	OpenEventID      string
	ContextID        string
	SessionID        string
	WorkspaceID      string
	EntrySeq         int64
	ExpectedInstance string
	ExpectedGen      string
	Fingerprint      []byte
}

// CountOpen records the open event atomically with counters and daily facts.
// It returns true when a new view was counted and false on a matching retry.
func (db *DB) CountOpen(ctx context.Context, r OpenRequest) (bool, error) {
	counted := false
	err := db.Writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		st, err := checkInstance(ctx, tx, r.ExpectedInstance)
		if err != nil {
			return err
		}
		if st.GenerationID != r.ExpectedGen {
			return ErrGenerationChanged
		}
		now := Now()
		nowUS := now.UnixMicro()
		nowS := domain.FormatTime(now)
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM entries WHERE sequence=?`, r.EntrySeq).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
		var storedFP []byte
		var expires string
		err = tx.QueryRowContext(ctx, `SELECT fingerprint, expires_at FROM open_events WHERE principal_id=? AND open_event_id=?`, r.PrincipalID, r.OpenEventID).Scan(&storedFP, &expires)
		switch {
		case err == nil && expires > nowS:
			if string(storedFP) != string(r.Fingerprint) {
				return ErrOpenEventConflict
			}
			return nil // matching retry: no changes
		case err == nil:
			// Expired receipt: replace atomically, previous contribution stands.
			if nowUS < st.ViewTimeHWMUS {
				return ErrClockUnreliable
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM open_events WHERE principal_id=? AND open_event_id=?`, r.PrincipalID, r.OpenEventID); err != nil {
				return err
			}
		case errors.Is(err, sql.ErrNoRows):
		default:
			return err
		}
		if nowUS < st.ViewTimeHWMUS {
			return ErrClockUnreliable
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO open_events(principal_id, open_event_id, entry_sequence, context_id, fingerprint, first_counted_at, expires_at) VALUES (?,?,?,?,?,?,?)`,
			r.PrincipalID, r.OpenEventID, r.EntrySeq, r.ContextID, r.Fingerprint, nowS, domain.FormatTime(now.Add(domain.OpenEventRetention))); err != nil {
			return err
		}
		var vc, qc int64
		if err := tx.QueryRowContext(ctx, `SELECT view_count, qualified_view_count FROM entry_view_stats WHERE entry_sequence=?`, r.EntrySeq).Scan(&vc, &qc); err != nil {
			return err
		}
		if vc >= math.MaxInt64-1 || qc >= math.MaxInt64-1 {
			return ErrCounterOverflow
		}
		date := domain.UTCDate(now)
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO view_daily_sessions(principal_id, session_id, entry_sequence, utc_date) VALUES (?,?,?,?)`, r.PrincipalID, r.SessionID, r.EntrySeq, date)
		if err != nil {
			return err
		}
		qualified, _ := res.RowsAffected()
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO view_daily_workspaces(workspace_id, entry_sequence, utc_date) VALUES (?,?,?)`, r.WorkspaceID, r.EntrySeq, date); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE entry_view_stats SET view_count=view_count+1, qualified_view_count=qualified_view_count+?, last_view_at=? WHERE entry_sequence=?`, qualified, nowS, r.EntrySeq); err != nil {
			return err
		}
		if err := touchRetention(ctx, tx, r.EntrySeq, nowS); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE board_state SET view_time_hwm_us=? WHERE id=1`, nowUS); err != nil {
			return err
		}
		counted = true
		return nil
	})
	return counted, err
}

// Stats is the /stats payload.
type Stats struct {
	ItemID                string  `json:"item_id"`
	ItemType              string  `json:"item_type"`
	ViewCount             int64   `json:"view_count"`
	QualifiedViewCount    int64   `json:"qualified_view_count"`
	LastViewAt            *string `json:"last_view_at"`
	SessionDayViews30d    int64   `json:"session_day_views_30d"`
	DistinctWorkspaces30d int64   `json:"distinct_workspaces_30d"`
	WindowStart           string  `json:"window_start"`
	WindowEndExclusive    string  `json:"window_end_exclusive"`
	AsOf                  string  `json:"as_of"`
}

// LoadStats reads counters and the 30 date window in one read transaction.
func (db *DB) LoadStats(ctx context.Context, publicID, wantKind string) (*Stats, error) {
	tx, err := db.beginRead(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var seq int64
	var kind string
	err = tx.QueryRowContext(ctx, `SELECT sequence, kind FROM entries WHERE public_id=?`, publicID).Scan(&seq, &kind)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && kind != wantKind) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	now := Now()
	s := &Stats{ItemID: publicID, ItemType: kind, AsOf: domain.FormatTime(now)}
	var last sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT view_count, qualified_view_count, last_view_at FROM entry_view_stats WHERE entry_sequence=?`, seq).Scan(&s.ViewCount, &s.QualifiedViewCount, &last); err != nil {
		return nil, err
	}
	if last.Valid {
		s.LastViewAt = &last.String
	}
	today := now.UTC().Truncate(24 * time.Hour)
	start := today.AddDate(0, 0, -(domain.StatsWindowDates - 1))
	s.WindowStart = domain.UTCDate(start)
	s.WindowEndExclusive = domain.UTCDate(today.AddDate(0, 0, 1))
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM view_daily_sessions WHERE entry_sequence=? AND utc_date >= ? AND utc_date < ?`, seq, s.WindowStart, s.WindowEndExclusive).Scan(&s.SessionDayViews30d); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(DISTINCT workspace_id) FROM view_daily_workspaces WHERE entry_sequence=? AND utc_date >= ? AND utc_date < ?`, seq, s.WindowStart, s.WindowEndExclusive).Scan(&s.DistinctWorkspaces30d); err != nil {
		return nil, err
	}
	return s, nil
}

// CleanupResult reports what a bounded cleanup pass removed.
type CleanupResult struct {
	Idempotency int64 `json:"idempotency_records"`
	OpenEvents  int64 `json:"open_events"`
	DailyFacts  int64 `json:"daily_facts"`
	Posts       int64 `json:"posts_deleted"`
	Comments    int64 `json:"comments_deleted"`
	Deferred    bool  `json:"deferred"`
}

// Cleanup removes inactive discussions, expired receipts, and old daily facts.
// It defers when the writer queue is under foreground pressure.
func (db *DB) Cleanup(ctx context.Context) (*CleanupResult, error) {
	return db.cleanup(ctx, Now)
}

func (db *DB) cleanup(ctx context.Context, clock func() time.Time) (*CleanupResult, error) {
	res := &CleanupResult{}
	if db.Writer.Depth.Load() > domain.PendingWritesMax/2 {
		res.Deferred = true
		return res, nil
	}
	err := db.Writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		st, err := LoadState(ctx, tx)
		if err != nil {
			return err
		}
		now := clock()
		if now.UnixMicro() < st.ViewTimeHWMUS {
			return ErrClockUnreliable
		}
		nowS := domain.FormatTime(now)
		r, err := tx.ExecContext(ctx, `DELETE FROM idempotency_records WHERE rowid IN (SELECT rowid FROM idempotency_records WHERE expires_at <= ? LIMIT ?)`, nowS, domain.CleanupBatch)
		if err != nil {
			return err
		}
		res.Idempotency, _ = r.RowsAffected()
		r, err = tx.ExecContext(ctx, `DELETE FROM open_events WHERE rowid IN (SELECT rowid FROM open_events WHERE expires_at <= ? LIMIT ?)`, nowS, domain.CleanupBatch)
		if err != nil {
			return err
		}
		res.OpenEvents, _ = r.RowsAffected()
		cutoff := domain.UTCDate(now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -(domain.DailyFactDates - 1)))
		r, err = tx.ExecContext(ctx, `DELETE FROM view_daily_sessions WHERE rowid IN (SELECT rowid FROM view_daily_sessions WHERE utc_date < ? LIMIT ?)`, cutoff, domain.CleanupBatch)
		if err != nil {
			return err
		}
		n1, _ := r.RowsAffected()
		r, err = tx.ExecContext(ctx, `DELETE FROM view_daily_workspaces WHERE rowid IN (SELECT rowid FROM view_daily_workspaces WHERE utc_date < ? LIMIT ?)`, cutoff, domain.CleanupBatch)
		if err != nil {
			return err
		}
		n2, _ := r.RowsAffected()
		res.DailyFacts = n1 + n2
		if err := deleteInactivePosts(ctx, tx, domain.FormatTime(now.Add(-domain.PostInactivityRetention)), res); err != nil {
			return err
		}
		if res.Posts > 0 {
			if err := bumpRevision(ctx, tx); err != nil {
				return err
			}
		}
		if res.Idempotency+res.OpenEvents+res.DailyFacts+res.Posts > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE board_state SET view_time_hwm_us=? WHERE id=1`, now.UnixMicro()); err != nil {
				return err
			}
		}
		return nil
	})
	return res, err
}
