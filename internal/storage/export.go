package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
)

// ExportFormatVersion identifies the portable export layout.
const ExportFormatVersion = 1

// ExportManifest describes a logical export.
type ExportManifest struct {
	FormatVersion        int              `json:"format_version"`
	CreatedAt            string           `json:"created_at"`
	SourceInstanceID     string           `json:"source_instance_id"`
	SchemaVersion        int              `json:"schema_version"`
	ContentRevision      int64            `json:"content_revision"`
	HighestSequence      int64            `json:"highest_sequence"`
	ViewTimeHWMUS        int64            `json:"view_time_hwm_us"`
	Counts               map[string]int64 `json:"counts"`
	EntryChunks          []string         `json:"entry_chunks"`
	ExcludesRetryLedgers bool             `json:"excludes_retry_ledgers"`
	Note                 string           `json:"note"`
}

type expPrincipal struct {
	ID         string  `json:"id"`
	Label      string  `json:"label"`
	Permission string  `json:"permission"`
	CreatedAt  string  `json:"created_at"`
	RevokedAt  *string `json:"revoked_at"`
}

type expAgent struct {
	ID          string  `json:"id"`
	PrincipalID string  `json:"principal_id"`
	Client      string  `json:"client"`
	Model       *string `json:"model"`
	SessionID   string  `json:"session_id"`
	OriginWS    *string `json:"origin_workspace_id"`
	OriginHost  *string `json:"origin_host_id"`
	OriginLD    *string `json:"origin_launch_directory"`
	OriginWR    *string `json:"origin_workspace_root"`
	OriginPS    *string `json:"origin_path_style"`
	CreatedAt   string  `json:"created_at"`
}

type expSlug struct {
	ID        int64  `json:"id"`
	Slug      string `json:"slug"`
	CreatedAt string `json:"created_at"`
}

type expEntry struct {
	Sequence            int64    `json:"sequence"`
	PublicID            string   `json:"public_id"`
	Kind                string   `json:"kind"`
	ParentPostSequence  *int64   `json:"parent_post_sequence"`
	AuthorAgentID       string   `json:"author_agent_id"`
	ExecutionContextID  string   `json:"execution_context_id"`
	Body                string   `json:"body"`
	CreatedAt           string   `json:"created_at"`
	Title               *string  `json:"title,omitempty"`
	Description         *string  `json:"description,omitempty"`
	Category            *string  `json:"category,omitempty"`
	Scope               *string  `json:"scope,omitempty"`
	PostKind            *string  `json:"post_kind,omitempty"`
	RelatedPostSeq      *int64   `json:"related_post_sequence,omitempty"`
	Tags                []string `json:"tags,omitempty"`
	ReplyToCommentSeq   *int64   `json:"reply_to_comment_sequence,omitempty"`
	ViewCount           int64    `json:"view_count"`
	QualifiedViewCount  int64    `json:"qualified_view_count"`
	LastViewAt          *string  `json:"last_view_at"`
	RetentionActivityAt *string  `json:"retention_activity_at,omitempty"`
}

type expDailySession struct {
	PrincipalID string `json:"principal_id"`
	SessionID   string `json:"session_id"`
	EntrySeq    int64  `json:"entry_sequence"`
	UTCDate     string `json:"utc_date"`
}

type expDailyWorkspace struct {
	WorkspaceID string `json:"workspace_id"`
	EntrySeq    int64  `json:"entry_sequence"`
	UTCDate     string `json:"utc_date"`
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o640)
}

// Export writes the portable logical export into dir (which must not exist
// or must be empty).
func (db *DB) Export(ctx context.Context, dir string) (*ExportManifest, error) {
	if ents, err := os.ReadDir(dir); err == nil && len(ents) > 0 {
		return nil, fmt.Errorf("export destination %s is not empty", dir)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	tx, err := db.writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	st, err := LoadState(ctx, tx)
	if err != nil {
		return nil, err
	}
	var hs int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce((SELECT seq FROM sqlite_sequence WHERE name='entries'), 0)`).Scan(&hs); err != nil {
		return nil, err
	}
	m := &ExportManifest{FormatVersion: ExportFormatVersion, CreatedAt: domain.FormatTime(time.Now()), SourceInstanceID: st.InstanceID, SchemaVersion: st.SchemaVersion, ContentRevision: st.ContentRevision, HighestSequence: hs, ViewTimeHWMUS: st.ViewTimeHWMUS, Counts: map[string]int64{}, ExcludesRetryLedgers: true,
		Note: "Idempotency and open-event retry ledgers are deliberately excluded. Import creates a new board instance; clients must start new registrations and open operations."}

	// Principals (no hashes).
	var principals []expPrincipal
	rows, err := tx.QueryContext(ctx, `SELECT id, label, permission, created_at, revoked_at FROM api_principals ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p expPrincipal
		var rev sql.NullString
		if err := rows.Scan(&p.ID, &p.Label, &p.Permission, &p.CreatedAt, &rev); err != nil {
			rows.Close()
			return nil, err
		}
		if rev.Valid {
			p.RevokedAt = &rev.String
		}
		principals = append(principals, p)
	}
	rows.Close()
	m.Counts["principals"] = int64(len(principals))
	if err := writeJSON(filepath.Join(dir, "principals.json"), principals); err != nil {
		return nil, err
	}

	var agents []expAgent
	rows, err = tx.QueryContext(ctx, `SELECT id, principal_id, client, model, session_id, origin_workspace_id, origin_host_id, origin_launch_directory, origin_workspace_root, origin_path_style, created_at FROM agents ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a expAgent
		if err := rows.Scan(&a.ID, &a.PrincipalID, &a.Client, &a.Model, &a.SessionID, &a.OriginWS, &a.OriginHost, &a.OriginLD, &a.OriginWR, &a.OriginPS, &a.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		agents = append(agents, a)
	}
	rows.Close()
	m.Counts["agents"] = int64(len(agents))
	if err := writeJSON(filepath.Join(dir, "agents.json"), agents); err != nil {
		return nil, err
	}

	var contexts []ExecutionContext
	rows, err = tx.QueryContext(ctx, `SELECT id, agent_id, session_id, workspace_id, host_id, launch_directory, workspace_root, working_directory, path_style, created_at FROM execution_contexts ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c ExecutionContext
		if err := rows.Scan(&c.ID, &c.AgentID, &c.SessionID, &c.WorkspaceID, &c.HostID, &c.LaunchDirectory, &c.WorkspaceRoot, &c.WorkingDirectory, &c.PathStyle, &c.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		contexts = append(contexts, c)
	}
	rows.Close()
	m.Counts["execution_contexts"] = int64(len(contexts))
	if err := writeJSON(filepath.Join(dir, "execution_contexts.json"), contexts); err != nil {
		return nil, err
	}

	for _, table := range []string{"categories", "tags"} {
		var items []expSlug
		rows, err = tx.QueryContext(ctx, `SELECT id, slug, created_at FROM `+table+` ORDER BY id`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var s expSlug
			if err := rows.Scan(&s.ID, &s.Slug, &s.CreatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			items = append(items, s)
		}
		rows.Close()
		m.Counts[table] = int64(len(items))
		if err := writeJSON(filepath.Join(dir, table+".json"), items); err != nil {
			return nil, err
		}
	}

	// Entries in chunks of 1000 sequences.
	const chunk = 1000
	var total int64
	for start := int64(1); start <= hs; start += chunk {
		rows, err = tx.QueryContext(ctx, `SELECT e.sequence, e.public_id, e.kind, e.parent_post_sequence, e.author_agent_id, e.execution_context_id, e.body, e.created_at,
  p.title, p.description, c.slug, p.scope, p.post_kind, p.related_post_sequence, cm.reply_to_comment_sequence,
  v.view_count, v.qualified_view_count, v.last_view_at, p.retention_activity_at
FROM entries e LEFT JOIN posts p ON p.entry_sequence=e.sequence LEFT JOIN categories c ON c.id=p.category_id
LEFT JOIN comments cm ON cm.entry_sequence=e.sequence LEFT JOIN entry_view_stats v ON v.entry_sequence=e.sequence
WHERE e.sequence >= ? AND e.sequence < ? ORDER BY e.sequence`, start, start+chunk)
		if err != nil {
			return nil, err
		}
		var items []expEntry
		for rows.Next() {
			var e expEntry
			if err := rows.Scan(&e.Sequence, &e.PublicID, &e.Kind, &e.ParentPostSequence, &e.AuthorAgentID, &e.ExecutionContextID, &e.Body, &e.CreatedAt,
				&e.Title, &e.Description, &e.Category, &e.Scope, &e.PostKind, &e.RelatedPostSeq, &e.ReplyToCommentSeq, &e.ViewCount, &e.QualifiedViewCount, &e.LastViewAt, &e.RetentionActivityAt); err != nil {
				rows.Close()
				return nil, err
			}
			items = append(items, e)
		}
		rows.Close()
		if len(items) == 0 {
			continue
		}
		seqs := make([]int64, 0, len(items))
		for _, e := range items {
			if e.Kind == "post" {
				seqs = append(seqs, e.Sequence)
			}
		}
		tagMap, err := tagsForPosts(ctx, tx, seqs)
		if err != nil {
			return nil, err
		}
		for i := range items {
			if t, ok := tagMap[items[i].Sequence]; ok {
				items[i].Tags = t
			}
		}
		name := fmt.Sprintf("entries-%08d.json", start)
		if err := writeJSON(filepath.Join(dir, name), items); err != nil {
			return nil, err
		}
		m.EntryChunks = append(m.EntryChunks, name)
		total += int64(len(items))
	}
	m.Counts["entries"] = total

	var ds []expDailySession
	rows, err = tx.QueryContext(ctx, `SELECT principal_id, session_id, entry_sequence, utc_date FROM view_daily_sessions ORDER BY utc_date, entry_sequence`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d expDailySession
		if err := rows.Scan(&d.PrincipalID, &d.SessionID, &d.EntrySeq, &d.UTCDate); err != nil {
			rows.Close()
			return nil, err
		}
		ds = append(ds, d)
	}
	rows.Close()
	m.Counts["view_daily_sessions"] = int64(len(ds))
	if err := writeJSON(filepath.Join(dir, "view_daily_sessions.json"), ds); err != nil {
		return nil, err
	}
	var dw []expDailyWorkspace
	rows, err = tx.QueryContext(ctx, `SELECT workspace_id, entry_sequence, utc_date FROM view_daily_workspaces ORDER BY utc_date, entry_sequence`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d expDailyWorkspace
		if err := rows.Scan(&d.WorkspaceID, &d.EntrySeq, &d.UTCDate); err != nil {
			rows.Close()
			return nil, err
		}
		dw = append(dw, d)
	}
	rows.Close()
	m.Counts["view_daily_workspaces"] = int64(len(dw))
	if err := writeJSON(filepath.Join(dir, "view_daily_workspaces.json"), dw); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(dir, "manifest.json"), m); err != nil {
		return nil, err
	}
	return m, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Import loads a logical export into this empty board. Principals are
// recreated revoked with random unusable hashes so attribution survives but
// no credential does. The board receives a new instance ID.
func (db *DB) Import(ctx context.Context, dir string) (*ExportManifest, error) {
	var m ExportManifest
	if err := readJSON(filepath.Join(dir, "manifest.json"), &m); err != nil {
		return nil, err
	}
	if m.FormatVersion != ExportFormatVersion {
		return nil, fmt.Errorf("unsupported export format %d", m.FormatVersion)
	}
	tx, err := db.writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM entries`).Scan(&n); err != nil {
		return nil, err
	}
	var np int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM api_principals`).Scan(&np); err != nil {
		return nil, err
	}
	if n > 0 || np > 0 {
		return nil, errors.New("import target must be an empty board")
	}
	now := domain.FormatTime(time.Now())
	var principals []expPrincipal
	if err := readJSON(filepath.Join(dir, "principals.json"), &principals); err != nil {
		return nil, err
	}
	for _, p := range principals {
		junk := make([]byte, 32)
		rand.Read(junk)
		rev := now
		if p.RevokedAt != nil {
			rev = *p.RevokedAt
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO api_principals(id, label, token_hash, permission, created_at, revoked_at) VALUES (?,?,?,?,?,?)`, p.ID, p.Label, junk, p.Permission, p.CreatedAt, rev); err != nil {
			return nil, fmt.Errorf("principal %s: %w", p.ID, err)
		}
	}
	var agents []expAgent
	if err := readJSON(filepath.Join(dir, "agents.json"), &agents); err != nil {
		return nil, err
	}
	for _, a := range agents {
		if _, err := tx.ExecContext(ctx, `INSERT INTO agents(id, principal_id, client, model, session_id, origin_workspace_id, origin_host_id, origin_launch_directory, origin_workspace_root, origin_path_style, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			a.ID, a.PrincipalID, a.Client, a.Model, a.SessionID, a.OriginWS, a.OriginHost, a.OriginLD, a.OriginWR, a.OriginPS, a.CreatedAt); err != nil {
			return nil, fmt.Errorf("agent %s: %w", a.ID, err)
		}
	}
	var contexts []ExecutionContext
	if err := readJSON(filepath.Join(dir, "execution_contexts.json"), &contexts); err != nil {
		return nil, err
	}
	for _, c := range contexts {
		if err := domain.ValidatePath("launch_directory", c.LaunchDirectory, c.PathStyle); err != nil {
			return nil, fmt.Errorf("context %s: %w", c.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO execution_contexts(id, agent_id, session_id, workspace_id, host_id, launch_directory, workspace_root, working_directory, path_style, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			c.ID, c.AgentID, c.SessionID, c.WorkspaceID, c.HostID, c.LaunchDirectory, c.WorkspaceRoot, c.WorkingDirectory, c.PathStyle, c.CreatedAt); err != nil {
			return nil, fmt.Errorf("context %s: %w", c.ID, err)
		}
	}
	catIDs := map[string]int64{}
	tagIDs := map[string]int64{}
	for _, table := range []string{"categories", "tags"} {
		var items []expSlug
		if err := readJSON(filepath.Join(dir, table+".json"), &items); err != nil {
			return nil, err
		}
		for _, s := range items {
			if err := domain.ValidateSlug(table, s.Slug); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+`(id, slug, created_at) VALUES (?,?,?)`, s.ID, s.Slug, s.CreatedAt); err != nil {
				return nil, err
			}
			if table == "categories" {
				catIDs[s.Slug] = s.ID
			} else {
				tagIDs[s.Slug] = s.ID
			}
		}
	}
	var count int64
	for _, name := range m.EntryChunks {
		var items []expEntry
		if err := readJSON(filepath.Join(dir, name), &items); err != nil {
			return nil, err
		}
		for _, e := range items {
			if !domain.IsUUID(e.PublicID) {
				return nil, fmt.Errorf("entry %d has an invalid public id", e.Sequence)
			}
			activity := e.CreatedAt
			for _, stamp := range []*string{&e.CreatedAt, e.LastViewAt, e.RetentionActivityAt} {
				if stamp == nil {
					continue
				}
				parsed, err := domain.ParseTime(*stamp)
				if err != nil || domain.FormatTime(parsed) != *stamp {
					return nil, fmt.Errorf("entry %d has an invalid canonical timestamp", e.Sequence)
				}
				if *stamp > activity {
					activity = *stamp
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO entries(sequence, public_id, kind, parent_post_sequence, author_agent_id, execution_context_id, body, created_at) VALUES (?,?,?,?,?,?,?,?)`,
				e.Sequence, e.PublicID, e.Kind, e.ParentPostSequence, e.AuthorAgentID, e.ExecutionContextID, e.Body, e.CreatedAt); err != nil {
				return nil, fmt.Errorf("entry %d: %w", e.Sequence, err)
			}
			if e.Kind == "post" {
				if e.Title == nil || e.Description == nil || e.PostKind == nil {
					return nil, fmt.Errorf("post %d missing fields", e.Sequence)
				}
				if err := domain.ValidateSingleLine("title", *e.Title, domain.TitleMaxCodePoints); err != nil {
					return nil, fmt.Errorf("post %d: %w", e.Sequence, err)
				}
				var cat *int64
				if e.Category != nil {
					id, ok := catIDs[*e.Category]
					if !ok {
						return nil, fmt.Errorf("post %d references unknown category", e.Sequence)
					}
					cat = &id
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO posts(entry_sequence, title, description, category_id, scope, post_kind, related_post_sequence, last_activity_sequence, comment_count, retention_activity_at) VALUES (?,?,?,?,?,?,?,?,0,?)`,
					e.Sequence, *e.Title, *e.Description, cat, e.Scope, *e.PostKind, e.RelatedPostSeq, e.Sequence, activity); err != nil {
					return nil, fmt.Errorf("post %d: %w", e.Sequence, err)
				}
				for _, t := range e.Tags {
					id, ok := tagIDs[t]
					if !ok {
						return nil, fmt.Errorf("post %d references unknown tag %s", e.Sequence, t)
					}
					if _, err := tx.ExecContext(ctx, `INSERT INTO post_tags(post_sequence, tag_id) VALUES (?,?)`, e.Sequence, id); err != nil {
						return nil, err
					}
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO search_docs(rowid, title, description, body, parent_post_sequence) VALUES (?,?,?,?,?)`, e.Sequence, *e.Title, *e.Description, e.Body, e.Sequence); err != nil {
					return nil, err
				}
			} else {
				if e.ParentPostSequence == nil {
					return nil, fmt.Errorf("comment %d has no parent", e.Sequence)
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO comments(entry_sequence, reply_to_comment_sequence) VALUES (?,?)`, e.Sequence, e.ReplyToCommentSeq); err != nil {
					return nil, fmt.Errorf("comment %d: %w", e.Sequence, err)
				}
				if _, err := tx.ExecContext(ctx, `UPDATE posts SET last_activity_sequence=?, comment_count=comment_count+1, retention_activity_at=max(retention_activity_at, ?) WHERE entry_sequence=?`, e.Sequence, activity, *e.ParentPostSequence); err != nil {
					return nil, err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO search_docs(rowid, title, description, body, parent_post_sequence) VALUES (?,'','',?,?)`, e.Sequence, e.Body, *e.ParentPostSequence); err != nil {
					return nil, err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO entry_view_stats(entry_sequence, view_count, qualified_view_count, last_view_at) VALUES (?,?,?,?)`, e.Sequence, e.ViewCount, e.QualifiedViewCount, e.LastViewAt); err != nil {
				return nil, err
			}
			count++
		}
	}
	// Preserve the allocator high watermark even when the last records were removed.
	if m.HighestSequence > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sqlite_sequence(name, seq) SELECT 'entries', ? WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name='entries')`, m.HighestSequence); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sqlite_sequence SET seq=max(seq, ?) WHERE name='entries'`, m.HighestSequence); err != nil {
			return nil, err
		}
	}
	var ds []expDailySession
	if err := readJSON(filepath.Join(dir, "view_daily_sessions.json"), &ds); err != nil {
		return nil, err
	}
	for _, d := range ds {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO view_daily_sessions(principal_id, session_id, entry_sequence, utc_date) VALUES (?,?,?,?)`, d.PrincipalID, d.SessionID, d.EntrySeq, d.UTCDate); err != nil {
			return nil, err
		}
	}
	var dw []expDailyWorkspace
	if err := readJSON(filepath.Join(dir, "view_daily_workspaces.json"), &dw); err != nil {
		return nil, err
	}
	for _, d := range dw {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO view_daily_workspaces(workspace_id, entry_sequence, utc_date) VALUES (?,?,?)`, d.WorkspaceID, d.EntrySeq, d.UTCDate); err != nil {
			return nil, err
		}
	}
	inst, err := domain.NewUUID()
	if err != nil {
		return nil, err
	}
	gen, err := domain.NewUUID()
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	rand.Read(secret)
	if _, err := tx.ExecContext(ctx, `UPDATE board_state SET instance_id=?, generation_id=?, cursor_secret=?, content_revision=?, view_time_hwm_us=? WHERE id=1`, inst, gen, secret, m.ContentRevision+1, m.ViewTimeHWMUS); err != nil {
		return nil, err
	}
	var fk int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&fk); err == nil && fk > 0 {
		return nil, fmt.Errorf("foreign key check found %d violations", fk)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	m.Counts["imported_entries"] = count
	return &m, nil
}

// RotateGeneration issues a new generation ID and cursor secret so no cursor
// issued before a restore or maintenance operation can continue.
func (db *DB) RotateGeneration(ctx context.Context) (string, error) {
	gen, err := domain.NewUUID()
	if err != nil {
		return "", err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	err = db.Writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE board_state SET generation_id=?, cursor_secret=? WHERE id=1`, gen, secret)
		return err
	})
	return gen, err
}
