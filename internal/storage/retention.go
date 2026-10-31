package storage

import (
	"context"
	"database/sql"
	"errors"

	"trevorblackman.dev/agent-board/internal/domain"
)

// touchRetention maps an entry (including any nested reply) to its discussion.
// It does not reorder the content feed or count an agent view.
func touchRetention(ctx context.Context, tx *sql.Tx, seq int64, now string) error {
	r, err := tx.ExecContext(ctx, `UPDATE posts SET retention_activity_at=max(retention_activity_at, ?)
WHERE entry_sequence=(SELECT coalesce(parent_post_sequence, sequence) FROM entries WHERE sequence=?)`, now, seq)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

// TouchWebOpen records a personal UI interaction after successful rendering.
// Recheck authorization, epoch, and existence inside the serialized transaction.
func (db *DB) TouchWebOpen(ctx context.Context, principal, instance, generation, publicID, kind string) error {
	return db.Writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		st, err := checkInstance(ctx, tx, instance)
		if err != nil {
			return err
		}
		if st.GenerationID != generation {
			return ErrGenerationChanged
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM api_principals WHERE id=? AND permission='read' AND revoked_at IS NULL`, principal).Scan(&active); err != nil {
			return err
		}
		if active != 1 {
			return ErrForbidden
		}
		var seq int64
		err = tx.QueryRowContext(ctx, `SELECT sequence FROM entries WHERE public_id=? AND kind=?`, publicID, kind).Scan(&seq)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		now := Now()
		if now.UnixMicro() < st.ViewTimeHWMUS {
			return ErrClockUnreliable
		}
		if err := touchRetention(ctx, tx, seq, domain.FormatTime(now)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE board_state SET view_time_hwm_us=? WHERE id=1`, now.UnixMicro())
		return err
	})
}

// deleteInactivePosts runs with the same writer lock used by comments and opens.
// Each thread is removed completely, with foreign-key enforcement left enabled.
func deleteInactivePosts(ctx context.Context, tx *sql.Tx, cutoff string, res *CleanupResult) error {
	rows, err := tx.QueryContext(ctx, `SELECT entry_sequence FROM posts WHERE retention_activity_at < ? ORDER BY retention_activity_at, entry_sequence LIMIT ?`, cutoff, domain.PostCleanupBatch)
	if err != nil {
		return err
	}
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			rows.Close()
			return err
		}
		seqs = append(seqs, seq)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, seq := range seqs {
		// Resolve entry IDs before removing canonical rows.
		statements := []string{
			`DELETE FROM idempotency_records WHERE resource_id IN (SELECT public_id FROM entries WHERE sequence=? OR parent_post_sequence=?)`,
			`DELETE FROM open_events WHERE entry_sequence IN (SELECT sequence FROM entries WHERE sequence=? OR parent_post_sequence=?)`,
			`DELETE FROM view_daily_sessions WHERE entry_sequence IN (SELECT sequence FROM entries WHERE sequence=? OR parent_post_sequence=?)`,
			`DELETE FROM view_daily_workspaces WHERE entry_sequence IN (SELECT sequence FROM entries WHERE sequence=? OR parent_post_sequence=?)`,
			`DELETE FROM entry_view_stats WHERE entry_sequence IN (SELECT sequence FROM entries WHERE sequence=? OR parent_post_sequence=?)`,
			`UPDATE comments SET reply_to_comment_sequence=NULL WHERE entry_sequence IN (SELECT sequence FROM entries WHERE sequence=? OR parent_post_sequence=?)`,
		}
		for _, query := range statements {
			if _, err := tx.ExecContext(ctx, query, seq, seq); err != nil {
				return err
			}
		}
		for _, query := range []string{
			// Imports can contain FK-valid replies from a different thread.
			`UPDATE comments SET reply_to_comment_sequence=NULL WHERE reply_to_comment_sequence IN (SELECT sequence FROM entries WHERE parent_post_sequence=?)`,
			`DELETE FROM search_docs WHERE parent_post_sequence=?`,
			`DELETE FROM post_tags WHERE post_sequence=?`,
			`UPDATE posts SET related_post_sequence=NULL WHERE related_post_sequence=?`,
			`DELETE FROM comments WHERE entry_sequence IN (SELECT sequence FROM entries WHERE parent_post_sequence=?)`,
		} {
			if _, err := tx.ExecContext(ctx, query, seq); err != nil {
				return err
			}
		}
		r, err := tx.ExecContext(ctx, `DELETE FROM entries WHERE parent_post_sequence=?`, seq)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		res.Comments += n
		if _, err := tx.ExecContext(ctx, `DELETE FROM posts WHERE entry_sequence=?`, seq); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM entries WHERE sequence=?`, seq); err != nil {
			return err
		}
		res.Posts++
	}
	return nil
}
