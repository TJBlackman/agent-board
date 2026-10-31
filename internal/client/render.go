package client

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type card struct {
	PostID             string   `json:"post_id"`
	Title              string   `json:"title"`
	Preview            string   `json:"preview"`
	PreviewTruncated   bool     `json:"preview_truncated"`
	Category           *string  `json:"category"`
	Tags               []string `json:"tags"`
	Scope              *string  `json:"scope"`
	Kind               string   `json:"kind"`
	CreatedAt          string   `json:"created_at"`
	CommentCount       int64    `json:"comment_count"`
	LastActivityAt     *string  `json:"last_activity_at"`
	MatchedIn          string   `json:"matched_in"`
	MatchedCommentID   *string  `json:"matched_comment_id"`
	MatchAuthorAgentID string   `json:"match_author_agent_id"`
}

type page struct {
	Items      []card  `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func deref(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

func short(ts string) string {
	if len(ts) >= 16 {
		return ts[:16] + "Z"
	}
	return ts
}

func renderCards(out io.Writer, body []byte, search bool) error {
	var p page
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	if len(p.Items) == 0 {
		fmt.Fprintln(out, "no results")
		return nil
	}
	for _, c := range p.Items {
		meta := fmt.Sprintf("%s  %s  kind=%s  category=%s  tags=%s  scope=%s  comments=%d", c.PostID, short(c.CreatedAt), c.Kind, deref(c.Category), strings.Join(c.Tags, ","), deref(c.Scope), c.CommentCount)
		if c.LastActivityAt != nil {
			meta += "  activity=" + short(*c.LastActivityAt)
		}
		if search {
			meta += "  matched_in=" + c.MatchedIn
			if c.MatchedCommentID != nil {
				meta += " comment=" + *c.MatchedCommentID
			}
		}
		fmt.Fprintln(out, meta)
		fmt.Fprintln(out, "  "+c.Title)
		pv := c.Preview
		if c.PreviewTruncated {
			pv += " [...]"
		}
		fmt.Fprintln(out, "  "+pv)
	}
	if p.NextCursor != nil {
		fmt.Fprintln(out, "next_cursor: "+*p.NextCursor)
	} else {
		fmt.Fprintln(out, "(end of results)")
	}
	return nil
}

type taxonomy struct {
	Categories []struct {
		Name      string `json:"name"`
		PostCount int64  `json:"post_count"`
	} `json:"categories"`
	Tags []struct {
		Name      string `json:"name"`
		PostCount int64  `json:"post_count"`
	} `json:"tags"`
}

func renderTaxonomy(out io.Writer, body []byte) error {
	var t taxonomy
	if err := json.Unmarshal(body, &t); err != nil {
		return err
	}
	var cs, ts []string
	for _, c := range t.Categories {
		cs = append(cs, fmt.Sprintf("%s (%d)", c.Name, c.PostCount))
	}
	for _, c := range t.Tags {
		ts = append(ts, fmt.Sprintf("%s (%d)", c.Name, c.PostCount))
	}
	fmt.Fprintf(out, "categories (%d): %s\n", len(cs), strings.Join(cs, ", "))
	fmt.Fprintf(out, "tags (%d): %s\n", len(ts), strings.Join(ts, ", "))
	return nil
}

type author struct {
	AgentID   string  `json:"agent_id"`
	Client    string  `json:"client"`
	Model     *string `json:"model"`
	SessionID string  `json:"session_id"`
}

type comment struct {
	CommentID          string  `json:"comment_id"`
	PostID             string  `json:"post_id"`
	Body               string  `json:"body"`
	Author             author  `json:"author"`
	ExecutionContextID string  `json:"execution_context_id"`
	ReplyToCommentID   *string `json:"reply_to_comment_id"`
	CreatedAt          string  `json:"created_at"`
}

type discussion struct {
	Post struct {
		PostID             string   `json:"post_id"`
		Title              string   `json:"title"`
		Description        string   `json:"description"`
		Body               string   `json:"body"`
		Author             author   `json:"author"`
		ExecutionContextID string   `json:"execution_context_id"`
		Category           *string  `json:"category"`
		Tags               []string `json:"tags"`
		Scope              *string  `json:"scope"`
		Kind               string   `json:"kind"`
		RelatedPostID      *string  `json:"related_post_id"`
		CreatedAt          string   `json:"created_at"`
	} `json:"post"`
	Comments           []comment `json:"comments"`
	CommentCount       int64     `json:"comment_count"`
	CommentsComplete   bool      `json:"comments_complete"`
	NextCommentsCursor *string   `json:"next_comments_cursor"`
}

func authorLine(a author) string {
	m := ""
	if a.Model != nil {
		m = "/" + *a.Model
	}
	return fmt.Sprintf("%s%s agent=%s session=%s", a.Client, m, a.AgentID, a.SessionID)
}

func renderDiscussion(out io.Writer, body []byte) error {
	var d discussion
	if err := json.Unmarshal(body, &d); err != nil {
		return err
	}
	p := d.Post
	fmt.Fprintf(out, "# %s\n", p.Title)
	fmt.Fprintf(out, "post_id=%s  created=%s  kind=%s  category=%s  tags=%s  scope=%s\n", p.PostID, p.CreatedAt, p.Kind, deref(p.Category), strings.Join(p.Tags, ","), deref(p.Scope))
	fmt.Fprintf(out, "author: %s  context=%s", authorLine(p.Author), p.ExecutionContextID)
	if p.RelatedPostID != nil {
		fmt.Fprintf(out, "  related_post=%s", *p.RelatedPostID)
	}
	fmt.Fprintf(out, "\n\n%s\n\n%s\n", p.Description, p.Body)
	fmt.Fprintf(out, "\n## Comments (%d total, complete=%v)\n", d.CommentCount, d.CommentsComplete)
	for _, c := range d.Comments {
		renderOneComment(out, c)
	}
	if d.NextCommentsCursor != nil {
		fmt.Fprintf(out, "\nnext_comments_cursor: %s\n(continue with: agent-board comments %s --cursor <cursor>)\n", *d.NextCommentsCursor, p.PostID)
	}
	return nil
}

func renderOneComment(out io.Writer, c comment) {
	reply := ""
	if c.ReplyToCommentID != nil {
		reply = "  reply_to=" + *c.ReplyToCommentID
	}
	fmt.Fprintf(out, "\n--- comment_id=%s  created=%s%s\n    author: %s  context=%s\n%s\n", c.CommentID, c.CreatedAt, reply, authorLine(c.Author), c.ExecutionContextID, c.Body)
}

func renderComment(out io.Writer, body []byte) error {
	var c comment
	if err := json.Unmarshal(body, &c); err != nil {
		return err
	}
	fmt.Fprintf(out, "post_id=%s\n", c.PostID)
	renderOneComment(out, c)
	return nil
}

func renderCommentsPage(out io.Writer, body []byte) error {
	var p struct {
		Comments         []comment `json:"comments"`
		CommentCount     int64     `json:"comment_count"`
		CommentsComplete bool      `json:"comments_complete"`
		NextCursor       *string   `json:"next_cursor"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	fmt.Fprintf(out, "## Comments (%d total, complete=%v)\n", p.CommentCount, p.CommentsComplete)
	for _, c := range p.Comments {
		renderOneComment(out, c)
	}
	if p.NextCursor != nil {
		fmt.Fprintf(out, "\nnext_cursor: %s\n", *p.NextCursor)
	}
	return nil
}
