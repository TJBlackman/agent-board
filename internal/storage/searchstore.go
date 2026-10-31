package storage

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"

	"trevorblackman.dev/agent-board/internal/domain"
	"trevorblackman.dev/agent-board/internal/pagination"
	"trevorblackman.dev/agent-board/internal/search"
)

// ErrSearchChanged means the content revision moved since the cursor was issued.
var ErrSearchChanged = errors.New("search_changed")

// SearchCard extends Card with match attribution.
type SearchCard struct {
	Card
	MatchedIn          string  `json:"matched_in"`
	MatchedCommentID   *string `json:"matched_comment_id"`
	MatchAuthorAgentID string  `json:"match_author_agent_id"`
}

// SearchQuery describes a search page request.
type SearchQuery struct {
	PrincipalID  string
	Terms        []search.Term
	Filters      Filters
	SortRecent   bool
	Limit        int
	PreviewChars int
	Cursor       string
}

// Search returns one page of grouped search results.
func (db *DB) Search(ctx context.Context, q SearchQuery) (*Page[SearchCard], error) {
	tx, err := db.beginRead(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	st, err := LoadState(ctx, tx)
	if err != nil {
		return nil, err
	}
	match := search.Compile(q.Terms)
	sort := "relevance"
	if q.SortRecent {
		sort = "recent"
	}
	fp := pagination.Fingerprint(append([]string{"search", match, sort, strconv.Itoa(q.Limit), strconv.Itoa(q.PreviewChars)}, q.Filters.fingerprintParts()...)...)
	now := Now()
	var lastScore float64
	var lastPost int64
	first := true
	if q.Cursor != "" {
		p, err := pagination.Decode(st.CursorSecret, q.Cursor, pagination.KindSearch, st.GenerationID, q.PrincipalID, fp, now)
		if err != nil {
			return nil, err
		}
		if p.Revision != st.ContentRevision {
			return nil, ErrSearchChanged
		}
		lastScore = math.Float64frombits(p.Score)
		lastPost = p.Pos2
		first = false
	}
	where, fargs := q.Filters.sql("p", "e")
	var cont, order string
	var cargs []any
	if q.SortRecent {
		order = "post_seq DESC"
		if !first {
			cont = " AND post_seq < ?"
			cargs = []any{lastPost}
		}
	} else {
		order = "score ASC, post_seq DESC"
		if !first {
			cont = " AND (score > ? OR (score = ? AND post_seq < ?))"
			cargs = []any{lastScore, lastScore, lastPost}
		}
	}
	args := []any{match}
	args = append(args, fargs...)
	args = append(args, cargs...)
	args = append(args, q.Limit+1)
	rows, err := tx.QueryContext(ctx, `
WITH hits AS (
  SELECT rowid AS seq, parent_post_sequence AS post_seq, bm25(search_docs, 3.0, 2.0, 1.0) AS score
  FROM search_docs WHERE search_docs MATCH ?
), eligible AS (
  SELECT h.seq, h.post_seq, h.score FROM hits h
  JOIN posts p ON p.entry_sequence=h.post_seq JOIN entries e ON e.sequence=p.entry_sequence
  WHERE 1=1`+where+`
), ranked AS (
  SELECT seq, post_seq, score, row_number() OVER (PARTITION BY post_seq ORDER BY score ASC, seq ASC) AS rn FROM eligible
)
SELECT seq, post_seq, score FROM ranked WHERE rn=1`+cont+` ORDER BY `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	type hit struct {
		seq, post int64
		score     float64
	}
	var hits []hit
	for rows.Next() {
		var h hit
		if err := rows.Scan(&h.seq, &h.post, &h.score); err != nil {
			rows.Close()
			return nil, err
		}
		hits = append(hits, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	more := len(hits) > q.Limit
	if more {
		hits = hits[:q.Limit]
	}
	page := &Page[SearchCard]{Items: make([]SearchCard, 0, len(hits))}
	if len(hits) == 0 {
		return page, nil
	}
	// Load cards for the posts and the matched documents.
	postSeqs := make([]int64, len(hits))
	docSeqs := make([]int64, len(hits))
	for i, h := range hits {
		postSeqs[i] = h.post
		docSeqs[i] = h.seq
	}
	cards, err := cardsBySeq(ctx, tx, postSeqs)
	if err != nil {
		return nil, err
	}
	tagMap, err := tagsForPosts(ctx, tx, postSeqs)
	if err != nil {
		return nil, err
	}
	docs, err := docsBySeq(ctx, tx, docSeqs)
	if err != nil {
		return nil, err
	}
	words := search.Words(q.Terms)
	for _, h := range hits {
		c, ok := cards[h.post]
		if !ok {
			continue
		}
		if t, ok := tagMap[h.post]; ok {
			c.Tags = t
		}
		d := docs[h.seq]
		sc := SearchCard{Card: c, MatchAuthorAgentID: d.author}
		if h.seq == h.post {
			sc.MatchedIn = "post"
			if search.ContainsAny(c.Preview, words) || !search.ContainsAny(d.body, words) {
				// Description explains the match (or nothing better exists).
			} else {
				sc.Preview, sc.PreviewTruncated = search.Excerpt(d.body, words, q.PreviewChars)
			}
		} else {
			sc.MatchedIn = "comment"
			id := d.publicID
			sc.MatchedCommentID = &id
			sc.Preview, sc.PreviewTruncated = search.Excerpt(d.body, words, q.PreviewChars)
		}
		page.Items = append(page.Items, sc)
	}
	if more {
		last := hits[len(hits)-1]
		enc, err := pagination.Encode(st.CursorSecret, pagination.Payload{Kind: pagination.KindSearch, Gen: st.GenerationID, Principal: q.PrincipalID, FP: fp, Exp: now.Add(domain.SearchCursorLifetime).Unix(), Revision: st.ContentRevision, Pos: last.seq, Pos2: last.post, Score: math.Float64bits(last.score)})
		if err != nil {
			return nil, err
		}
		page.NextCursor = &enc
	}
	return page, nil
}

type doc struct {
	publicID string
	body     string
	author   string
}

func docsBySeq(ctx context.Context, q Querier, seqs []int64) (map[int64]doc, error) {
	out := map[int64]doc{}
	if len(seqs) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(seqs)), ",")
	args := make([]any, len(seqs))
	for i, s := range seqs {
		args[i] = s
	}
	rows, err := q.QueryContext(ctx, `SELECT sequence, public_id, body, author_agent_id FROM entries WHERE sequence IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s int64
		var d doc
		if err := rows.Scan(&s, &d.publicID, &d.body, &d.author); err != nil {
			return nil, err
		}
		out[s] = d
	}
	return out, rows.Err()
}

func cardsBySeq(ctx context.Context, q Querier, seqs []int64) (map[int64]Card, error) {
	out := map[int64]Card{}
	if len(seqs) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(seqs)), ",")
	args := make([]any, len(seqs))
	for i, s := range seqs {
		args[i] = s
	}
	rows, err := q.QueryContext(ctx, `SELECT e.sequence, e.public_id, p.title, p.description, c.slug, p.scope, p.post_kind, e.created_at, p.comment_count
FROM posts p JOIN entries e ON e.sequence=p.entry_sequence LEFT JOIN categories c ON c.id=p.category_id WHERE e.sequence IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s int64
		var c Card
		var cat, scope sql.NullString
		var desc string
		if err := rows.Scan(&s, &c.PostID, &c.Title, &desc, &cat, &scope, &c.Kind, &c.CreatedAt, &c.CommentCount); err != nil {
			return nil, err
		}
		if cat.Valid {
			c.Category = &cat.String
		}
		if scope.Valid {
			c.Scope = &scope.String
		}
		c.Preview = search.Normalize(desc)
		c.Tags = []string{}
		out[s] = c
	}
	return out, rows.Err()
}

// RebuildSearch regenerates the derived index from canonical records. It
// runs as one maintenance transaction on the writer and bumps the revision so
// outstanding search cursors are invalidated.
func (db *DB) RebuildSearch(ctx context.Context) (int64, error) {
	var n int64
	err := db.Writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM search_docs`); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO search_docs(rowid, title, description, body, parent_post_sequence)
SELECT e.sequence, COALESCE(p.title,''), COALESCE(p.description,''), e.body, COALESCE(e.parent_post_sequence, e.sequence)
FROM entries e LEFT JOIN posts p ON p.entry_sequence=e.sequence`)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		if _, err := tx.ExecContext(ctx, `UPDATE board_state SET search_profile_version=? WHERE id=1`, SearchProfileVersion); err != nil {
			return err
		}
		return bumpRevision(ctx, tx)
	})
	return n, err
}
