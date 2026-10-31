package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
)

type retentionFixture struct {
	db                          *DB
	principal, agent, contextID string
}

func newRetentionFixture(t *testing.T) *retentionFixture {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "board.db"), Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	p, _, err := db.CreateToken(context.Background(), "writer", "write")
	if err != nil {
		t.Fatal(err)
	}
	f := &retentionFixture{db: db, principal: p.ID}
	f.write(t, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		f.agent, err = InsertAgent(ctx, tx, p.ID, "test", nil, "session", domain.FormatTime(Now()))
		if err != nil {
			return err
		}
		f.contextID, err = InsertContext(ctx, tx, p.ID, &ExecutionContext{AgentID: f.agent, SessionID: "session", WorkspaceID: "test", HostID: "test", LaunchDirectory: "/test", WorkspaceRoot: "/test", PathStyle: "posix"})
		return err
	})
	return f
}

func (f *retentionFixture) write(t *testing.T, job Job) {
	t.Helper()
	if err := f.db.Writer.Submit(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func (f *retentionFixture) post(t *testing.T, at time.Time, related *string) string {
	t.Helper()
	var id string
	f.write(t, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		cat := "test"
		id, err = InsertPost(ctx, tx, f.principal, &NewPost{AuthorAgentID: f.agent, ExecutionContextID: f.contextID, Title: "retention", Description: "retention", Body: "retentionneedle", Category: &cat, Tags: []string{"test"}, Kind: "finding", RelatedPostID: related}, domain.FormatTime(at))
		return err
	})
	return id
}

func (f *retentionFixture) comment(t *testing.T, post string, at time.Time, reply *string) string {
	t.Helper()
	var id string
	f.write(t, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		id, err = InsertComment(ctx, tx, f.principal, &NewComment{PostID: post, AuthorAgentID: f.agent, ExecutionContextID: f.contextID, Body: "retentionneedle", ReplyToCommentID: reply}, domain.FormatTime(at))
		return err
	})
	return id
}

func (f *retentionFixture) activity(t *testing.T, id string) string {
	t.Helper()
	var stamp string
	if err := f.db.Reads().QueryRow(`SELECT retention_activity_at FROM posts JOIN entries ON sequence=entry_sequence WHERE public_id=?`, id).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	return stamp
}

func TestRetentionBoundaryAndNestedReplies(t *testing.T) {
	f := newRetentionFixture(t)
	now := Now().Add(time.Hour).Truncate(time.Microsecond)
	cut := now.Add(-domain.PostInactivityRetention)
	stale := f.post(t, cut.Add(-time.Microsecond), nil)
	boundary := f.post(t, cut, nil)
	recent := f.post(t, cut.Add(time.Microsecond), nil)
	direct := f.post(t, cut.Add(-time.Hour), nil)
	f.comment(t, direct, now.Add(-time.Hour), nil)
	nested := f.post(t, cut.Add(-time.Hour), nil)
	root := f.comment(t, nested, cut.Add(-time.Minute), nil)
	reply := f.comment(t, nested, cut, &root)
	f.comment(t, nested, now.Add(-time.Hour), &reply)
	res, err := f.db.cleanup(context.Background(), func() time.Time { return now })
	if err != nil || res.Posts != 1 || res.Comments != 0 {
		t.Fatalf("cleanup: %+v %v", res, err)
	}
	if _, _, err := f.db.EntrySequence(context.Background(), stale); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale post survived: %v", err)
	}
	for _, id := range []string{boundary, recent, direct, nested} {
		if _, _, err := f.db.EntrySequence(context.Background(), id); err != nil {
			t.Fatalf("active post deleted: %v", err)
		}
	}
}

func TestRetentionOpensAndRetries(t *testing.T) {
	f := newRetentionFixture(t)
	old := Now().Add(-domain.PostInactivityRetention - time.Hour)
	for _, kind := range []string{"post", "comment"} {
		p := f.post(t, old, nil)
		id := p
		if kind == "comment" {
			root := f.comment(t, p, old, nil)
			id = f.comment(t, p, old, &root)
		}
		seq, _, err := f.db.EntrySequence(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		st, err := LoadState(context.Background(), f.db.Reads())
		if err != nil {
			t.Fatal(err)
		}
		event, _ := domain.NewUUID()
		r := OpenRequest{PrincipalID: f.principal, OpenEventID: event, ContextID: f.contextID, SessionID: "session", WorkspaceID: "test", EntrySeq: seq, ExpectedInstance: st.InstanceID, ExpectedGen: st.GenerationID, Fingerprint: []byte(kind)}
		counted, err := f.db.CountOpen(context.Background(), r)
		if err != nil || !counted {
			t.Fatalf("open: %v %v", counted, err)
		}
		stamp := f.activity(t, p)
		counted, err = f.db.CountOpen(context.Background(), r)
		if err != nil || counted || f.activity(t, p) != stamp {
			t.Fatalf("retry touched retention: %v %v", counted, err)
		}
		res, err := f.db.Cleanup(context.Background())
		if err != nil || res.Posts != 0 {
			t.Fatalf("opened thread expired: %+v %v", res, err)
		}
	}
}

func TestRetentionDeletesWholeThreadAndReferences(t *testing.T) {
	f := newRetentionFixture(t)
	now := Now().Add(time.Hour)
	old := now.Add(-domain.PostInactivityRetention - time.Hour)
	p := f.post(t, old, nil)
	c := f.comment(t, p, old, nil)
	for i := 0; i < 1200; i++ {
		c = f.comment(t, p, old, &c)
	}
	survivor := f.post(t, now, &p)
	seq, _, _ := f.db.EntrySequence(context.Background(), c)
	st, _ := LoadState(context.Background(), f.db.Reads())
	f.write(t, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO open_events VALUES (?,?,?,?,?,?,?)`, f.principal, "event", seq, f.contextID, []byte("test"), domain.FormatTime(old), domain.FormatTime(now.Add(time.Hour))); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO view_daily_sessions VALUES (?,?,?,?)`, f.principal, "session", seq, domain.UTCDate(now)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO view_daily_workspaces VALUES (?,?,?)`, "test", seq, domain.UTCDate(now)); err != nil {
			return err
		}
		return storeReceipt(ctx, tx, f.principal, "receipt", "POST", "/test", []byte("test"), st.InstanceID, &Receipt{Status: 201, ResourceID: c, Body: []byte(`{}`)}, now)
	})
	start := time.Now()
	res, err := f.db.cleanup(context.Background(), func() time.Time { return now })
	if err != nil || res.Posts != 1 || res.Comments != 1201 {
		t.Fatalf("cleanup: %+v %v", res, err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatal("cleanup exceeded budget")
	}
	for _, table := range []string{"comments", "open_events", "view_daily_sessions", "view_daily_workspaces", "idempotency_records"} {
		var count int
		if err := f.db.Reads().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s: %d %v", table, count, err)
		}
	}
	for _, table := range []string{"posts", "entries", "entry_view_stats", "post_tags", "search_docs"} {
		var count int
		if err := f.db.Reads().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s: %d %v", table, count, err)
		}
	}
	var links, violations int
	if err := f.db.Reads().QueryRow(`SELECT count(*) FROM posts WHERE related_post_sequence IS NOT NULL`).Scan(&links); err != nil || links != 0 {
		t.Fatalf("links %d %v", links, err)
	}
	if err := f.db.Reads().QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("FK %d %v", violations, err)
	}
	after, _ := LoadState(context.Background(), f.db.Reads())
	if after.ContentRevision != st.ContentRevision+1 {
		t.Fatal("content revision not bumped")
	}
	if _, _, err := f.db.EntrySequence(context.Background(), survivor); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionBatchesAndClockRegression(t *testing.T) {
	f := newRetentionFixture(t)
	now := Now().Add(time.Hour)
	for i := 0; i < domain.PostCleanupBatch+1; i++ {
		f.post(t, now.Add(-domain.PostInactivityRetention-time.Hour), nil)
	}
	res, err := f.db.cleanup(context.Background(), func() time.Time { return now })
	if err != nil || res.Posts != domain.PostCleanupBatch {
		t.Fatalf("first batch %+v %v", res, err)
	}
	if _, err = f.db.cleanup(context.Background(), func() time.Time { return now.Add(-time.Second) }); !errors.Is(err, ErrClockUnreliable) {
		t.Fatalf("regressed clock: %v", err)
	}
	res, err = f.db.cleanup(context.Background(), func() time.Time { return now })
	if err != nil || res.Posts != 1 {
		t.Fatalf("second batch %+v %v", res, err)
	}
}

func TestRetentionIncomingCommentReference(t *testing.T) {
	f := newRetentionFixture(t)
	old := Now().Add(-domain.PostInactivityRetention - time.Hour)
	p := f.post(t, old, nil)
	c := f.comment(t, p, old, nil)
	survivor := f.post(t, Now(), nil)
	survivingComment := f.comment(t, survivor, Now(), nil)
	f.write(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE comments SET reply_to_comment_sequence=(SELECT sequence FROM entries WHERE public_id=?) WHERE entry_sequence=(SELECT sequence FROM entries WHERE public_id=?)`, c, survivingComment)
		return err
	})
	res, err := f.db.Cleanup(context.Background())
	if err != nil || res.Posts != 1 || res.Comments != 1 {
		t.Fatalf("incoming reply blocked cleanup: %+v %v", res, err)
	}
	var reply sql.NullInt64
	if err := f.db.Reads().QueryRow(`SELECT reply_to_comment_sequence FROM comments`).Scan(&reply); err != nil || reply.Valid {
		t.Fatalf("dangling reply: %+v %v", reply, err)
	}
}

func TestRetentionWebOpenRechecksAndRoundTrip(t *testing.T) {
	f := newRetentionFixture(t)
	p := f.post(t, Now().Add(-domain.PostInactivityRetention-time.Hour), nil)
	personal, _, err := f.db.CreateToken(context.Background(), "personal", "read")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := LoadState(context.Background(), f.db.Reads())
	if err := f.db.TouchWebOpen(context.Background(), f.principal, st.InstanceID, st.GenerationID, p, "post"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("writer accepted: %v", err)
	}
	if err := f.db.TouchWebOpen(context.Background(), personal.ID, st.InstanceID, "wrong", p, "post"); !errors.Is(err, ErrGenerationChanged) {
		t.Fatalf("epoch accepted: %v", err)
	}
	if err := f.db.TouchWebOpen(context.Background(), personal.ID, st.InstanceID, st.GenerationID, p, "comment"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong kind accepted: %v", err)
	}
	if err := f.db.TouchWebOpen(context.Background(), personal.ID, st.InstanceID, st.GenerationID, p, "post"); err != nil {
		t.Fatal(err)
	}
	stamp := f.activity(t, p)
	exportDir := filepath.Join(t.TempDir(), "export")
	if _, err := f.db.Export(context.Background(), exportDir); err != nil {
		t.Fatal(err)
	}
	imported := newEmptyRetentionDB(t)
	if _, err := imported.Import(context.Background(), exportDir); err != nil {
		t.Fatal(err)
	}
	var importedStamp string
	if err := imported.Reads().QueryRow(`SELECT retention_activity_at FROM posts`).Scan(&importedStamp); err != nil || stamp != importedStamp {
		t.Fatalf("activity roundtrip: %s %s %v", stamp, importedStamp, err)
	}
	stats, err := f.db.LoadStats(context.Background(), p, "post")
	if err != nil || stats.ViewCount != 0 || stats.LastViewAt != nil {
		t.Fatalf("UI counted view %+v %v", stats, err)
	}
	if err := f.db.RevokePrincipal(context.Background(), personal.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.TouchWebOpen(context.Background(), personal.ID, st.InstanceID, st.GenerationID, p, "post"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked accepted: %v", err)
	}
	// A page was read, then cleanup won the write queue: touch must fail.
	f.write(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE posts SET retention_activity_at=?`, domain.FormatTime(Now().Add(-domain.PostInactivityRetention-time.Hour)))
		return err
	})
	if _, err := f.db.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	personal2, _, _ := f.db.CreateToken(context.Background(), "personal2", "read")
	if err := f.db.TouchWebOpen(context.Background(), personal2.ID, st.InstanceID, st.GenerationID, p, "post"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted accepted: %v", err)
	}
}

func newEmptyRetentionDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "board.db"), Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRetentionEmptyExportPreservesAllocator(t *testing.T) {
	f := newRetentionFixture(t)
	p := f.post(t, Now().Add(-domain.PostInactivityRetention-time.Hour), nil)
	seq, _, _ := f.db.EntrySequence(context.Background(), p)
	if _, err := f.db.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "export")
	m, err := f.db.Export(context.Background(), dir)
	if err != nil || m.HighestSequence != seq {
		t.Fatalf("allocator export: %+v %v", m, err)
	}
	db := newEmptyRetentionDB(t)
	if _, err := db.Import(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	var next int64
	if err := db.Writer.Submit(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `INSERT INTO entries(public_id,kind,author_agent_id,execution_context_id,body,created_at) VALUES (?,'post',?,?,?,?)`, "new", f.agent, f.contextID, "new", domain.FormatTime(Now()))
		if err != nil {
			return err
		}
		next, err = r.LastInsertId()
		return err
	}); err != nil || next <= seq {
		t.Fatalf("allocator import: %d <= %d, %v", next, seq, err)
	}
}

func TestRetentionMigrationBackfillAndLegacyImport(t *testing.T) {
	f := newRetentionFixture(t)
	old := Now().Add(-200 * 24 * time.Hour)
	p := f.post(t, old, nil)
	c := f.comment(t, p, old.Add(time.Hour), nil)
	view := domain.FormatTime(Now().Add(-time.Hour))
	f.write(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE entry_view_stats SET last_view_at=? WHERE entry_sequence=(SELECT sequence FROM entries WHERE public_id=?)`, view, c)
		return err
	})
	dir := filepath.Join(t.TempDir(), "export")
	m, err := f.db.Export(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range m.EntryChunks {
		path := filepath.Join(dir, name)
		var entries []expEntry
		if err := readJSON(path, &entries); err != nil {
			t.Fatal(err)
		}
		for i := range entries {
			entries[i].RetentionActivityAt = nil
		}
		if err := writeJSON(path, entries); err != nil {
			t.Fatal(err)
		}
	}
	db := newEmptyRetentionDB(t)
	if _, err := db.Import(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	var stamp string
	if err := db.Reads().QueryRow(`SELECT retention_activity_at FROM posts`).Scan(&stamp); err != nil || stamp != view {
		t.Fatalf("legacy import: %s %s %v", stamp, view, err)
	}
	// Build a genuine schema-1 file without rewriting migration-1's checksum.
	path := filepath.Join(t.TempDir(), "v1.db")
	v1, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	defer v1.Close()
	for _, stmt := range splitStatements(migrations[0]) {
		if _, err := v1.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := v1.Exec(`INSERT INTO schema_migrations VALUES (1,?,?)`, checksum(migrations[0]), domain.FormatTime(Now())); err != nil {
		t.Fatal(err)
	}
	// Import fixture rows using attached schema-2 DB, excluding its added column.
	if _, err := v1.Exec(`ATTACH DATABASE ? AS fixture`, f.db.Path); err != nil {
		t.Fatal(err)
	}
	copyTx, err := v1.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer copyTx.Rollback()
	if _, err := copyTx.Exec(`PRAGMA defer_foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"board_state", "api_principals", "agents", "execution_contexts", "categories", "tags", "entries", "posts", "comments", "post_tags", "entry_view_stats"} {
		columns := "*"
		if table == "posts" {
			columns = "entry_sequence,title,description,category_id,scope,post_kind,related_post_sequence,last_activity_sequence,comment_count"
		}
		if _, err := copyTx.Exec(`INSERT INTO main.` + table + ` SELECT ` + columns + ` FROM fixture.` + table); err != nil {
			t.Fatal(fmt.Errorf("copy %s: %w", table, err))
		}
	}
	if _, err := copyTx.Exec(`UPDATE board_state SET schema_version=1`); err != nil {
		t.Fatal(err)
	}
	if err := copyTx.Commit(); err != nil {
		t.Fatal(err)
	}
	v1.Close()
	migrated, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	if err := migrated.Reads().QueryRow(`SELECT retention_activity_at FROM posts`).Scan(&stamp); err != nil || stamp != view {
		t.Fatalf("backfill: %s %s %v", stamp, view, err)
	}
	// Malformed retained timestamps must fail rather than become lexicographic dates.
	var items []expEntry
	chunk := filepath.Join(dir, m.EntryChunks[0])
	if err := readJSON(chunk, &items); err != nil {
		t.Fatal(err)
	}
	bad := "not-a-time"
	items[0].RetentionActivityAt = &bad
	b, _ := json.Marshal(items)
	if err := os.WriteFile(chunk, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newEmptyRetentionDB(t).Import(context.Background(), dir); err == nil {
		t.Fatal("invalid timestamp accepted")
	}
}
