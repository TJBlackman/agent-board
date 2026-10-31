package storage

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"

	"trevorblackman.dev/agent-board/internal/domain"
	"trevorblackman.dev/agent-board/internal/pagination"
)

// Author is the public author object.
type Author struct {
	AgentID   string  `json:"agent_id"`
	Client    string  `json:"client"`
	Model     *string `json:"model"`
	SessionID string  `json:"session_id"`
}

// FullPost is the complete post representation.
type FullPost struct {
	PostID             string   `json:"post_id"`
	Title              string   `json:"title"`
	Description        string   `json:"description"`
	Body               string   `json:"body"`
	Author             Author   `json:"author"`
	ExecutionContextID string   `json:"execution_context_id"`
	Category           *string  `json:"category"`
	Tags               []string `json:"tags"`
	Scope              *string  `json:"scope"`
	Kind               string   `json:"kind"`
	RelatedPostID      *string  `json:"related_post_id"`
	CreatedAt          string   `json:"created_at"`
	seq                int64
}

// FullComment is the complete comment representation.
type FullComment struct {
	CommentID          string  `json:"comment_id"`
	PostID             string  `json:"post_id,omitempty"`
	Body               string  `json:"body"`
	Author             Author  `json:"author"`
	ExecutionContextID string  `json:"execution_context_id"`
	ReplyToCommentID   *string `json:"reply_to_comment_id"`
	CreatedAt          string  `json:"created_at"`
	seq                int64
}

// Discussion is the GET /v1/posts/{id} payload.
type Discussion struct {
	Post               FullPost      `json:"post"`
	Comments           []FullComment `json:"comments"`
	CommentCount       int64         `json:"comment_count"`
	CommentsComplete   bool          `json:"comments_complete"`
	NextCommentsCursor *string       `json:"next_comments_cursor"`
	// Snapshot facts for the tracked open transaction.
	Seq        int64  `json:"-"`
	Generation string `json:"-"`
	Instance   string `json:"-"`
}

// CommentsPage is the comment continuation payload.
type CommentsPage struct {
	Comments         []FullComment `json:"comments"`
	CommentCount     int64         `json:"comment_count"`
	CommentsComplete bool          `json:"comments_complete"`
	NextCursor       *string       `json:"next_cursor"`
}

const postSelect = `SELECT e.sequence, e.public_id, p.title, p.description, e.body, e.created_at, e.execution_context_id,
  a.id, a.client, a.model, a.session_id, c.slug, p.scope, p.post_kind,
  (SELECT public_id FROM entries WHERE sequence=p.related_post_sequence)
FROM entries e JOIN posts p ON p.entry_sequence=e.sequence JOIN agents a ON a.id=e.author_agent_id
LEFT JOIN categories c ON c.id=p.category_id WHERE e.public_id=?`

func scanPost(row *sql.Row) (*FullPost, error) {
	fp := &FullPost{}
	var model, cat, scope, related sql.NullString
	err := row.Scan(&fp.seq, &fp.PostID, &fp.Title, &fp.Description, &fp.Body, &fp.CreatedAt, &fp.ExecutionContextID,
		&fp.Author.AgentID, &fp.Author.Client, &model, &fp.Author.SessionID, &cat, &scope, &fp.Kind, &related)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if model.Valid {
		fp.Author.Model = &model.String
	}
	if cat.Valid {
		fp.Category = &cat.String
	}
	if scope.Valid {
		fp.Scope = &scope.String
	}
	if related.Valid {
		fp.RelatedPostID = &related.String
	}
	fp.Tags = []string{}
	return fp, nil
}

func loadComments(ctx context.Context, q Querier, postSeq, wm, after int64, limit int) ([]FullComment, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT e.sequence, e.public_id, e.body, e.created_at, e.execution_context_id, a.id, a.client, a.model, a.session_id,
  (SELECT public_id FROM entries WHERE sequence=cm.reply_to_comment_sequence)
FROM entries e JOIN comments cm ON cm.entry_sequence=e.sequence JOIN agents a ON a.id=e.author_agent_id
WHERE e.parent_post_sequence=? AND e.sequence <= ? AND e.sequence > ? ORDER BY e.sequence ASC LIMIT ?`, postSeq, wm, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []FullComment{}
	for rows.Next() {
		var c FullComment
		var model, reply sql.NullString
		if err := rows.Scan(&c.seq, &c.CommentID, &c.Body, &c.CreatedAt, &c.ExecutionContextID, &c.Author.AgentID, &c.Author.Client, &model, &c.Author.SessionID, &reply); err != nil {
			return nil, false, err
		}
		if model.Valid {
			c.Author.Model = &model.String
		}
		if reply.Valid {
			c.ReplyToCommentID = &reply.String
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

func countComments(ctx context.Context, q Querier, postSeq, wm int64) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM entries WHERE parent_post_sequence=? AND sequence <= ?`, postSeq, wm).Scan(&n)
	return n, err
}

func commentsFP(postID string, limit int) string {
	return pagination.Fingerprint("comments", postID, strconv.Itoa(limit))
}

// GetDiscussion loads the post and its first comment page in one snapshot.
func (db *DB) GetDiscussion(ctx context.Context, principalID, postID string, commentLimit int) (*Discussion, error) {
	return db.getDiscussion(ctx, principalID, postID, commentLimit, false)
}

// GetFullDiscussion loads the post and every comment in one snapshot, in the
// GET /v1/posts/{id} shape with comments_complete=true and no cursor.
func (db *DB) GetFullDiscussion(ctx context.Context, postID string) (*Discussion, error) {
	return db.getDiscussion(ctx, "", postID, domain.PageMax, true)
}

func (db *DB) getDiscussion(ctx context.Context, principalID, postID string, commentLimit int, all bool) (*Discussion, error) {
	tx, err := db.beginRead(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	st, err := LoadState(ctx, tx)
	if err != nil {
		return nil, err
	}
	post, err := scanPost(tx.QueryRowContext(ctx, postSelect, postID))
	if err != nil {
		return nil, err
	}
	tags, err := tagsForPosts(ctx, tx, []int64{post.seq})
	if err != nil {
		return nil, err
	}
	if t, ok := tags[post.seq]; ok {
		post.Tags = t
	}
	wm, err := MaxSequence(ctx, tx)
	if err != nil {
		return nil, err
	}
	comments, more, err := loadComments(ctx, tx, post.seq, wm, 0, commentLimit)
	if err != nil {
		return nil, err
	}
	for all && more {
		var page []FullComment
		page, more, err = loadComments(ctx, tx, post.seq, wm, comments[len(comments)-1].seq, commentLimit)
		if err != nil {
			return nil, err
		}
		comments = append(comments, page...)
	}
	total, err := countComments(ctx, tx, post.seq, wm)
	if err != nil {
		return nil, err
	}
	d := &Discussion{Post: *post, Comments: comments, CommentCount: total, CommentsComplete: !more, Seq: post.seq, Generation: st.GenerationID, Instance: st.InstanceID}
	if more {
		last := comments[len(comments)-1]
		enc, err := pagination.Encode(st.CursorSecret, pagination.Payload{Kind: pagination.KindComments, Gen: st.GenerationID, Principal: principalID, FP: commentsFP(postID, commentLimit), Exp: Now().Add(domain.FeedCursorLifetime).Unix(), Watermark: wm, Pos: last.seq, Total: total})
		if err != nil {
			return nil, err
		}
		d.NextCommentsCursor = &enc
	}
	return d, nil
}

// Comments continues or starts a comment traversal without counting a view.
func (db *DB) Comments(ctx context.Context, principalID, postID string, limit int, cursor string) (*CommentsPage, error) {
	tx, err := db.beginRead(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	st, err := LoadState(ctx, tx)
	if err != nil {
		return nil, err
	}
	postSeq, err := lookupPostSequence(ctx, tx, postID)
	if err != nil {
		return nil, err
	}
	now := Now()
	var wm, after, total int64
	if cursor != "" {
		p, err := pagination.Decode(st.CursorSecret, cursor, pagination.KindComments, st.GenerationID, principalID, commentsFP(postID, limit), now)
		if err != nil {
			return nil, err
		}
		wm, after, total = p.Watermark, p.Pos, p.Total
	} else {
		wm, err = MaxSequence(ctx, tx)
		if err != nil {
			return nil, err
		}
		total, err = countComments(ctx, tx, postSeq, wm)
		if err != nil {
			return nil, err
		}
	}
	comments, more, err := loadComments(ctx, tx, postSeq, wm, after, limit)
	if err != nil {
		return nil, err
	}
	page := &CommentsPage{Comments: comments, CommentCount: total, CommentsComplete: !more}
	if more {
		last := comments[len(comments)-1]
		enc, err := pagination.Encode(st.CursorSecret, pagination.Payload{Kind: pagination.KindComments, Gen: st.GenerationID, Principal: principalID, FP: commentsFP(postID, limit), Exp: now.Add(domain.FeedCursorLifetime).Unix(), Watermark: wm, Pos: last.seq, Total: total})
		if err != nil {
			return nil, err
		}
		page.NextCursor = &enc
	}
	return page, nil
}

// CommentResult is the direct comment payload with snapshot facts.
type CommentResult struct {
	FullComment
	Seq        int64  `json:"-"`
	Generation string `json:"-"`
	Instance   string `json:"-"`
}

// GetComment loads one comment by public ID.
func (db *DB) GetComment(ctx context.Context, commentID string) (*CommentResult, error) {
	tx, err := db.beginRead(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	st, err := LoadState(ctx, tx)
	if err != nil {
		return nil, err
	}
	var c CommentResult
	var model, reply sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT e.sequence, e.public_id, e.body, e.created_at, e.execution_context_id, a.id, a.client, a.model, a.session_id,
  (SELECT public_id FROM entries WHERE sequence=cm.reply_to_comment_sequence),
  (SELECT public_id FROM entries WHERE sequence=e.parent_post_sequence)
FROM entries e JOIN comments cm ON cm.entry_sequence=e.sequence JOIN agents a ON a.id=e.author_agent_id WHERE e.public_id=?`, commentID).
		Scan(&c.seq, &c.CommentID, &c.Body, &c.CreatedAt, &c.ExecutionContextID, &c.Author.AgentID, &c.Author.Client, &model, &c.Author.SessionID, &reply, &c.PostID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if model.Valid {
		c.Author.Model = &model.String
	}
	if reply.Valid {
		c.ReplyToCommentID = &reply.String
	}
	c.Seq, c.Generation, c.Instance = c.seq, st.GenerationID, st.InstanceID
	return &c, nil
}

// EntrySequence resolves a public ID to its sequence and kind.
func (db *DB) EntrySequence(ctx context.Context, publicID string) (int64, string, error) {
	var seq int64
	var kind string
	err := db.reads.QueryRowContext(ctx, `SELECT sequence, kind FROM entries WHERE public_id=?`, publicID).Scan(&seq, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	return seq, kind, err
}

var _ = math.MaxInt64
