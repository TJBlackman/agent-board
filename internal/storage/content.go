package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"trevorblackman.dev/agent-board/internal/domain"
)

// ErrTaxonomyCapacity is returned when a new label would exceed the limits.
var ErrTaxonomyCapacity = errors.New("taxonomy_capacity")

// ErrCrossThreadReply means reply_to_comment_id is not in the same discussion.
var ErrCrossThreadReply = errors.New("reply_not_in_thread")

// NewPost is the validated create request.
type NewPost struct {
	AuthorAgentID      string
	ExecutionContextID string
	Title              string
	Description        string
	Body               string
	Category           *string
	Tags               []string
	Scope              *string
	Kind               string
	RelatedPostID      *string
}

// NewComment is the validated create request.
type NewComment struct {
	PostID             string
	AuthorAgentID      string
	ExecutionContextID string
	Body               string
	ReplyToCommentID   *string
}

func lookupPostSequence(ctx context.Context, tx Querier, publicID string) (int64, error) {
	var seq int64
	err := tx.QueryRowContext(ctx, `SELECT e.sequence FROM entries e JOIN posts p ON p.entry_sequence=e.sequence WHERE e.public_id=?`, publicID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return seq, err
}

func ensureCategory(ctx context.Context, tx *sql.Tx, slug, now string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM categories WHERE slug=?`, slug).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM categories`).Scan(&n); err != nil {
		return 0, err
	}
	if n >= domain.MaxCategories {
		return 0, ErrTaxonomyCapacity
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO categories(slug, created_at) VALUES (?,?)`, slug, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func ensureTag(ctx context.Context, tx *sql.Tx, slug, now string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM tags WHERE slug=?`, slug).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tags`).Scan(&n); err != nil {
		return 0, err
	}
	if n >= domain.MaxTags {
		return 0, ErrTaxonomyCapacity
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO tags(slug, created_at) VALUES (?,?)`, slug, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func insertEntry(ctx context.Context, tx *sql.Tx, kind string, parent *int64, author, contextID, body, now string) (int64, string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		id, err := domain.NewUUID()
		if err != nil {
			return 0, "", err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO entries(public_id, kind, parent_post_sequence, author_agent_id, execution_context_id, body, created_at) VALUES (?,?,?,?,?,?,?)`,
			id, kind, parent, author, contextID, body, now)
		if err == nil {
			seq, err := res.LastInsertId()
			return seq, id, err
		}
		if !IsUniqueViolation(err) {
			return 0, "", err
		}
	}
	return 0, "", errors.New("uuid collision retry exhausted")
}

// InsertPost performs the whole post mutation inside tx: taxonomy, entry,
// post row, tags, stats row, search document, and revision bump.
func InsertPost(ctx context.Context, tx *sql.Tx, principalID string, p *NewPost, now string) (string, error) {
	if err := VerifyAuthorContext(ctx, tx, principalID, p.AuthorAgentID, p.ExecutionContextID); err != nil {
		return "", err
	}
	var related *int64
	if p.RelatedPostID != nil {
		seq, err := lookupPostSequence(ctx, tx, *p.RelatedPostID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return "", fmt.Errorf("%w: related_post_id", ErrNotFound)
			}
			return "", err
		}
		related = &seq
	}
	var catID *int64
	if p.Category != nil {
		id, err := ensureCategory(ctx, tx, *p.Category, now)
		if err != nil {
			return "", err
		}
		catID = &id
	}
	tagIDs := make([]int64, 0, len(p.Tags))
	for _, t := range p.Tags {
		id, err := ensureTag(ctx, tx, t, now)
		if err != nil {
			return "", err
		}
		tagIDs = append(tagIDs, id)
	}
	seq, pub, err := insertEntry(ctx, tx, "post", nil, p.AuthorAgentID, p.ExecutionContextID, p.Body, now)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO posts(entry_sequence, title, description, category_id, scope, post_kind, related_post_sequence, last_activity_sequence, comment_count, retention_activity_at) VALUES (?,?,?,?,?,?,?,?,0,?)`,
		seq, p.Title, p.Description, catID, p.Scope, p.Kind, related, seq, now); err != nil {
		return "", err
	}
	for _, id := range tagIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO post_tags(post_sequence, tag_id) VALUES (?,?)`, seq, id); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO entry_view_stats(entry_sequence) VALUES (?)`, seq); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO search_docs(rowid, title, description, body, parent_post_sequence) VALUES (?,?,?,?,?)`, seq, p.Title, p.Description, p.Body, seq); err != nil {
		return "", err
	}
	return pub, bumpRevision(ctx, tx)
}

// InsertComment performs the comment mutation inside tx and returns the
// comment public ID.
func InsertComment(ctx context.Context, tx *sql.Tx, principalID string, c *NewComment, now string) (string, error) {
	postSeq, err := lookupPostSequence(ctx, tx, c.PostID)
	if err != nil {
		return "", err
	}
	if err := VerifyAuthorContext(ctx, tx, principalID, c.AuthorAgentID, c.ExecutionContextID); err != nil {
		return "", err
	}
	var replyTo *int64
	if c.ReplyToCommentID != nil {
		var seq, parent int64
		err := tx.QueryRowContext(ctx, `SELECT e.sequence, e.parent_post_sequence FROM entries e JOIN comments cm ON cm.entry_sequence=e.sequence WHERE e.public_id=?`, *c.ReplyToCommentID).Scan(&seq, &parent)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: reply_to_comment_id", ErrNotFound)
		}
		if err != nil {
			return "", err
		}
		if parent != postSeq {
			return "", ErrCrossThreadReply
		}
		replyTo = &seq
	}
	seq, pub, err := insertEntry(ctx, tx, "comment", &postSeq, c.AuthorAgentID, c.ExecutionContextID, c.Body, now)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO comments(entry_sequence, reply_to_comment_sequence) VALUES (?,?)`, seq, replyTo); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE posts SET last_activity_sequence=?, comment_count=comment_count+1, retention_activity_at=max(retention_activity_at, ?) WHERE entry_sequence=?`, seq, now, postSeq); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO entry_view_stats(entry_sequence) VALUES (?)`, seq); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO search_docs(rowid, title, description, body, parent_post_sequence) VALUES (?,'','',?,?)`, seq, c.Body, postSeq); err != nil {
		return "", err
	}
	return pub, bumpRevision(ctx, tx)
}
