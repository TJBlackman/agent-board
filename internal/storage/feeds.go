package storage

import (
	"context"
	"database/sql"
	"math"
	"strconv"
	"strings"

	"trevorblackman.dev/agent-board/internal/domain"
	"trevorblackman.dev/agent-board/internal/pagination"
	"trevorblackman.dev/agent-board/internal/search"
)

// Card is the compact discovery representation of a post.
type Card struct {
	PostID           string   `json:"post_id"`
	Title            string   `json:"title"`
	Preview          string   `json:"preview"`
	PreviewTruncated bool     `json:"preview_truncated"`
	Category         *string  `json:"category"`
	Tags             []string `json:"tags"`
	Scope            *string  `json:"scope"`
	Kind             string   `json:"kind"`
	CreatedAt        string   `json:"created_at"`
	CommentCount     int64    `json:"comment_count"`
	LastActivityAt   *string  `json:"last_activity_at,omitempty"`
}

// Filters are the shared taxonomy filters for feeds and search.
type Filters struct {
	Category    *string
	Tags        []string
	TagMatchAny bool
	Scope       *string
}

func (f Filters) fingerprintParts() []string {
	parts := []string{}
	if f.Category != nil {
		parts = append(parts, "c="+*f.Category)
	}
	if len(f.Tags) > 0 {
		parts = append(parts, "t="+strings.Join(f.Tags, ","))
		if f.TagMatchAny {
			parts = append(parts, "any")
		}
	}
	if f.Scope != nil {
		parts = append(parts, "s="+*f.Scope)
	}
	return parts
}

// sql returns WHERE fragments (prefixed with AND) and args. alias is the posts
// table alias and ealias the entries alias.
func (f Filters) sql(alias, ealias string) (string, []any) {
	var b strings.Builder
	var args []any
	if f.Category != nil {
		b.WriteString(" AND " + alias + ".category_id = (SELECT id FROM categories WHERE slug=?)")
		args = append(args, *f.Category)
	}
	if f.Scope != nil {
		b.WriteString(" AND " + alias + ".scope = ?")
		args = append(args, *f.Scope)
	}
	if len(f.Tags) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(f.Tags)), ",")
		for _, t := range f.Tags {
			args = append(args, t)
		}
		if f.TagMatchAny {
			b.WriteString(" AND EXISTS (SELECT 1 FROM post_tags pt JOIN tags t ON t.id=pt.tag_id WHERE pt.post_sequence=" + ealias + ".sequence AND t.slug IN (" + ph + "))")
		} else {
			b.WriteString(" AND (SELECT count(*) FROM post_tags pt JOIN tags t ON t.id=pt.tag_id WHERE pt.post_sequence=" + ealias + ".sequence AND t.slug IN (" + ph + ")) = " + strconv.Itoa(len(f.Tags)))
		}
	}
	return b.String(), args
}

// FeedQuery describes a feed page request.
type FeedQuery struct {
	PrincipalID  string
	Filters      Filters
	Activity     bool
	Limit        int
	PreviewChars int
	Cursor       string
}

// Page is a compact list response.
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func (db *DB) beginRead(ctx context.Context) (*sql.Tx, error) {
	return db.reads.BeginTx(ctx, nil)
}

// Feed returns one page of the created or activity feed.
func (db *DB) Feed(ctx context.Context, q FeedQuery) (*Page[Card], error) {
	tx, err := db.beginRead(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	st, err := LoadState(ctx, tx)
	if err != nil {
		return nil, err
	}
	kind := pagination.KindFeed
	if q.Activity {
		kind = pagination.KindActivity
	}
	fp := pagination.Fingerprint(append([]string{string(kind), strconv.Itoa(q.Limit), strconv.Itoa(q.PreviewChars)}, q.Filters.fingerprintParts()...)...)
	now := Now()
	var wm, pos, pos2 int64
	if q.Cursor != "" {
		p, err := pagination.Decode(st.CursorSecret, q.Cursor, kind, st.GenerationID, q.PrincipalID, fp, now)
		if err != nil {
			return nil, err
		}
		wm, pos, pos2 = p.Watermark, p.Pos, p.Pos2
	} else {
		wm, err = MaxSequence(ctx, tx)
		if err != nil {
			return nil, err
		}
		pos, pos2 = math.MaxInt64, math.MaxInt64
	}
	where, fargs := q.Filters.sql("p", "e")
	var rows *sql.Rows
	if !q.Activity {
		args := []any{wm, wm, wm, pos}
		args = append(args, fargs...)
		args = append(args, q.Limit+1)
		rows, err = tx.QueryContext(ctx, `
SELECT e.sequence, e.public_id, p.title, p.description, c.slug, p.scope, p.post_kind, e.created_at,
  CASE WHEN p.last_activity_sequence <= ?1 THEN p.comment_count
       ELSE (SELECT count(*) FROM entries ce WHERE ce.parent_post_sequence=e.sequence AND ce.sequence <= ?2) END AS cc,
  NULL
FROM posts p JOIN entries e ON e.sequence=p.entry_sequence
LEFT JOIN categories c ON c.id=p.category_id
WHERE e.sequence <= ?3 AND e.sequence < ?4`+where+`
ORDER BY e.sequence DESC LIMIT ?`, args...)
	} else {
		args := []any{wm, wm, wm, wm}
		args = append(args, fargs...)
		args = append(args, pos, pos, pos2, q.Limit+1)
		rows, err = tx.QueryContext(ctx, `
WITH act AS (
  SELECT e.sequence AS seq, e.public_id, p.title, p.description, p.category_id, p.scope, p.post_kind, e.created_at,
    CASE WHEN p.last_activity_sequence <= ?1 THEN p.comment_count
         ELSE (SELECT count(*) FROM entries ce WHERE ce.parent_post_sequence=e.sequence AND ce.sequence <= ?2) END AS cc,
    CASE WHEN p.last_activity_sequence <= ?3 THEN p.last_activity_sequence
         ELSE COALESCE((SELECT max(x.sequence) FROM entries x WHERE x.parent_post_sequence=e.sequence AND x.sequence <= ?4), e.sequence) END AS act_seq
  FROM posts p JOIN entries e ON e.sequence=p.entry_sequence
  WHERE e.sequence <= ?1`+where+`
)
SELECT a.seq, a.public_id, a.title, a.description, c.slug, a.scope, a.post_kind, a.created_at, a.cc,
  (SELECT created_at FROM entries WHERE sequence=a.act_seq)
FROM act a LEFT JOIN categories c ON c.id=a.category_id
WHERE (a.act_seq < ? OR (a.act_seq = ? AND a.seq < ?))
ORDER BY a.act_seq DESC, a.seq DESC LIMIT ?`, args...)
	}
	if err != nil {
		return nil, err
	}
	type row struct {
		seq  int64
		card Card
	}
	var out []row
	for rows.Next() {
		var r row
		var cat, scope, last sql.NullString
		var desc string
		if err := rows.Scan(&r.seq, &r.card.PostID, &r.card.Title, &desc, &cat, &scope, &r.card.Kind, &r.card.CreatedAt, &r.card.CommentCount, &last); err != nil {
			rows.Close()
			return nil, err
		}
		if cat.Valid {
			r.card.Category = &cat.String
		}
		if scope.Valid {
			r.card.Scope = &scope.String
		}
		if last.Valid {
			r.card.LastActivityAt = &last.String
		}
		r.card.Preview, r.card.PreviewTruncated = search.Truncate(search.Normalize(desc), q.PreviewChars)
		r.card.Tags = []string{}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	seqs := make([]int64, len(out))
	for i := range out {
		seqs[i] = out[i].seq
	}
	tagMap, err := tagsForPosts(ctx, tx, seqs)
	if err != nil {
		return nil, err
	}
	page := &Page[Card]{Items: make([]Card, 0, len(out))}
	for i := range out {
		if t, ok := tagMap[out[i].seq]; ok {
			out[i].card.Tags = t
		}
		page.Items = append(page.Items, out[i].card)
	}
	if more {
		last := out[len(out)-1]
		p := pagination.Payload{Kind: kind, Gen: st.GenerationID, Principal: q.PrincipalID, FP: fp, Exp: now.Add(domain.FeedCursorLifetime).Unix(), Watermark: wm, Pos: last.seq}
		if q.Activity {
			// Recompute the activity sequence of the last row for the tuple.
			var act int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT max(x.sequence) FROM entries x WHERE x.parent_post_sequence=? AND x.sequence <= ?), ?)`, last.seq, wm, last.seq).Scan(&act); err != nil {
				return nil, err
			}
			p.Pos, p.Pos2 = act, last.seq
		}
		enc, err := pagination.Encode(st.CursorSecret, p)
		if err != nil {
			return nil, err
		}
		page.NextCursor = &enc
	}
	return page, nil
}

func tagsForPosts(ctx context.Context, q Querier, seqs []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(seqs) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(seqs)), ",")
	args := make([]any, len(seqs))
	for i, s := range seqs {
		args[i] = s
	}
	rows, err := q.QueryContext(ctx, `SELECT pt.post_sequence, t.slug FROM post_tags pt JOIN tags t ON t.id=pt.tag_id WHERE pt.post_sequence IN (`+ph+`) ORDER BY pt.post_sequence, t.slug`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s int64
		var slug string
		if err := rows.Scan(&s, &slug); err != nil {
			return nil, err
		}
		out[s] = append(out[s], slug)
	}
	return out, rows.Err()
}
