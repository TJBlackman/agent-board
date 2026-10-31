package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"trevorblackman.dev/agent-board/internal/domain"
	"trevorblackman.dev/agent-board/internal/storage"
)

type harness struct {
	t     *testing.T
	srv   *Server
	ts    *httptest.Server
	db    *storage.DB
	token string
	inst  string
	agent string
	ctxID string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), filepath.Join(dir, "board.db"), storage.Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv, err := New(Config{BackupDir: filepath.Join(dir, "backups"), Version: "test", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DisableRateLimits: true}, db)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)
	_, tok, err := db.CreateToken(context.Background(), "test-writer", "write")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, srv: srv, ts: ts, db: db, token: tok, inst: srv.InstanceID()}
	return h
}

type resp struct {
	Status int
	Header http.Header
	Body   []byte
}

func (h *harness) do(method, path string, headers map[string]string, body any, auth bool) resp {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case []byte:
			rd = bytes.NewReader(b)
		case string:
			rd = strings.NewReader(b)
		default:
			j, _ := json.Marshal(b)
			rd = bytes.NewReader(j)
		}
	}
	req, _ := http.NewRequest(method, h.ts.URL+path, rd)
	if auth {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{Status: r.StatusCode, Header: r.Header, Body: b}
}

func (h *harness) mut(path string, body any, key string) resp {
	return h.do("POST", path, map[string]string{"Idempotency-Key": key, "X-Board-Instance-ID": h.inst}, body, true)
}

func key() string { k, _ := domain.NewUUID(); return k }

func (h *harness) register() {
	h.t.Helper()
	r := h.mut("/v1/agents", map[string]any{"client": "test", "model": nil, "session_id": "sess-1"}, key())
	if r.Status != 201 {
		h.t.Fatalf("agent: %d %s", r.Status, r.Body)
	}
	var a struct {
		AgentID string `json:"agent_id"`
	}
	json.Unmarshal(r.Body, &a)
	h.agent = a.AgentID
	r = h.mut("/v1/execution-contexts", map[string]any{"agent_id": h.agent, "session_id": "sess-1", "workspace_id": "test-ws", "host_id": "test-host", "launch_directory": `C:\Projects\x`, "workspace_root": `C:\Projects\x`, "working_directory": nil, "path_style": "windows"}, key())
	if r.Status != 201 {
		h.t.Fatalf("context: %d %s", r.Status, r.Body)
	}
	var c struct {
		ID string `json:"execution_context_id"`
	}
	json.Unmarshal(r.Body, &c)
	h.ctxID = c.ID
}

func (h *harness) post(title, body string, cat string, tags []string) string {
	h.t.Helper()
	req := map[string]any{"author_agent_id": h.agent, "execution_context_id": h.ctxID, "title": title, "description": "desc " + title, "body": body, "tags": tags}
	if cat != "" {
		req["category"] = cat
	}
	r := h.mut("/v1/posts", req, key())
	if r.Status != 201 {
		h.t.Fatalf("post: %d %s", r.Status, r.Body)
	}
	var p struct {
		PostID string `json:"post_id"`
	}
	json.Unmarshal(r.Body, &p)
	return p.PostID
}

func (h *harness) comment(post, body string, reply *string) string {
	h.t.Helper()
	req := map[string]any{"author_agent_id": h.agent, "execution_context_id": h.ctxID, "body": body, "reply_to_comment_id": reply}
	r := h.mut("/v1/posts/"+post+"/comments", req, key())
	if r.Status != 201 {
		h.t.Fatalf("comment: %d %s", r.Status, r.Body)
	}
	var c struct {
		CommentID string `json:"comment_id"`
	}
	json.Unmarshal(r.Body, &c)
	return c.CommentID
}

func (h *harness) open(path string, event string) resp {
	return h.do("GET", path, map[string]string{"X-Execution-Context-ID": h.ctxID, "X-Open-Event-ID": event, "X-Board-Instance-ID": h.inst}, nil, true)
}

func TestRootAndOpenAPI(t *testing.T) {
	h := newHarness(t)
	r := h.do("GET", "/", nil, nil, false)
	if r.Status != 200 || !strings.Contains(r.Header.Get("Content-Type"), "text/markdown") || !strings.Contains(string(r.Body), h.inst) {
		t.Fatalf("root: %d %s", r.Status, r.Header.Get("Content-Type"))
	}
	if len(r.Body) > 12*1024 {
		t.Fatalf("root guide too long: %d bytes", len(r.Body))
	}
	tag := r.Header.Get("ETag")
	r2 := h.do("GET", "/", map[string]string{"If-None-Match": tag}, nil, false)
	if r2.Status != 304 {
		t.Fatalf("expected 304, got %d", r2.Status)
	}
	o := h.do("GET", "/openapi.json", nil, nil, false)
	var doc map[string]any
	if err := json.Unmarshal(o.Body, &doc); err != nil {
		t.Fatalf("openapi invalid JSON: %v", err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Fatal("openapi version missing")
	}
	if r := h.do("GET", "/nope", nil, nil, false); r.Status != 404 || !strings.Contains(string(r.Body), "not_found") {
		t.Fatalf("unknown route: %d %s", r.Status, r.Body)
	}
	if r := h.do("GET", "/v1/posts", nil, nil, false); r.Status != 401 {
		t.Fatalf("expected 401, got %d", r.Status)
	}
}

func TestLifecycle(t *testing.T) {
	h := newHarness(t)
	h.register()
	p1 := h.post("Diagnosing Docker volume permissions", "Observed permission denied on the data volume after restart.", "infrastructure", []string{"docker", "permissions"})
	p2 := h.post("OAuth2 refresh quirks", "Token refresh fails with 401 when the s3-client clock drifts.", "auth", []string{"oauth2"})
	c1 := h.comment(p1, "Also seen after chown; the zebra-phrase fix was chmod 775.", nil)
	c2 := h.comment(p1, "Confirming the chown fix.", &c1)

	// Cross-thread reply rejected.
	r := h.mut("/v1/posts/"+p2+"/comments", map[string]any{"author_agent_id": h.agent, "execution_context_id": h.ctxID, "body": "x", "reply_to_comment_id": c1}, key())
	if r.Status != 422 || !strings.Contains(string(r.Body), "reply_not_in_thread") {
		t.Fatalf("cross thread reply: %d %s", r.Status, r.Body)
	}

	// Feed newest first, compact.
	r = h.do("GET", "/v1/posts", nil, nil, true)
	var feed struct {
		Items      []map[string]any `json:"items"`
		NextCursor *string          `json:"next_cursor"`
	}
	json.Unmarshal(r.Body, &feed)
	if len(feed.Items) != 2 || feed.Items[0]["post_id"] != p2 || feed.Items[1]["comment_count"].(float64) != 2 {
		t.Fatalf("feed: %s", r.Body)
	}
	if _, has := feed.Items[0]["body"]; has {
		t.Fatal("feed leaked body")
	}
	// Filters.
	r = h.do("GET", "/v1/posts?category=infrastructure&tags=docker,permissions", nil, nil, true)
	json.Unmarshal(r.Body, &feed)
	if len(feed.Items) != 1 || feed.Items[0]["post_id"] != p1 {
		t.Fatalf("filter: %s", r.Body)
	}
	r = h.do("GET", "/v1/posts?tags=docker,oauth2&tag_match=any", nil, nil, true)
	json.Unmarshal(r.Body, &feed)
	if len(feed.Items) != 2 {
		t.Fatalf("any filter: %s", r.Body)
	}
	if r := h.do("GET", "/v1/posts?category=Infra", nil, nil, true); r.Status != 422 {
		t.Fatalf("bad slug accepted: %d", r.Status)
	}
	if r := h.do("GET", "/v1/posts?categry=x", nil, nil, true); r.Status != 400 {
		t.Fatalf("unknown param accepted: %d", r.Status)
	}
	if r := h.do("GET", "/v1/posts?limit=1&limit=2", nil, nil, true); r.Status != 400 {
		t.Fatalf("duplicate param accepted: %d", r.Status)
	}
	// Taxonomy with counts.
	r = h.do("GET", "/v1/taxonomy", nil, nil, true)
	var tax storage.Taxonomy
	json.Unmarshal(r.Body, &tax)
	if len(tax.Categories) != 2 || len(tax.Tags) != 3 || tax.Tags[0].Name != "docker" || tax.Tags[0].PostCount != 1 {
		t.Fatalf("taxonomy: %s", r.Body)
	}
	if r2 := h.do("GET", "/v1/taxonomy", map[string]string{"If-None-Match": r.Header.Get("ETag")}, nil, true); r2.Status != 304 {
		t.Fatalf("taxonomy etag: %d", r2.Status)
	}

	// Search: comment only phrase returns parent and comment id.
	r = h.do("GET", "/v1/search?q=%22zebra-phrase%22", nil, nil, true)
	var sp struct {
		Items []map[string]any `json:"items"`
	}
	json.Unmarshal(r.Body, &sp)
	if len(sp.Items) != 1 || sp.Items[0]["post_id"] != p1 || sp.Items[0]["matched_in"] != "comment" || sp.Items[0]["matched_comment_id"] != c1 {
		t.Fatalf("search comment: %s", r.Body)
	}
	// Grouping: many matches in one thread, one slot.
	r = h.do("GET", "/v1/search?q=chown", nil, nil, true)
	json.Unmarshal(r.Body, &sp)
	if len(sp.Items) != 1 {
		t.Fatalf("search grouping: %s", r.Body)
	}
	// AND across separate comments does not match.
	r = h.do("GET", "/v1/search?q=zebra-phrase+Confirming", nil, nil, true)
	json.Unmarshal(r.Body, &sp)
	if len(sp.Items) != 0 {
		t.Fatalf("search AND across docs matched: %s", r.Body)
	}
	// Operators neutralized.
	r = h.do("GET", "/v1/search?q=NOT+OR+docker*", nil, nil, true)
	if r.Status != 200 {
		t.Fatalf("operators: %d %s", r.Status, r.Body)
	}
	if r := h.do("GET", "/v1/search?q=%22unterminated", nil, nil, true); r.Status != 422 {
		t.Fatalf("unterminated accepted: %d", r.Status)
	}

	// Tracked open: counts once, dedups retry, conflicts on changed target.
	ev := key()
	r = h.open("/v1/posts/"+p1, ev)
	if r.Status != 200 || r.Header.Get("X-View-Counted") != "true" {
		t.Fatalf("open: %d %s %s", r.Status, r.Header.Get("X-View-Counted"), r.Body)
	}
	var d storage.Discussion
	json.Unmarshal(r.Body, &d)
	if d.Post.Body == "" || len(d.Comments) != 2 || !d.CommentsComplete || d.Comments[1].ReplyToCommentID == nil || *d.Comments[1].ReplyToCommentID != c1 {
		t.Fatalf("discussion: %s", r.Body)
	}
	r = h.open("/v1/posts/"+p1, ev)
	if r.Status != 200 || r.Header.Get("X-View-Counted") != "false" {
		t.Fatalf("open retry: %d %s", r.Status, r.Header.Get("X-View-Counted"))
	}
	r = h.open("/v1/posts/"+p2, ev)
	if r.Status != 409 || !strings.Contains(string(r.Body), "open_event_conflict") {
		t.Fatalf("open conflict: %d %s", r.Status, r.Body)
	}
	// Second deliberate open same session/day: view_count 2, qualified 1.
	r = h.open("/v1/posts/"+p1, key())
	if r.Header.Get("X-View-Counted") != "true" {
		t.Fatal("second open not counted")
	}
	r = h.do("GET", "/v1/posts/"+p1+"/stats", nil, nil, true)
	var st storage.Stats
	json.Unmarshal(r.Body, &st)
	if st.ViewCount != 2 || st.QualifiedViewCount != 1 || st.SessionDayViews30d != 1 || st.DistinctWorkspaces30d != 1 || st.LastViewAt == nil {
		t.Fatalf("stats: %s", r.Body)
	}
	// Comment open counts the comment only.
	r = h.open("/v1/comments/"+c2, key())
	if r.Status != 200 {
		t.Fatalf("comment open: %d %s", r.Status, r.Body)
	}
	r = h.do("GET", "/v1/comments/"+c2+"/stats", nil, nil, true)
	json.Unmarshal(r.Body, &st)
	if st.ViewCount != 1 {
		t.Fatalf("comment stats: %s", r.Body)
	}
	r = h.do("GET", "/v1/posts/"+p1+"/stats", nil, nil, true)
	json.Unmarshal(r.Body, &st)
	if st.ViewCount != 2 {
		t.Fatalf("post stats changed by comment open: %s", r.Body)
	}
	// Missing headers, HEAD, conditional, untracked headers.
	if r := h.do("GET", "/v1/posts/"+p1, nil, nil, true); r.Status != 400 {
		t.Fatalf("missing tracked headers: %d", r.Status)
	}
	if r := h.do("HEAD", "/v1/posts/"+p1, nil, nil, true); r.Status != 405 {
		t.Fatalf("HEAD: %d", r.Status)
	}
	if r := h.do("GET", "/v1/posts/"+p1, map[string]string{"X-Execution-Context-ID": h.ctxID, "X-Open-Event-ID": key(), "X-Board-Instance-ID": h.inst, "If-None-Match": `"x"`}, nil, true); r.Status != 400 {
		t.Fatalf("conditional: %d", r.Status)
	}
	if r := h.do("GET", "/v1/posts", map[string]string{"X-Open-Event-ID": key()}, nil, true); r.Status != 400 || !strings.Contains(string(r.Body), "untracked_route_headers") {
		t.Fatalf("untracked headers: %d %s", r.Status, r.Body)
	}
	// Wrong instance.
	r = h.do("GET", "/v1/posts/"+p1, map[string]string{"X-Execution-Context-ID": h.ctxID, "X-Open-Event-ID": key(), "X-Board-Instance-ID": key()}, nil, true)
	if r.Status != 409 || !strings.Contains(string(r.Body), "board_instance_changed") {
		t.Fatalf("instance: %d %s", r.Status, r.Body)
	}
	// Views did not change revision/ordering: search cursor-less works and feed unchanged.
	r = h.do("GET", "/v1/posts?sort=activity", nil, nil, true)
	json.Unmarshal(r.Body, &feed)
	if feed.Items[0]["post_id"] != p1 || feed.Items[0]["last_activity_at"] == nil {
		t.Fatalf("activity feed: %s", r.Body)
	}
}

func TestIdempotencyAndValidation(t *testing.T) {
	h := newHarness(t)
	h.register()
	k := key()
	body := map[string]any{"author_agent_id": h.agent, "execution_context_id": h.ctxID, "title": "T", "description": "D", "body": "B", "tags": []string{"b", "a", "a"}}
	r1 := h.mut("/v1/posts", body, k)
	r2 := h.mut("/v1/posts", body, k)
	if r1.Status != 201 || r2.Status != 201 || string(r1.Body) != string(r2.Body) || r2.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %d %s %s", r1.Status, r2.Status, r1.Body, r2.Body)
	}
	// Tag order differences hash identically (canonical form).
	body["tags"] = []string{"a", "b"}
	if r3 := h.mut("/v1/posts", body, k); r3.Status != 201 || string(r3.Body) != string(r1.Body) {
		t.Fatalf("canonical replay: %d %s", r3.Status, r3.Body)
	}
	body["title"] = "Other"
	if r4 := h.mut("/v1/posts", body, k); r4.Status != 409 {
		t.Fatalf("conflict: %d %s", r4.Status, r4.Body)
	}
	// Only one post exists.
	r := h.do("GET", "/v1/posts", nil, nil, true)
	if !strings.Contains(string(r.Body), `"T"`) || strings.Count(string(r.Body), `"post_id"`) != 1 {
		t.Fatalf("duplicates: %s", r.Body)
	}
	// Validation.
	cases := []struct {
		body string
		code string
		want int
	}{
		{`{"author_agent_id":"` + h.agent + `","execution_context_id":"` + h.ctxID + `","title":"T","description":"D","body":"B","tags":["Bad_Tag"]}`, "invalid_slug", 422},
		{`{"author_agent_id":"` + h.agent + `","execution_context_id":"` + h.ctxID + `","title":"` + strings.Repeat("é", 257) + `","description":"D","body":"B"}`, "too_long", 422},
		{`{"author_agent_id":"` + h.agent + `","execution_context_id":"` + h.ctxID + `","title":"` + strings.Repeat("😀", 256) + `","description":"D","body":"B"}`, "", 201},
		{`{"author_agent_id":"` + h.agent + `","execution_context_id":"` + h.ctxID + `","title":"a\u2028b","description":"D","body":"B"}`, "invalid_character", 422},
		{`{"author_agent_id":"` + h.agent + `","execution_context_id":"` + h.ctxID + `","title":"T","description":"D","body":"B","extra":1}`, "malformed_json", 400},
		{`{"a":1,"a":2}`, "malformed_json", 400},
		{`{"title":"\ud800"}`, "malformed_json", 400},
		{`{"author_agent_id":"` + h.agent + `","execution_context_id":"` + h.ctxID + `","title":"T","description":"D","body":"B","kind":"rumor"}`, "invalid_kind", 422},
		{`{"author_agent_id":"` + h.agent + `","execution_context_id":"` + h.ctxID + `","title":"T","description":"D","body":"B","scope":"/bad/"}`, "invalid_scope", 422},
	}
	for i, c := range cases {
		r := h.mut("/v1/posts", c.body, key())
		if r.Status != c.want || (c.code != "" && !strings.Contains(string(r.Body), c.code)) {
			t.Errorf("case %d: got %d %s, want %d %s", i, r.Status, r.Body, c.want, c.code)
		}
	}
	// Bad idempotency key, missing instance header, bad content type.
	if r := h.do("POST", "/v1/posts", map[string]string{"Idempotency-Key": "short", "X-Board-Instance-ID": h.inst}, body, true); r.Status != 422 {
		t.Fatalf("short key: %d", r.Status)
	}
	if r := h.do("POST", "/v1/posts", map[string]string{"Idempotency-Key": key()}, body, true); r.Status != 400 {
		t.Fatalf("missing instance: %d", r.Status)
	}
	req, _ := http.NewRequest("POST", h.ts.URL+"/v1/posts", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "text/plain")
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 415 {
		t.Fatalf("content type: %d", res.StatusCode)
	}
	// Oversized body.
	big := map[string]any{"author_agent_id": h.agent, "execution_context_id": h.ctxID, "title": "T", "description": "D", "body": strings.Repeat("x", 130*1024)}
	if r := h.mut("/v1/posts", big, key()); r.Status != 413 {
		t.Fatalf("oversize: %d", r.Status)
	}
	// Context origin conflict and foreign principal.
	r = h.mut("/v1/execution-contexts", map[string]any{"agent_id": h.agent, "session_id": "sess-1", "workspace_id": "test-ws", "host_id": "test-host", "launch_directory": `C:\Projects\y`, "workspace_root": `C:\Projects\y`, "path_style": "windows"}, key())
	if r.Status != 409 || !strings.Contains(string(r.Body), "context_origin_conflict") {
		t.Fatalf("origin conflict: %d %s", r.Status, r.Body)
	}
	r = h.mut("/v1/execution-contexts", map[string]any{"agent_id": h.agent, "session_id": "sess-1", "workspace_id": "test-ws", "host_id": "test-host", "launch_directory": `C:\Projects\x`, "workspace_root": `C:\Projects\x`, "working_directory": `C:\Projects\x\sub`, "path_style": "windows"}, key())
	if r.Status != 201 {
		t.Fatalf("working dir change: %d %s", r.Status, r.Body)
	}
	r = h.mut("/v1/execution-contexts", map[string]any{"agent_id": h.agent, "session_id": "sess-1", "workspace_id": "test-ws", "host_id": "test-host", "launch_directory": `C:/Projects/x`, "workspace_root": `C:\Projects\x`, "path_style": "windows"}, key())
	if r.Status != 422 {
		t.Fatalf("mixed separators accepted: %d", r.Status)
	}
	_, tok2, _ := h.db.CreateToken(context.Background(), "other", "write")
	other := *h
	other.token = tok2
	r = other.mut("/v1/posts", map[string]any{"author_agent_id": h.agent, "execution_context_id": h.ctxID, "title": "T", "description": "D", "body": "B"}, key())
	if r.Status != 403 {
		t.Fatalf("foreign author accepted: %d %s", r.Status, r.Body)
	}
	_, ro, _ := h.db.CreateToken(context.Background(), "reader", "read")
	reader := *h
	reader.token = ro
	if r := reader.mut("/v1/posts", body, key()); r.Status != 403 {
		t.Fatalf("read only wrote: %d", r.Status)
	}
	if r := reader.mut("/v1/agents", map[string]any{"client": "ro", "session_id": "s"}, key()); r.Status != 201 {
		t.Fatalf("read only cannot register: %d %s", r.Status, r.Body)
	}
}

func TestCursorsSnapshot(t *testing.T) {
	h := newHarness(t)
	h.register()
	var ids []string
	for i := 0; i < 7; i++ {
		ids = append(ids, h.post(fmt.Sprintf("Post %d", i), "body", "", nil))
	}
	r := h.do("GET", "/v1/posts?limit=3", nil, nil, true)
	var pg struct {
		Items      []map[string]any `json:"items"`
		NextCursor *string          `json:"next_cursor"`
	}
	json.Unmarshal(r.Body, &pg)
	if len(pg.Items) != 3 || pg.NextCursor == nil {
		t.Fatalf("page1: %s", r.Body)
	}
	seen := map[string]bool{}
	for _, it := range pg.Items {
		seen[it["post_id"].(string)] = true
	}
	// Insert new posts between pages and comment on an old thread.
	h.post("Late post", "body", "", nil)
	h.comment(ids[0], "late comment", nil)
	cur := *pg.NextCursor
	for cur != "" {
		r = h.do("GET", "/v1/posts?limit=3&cursor="+cur, nil, nil, true)
		if r.Status != 200 {
			t.Fatalf("page: %d %s", r.Status, r.Body)
		}
		json.Unmarshal(r.Body, &pg)
		for _, it := range pg.Items {
			id := it["post_id"].(string)
			if seen[id] {
				t.Fatalf("duplicate %s", id)
			}
			if it["title"] == "Late post" {
				t.Fatal("snapshot leaked a late post")
			}
			if id == ids[0] && it["comment_count"].(float64) != 0 {
				t.Fatal("snapshot comment count leaked")
			}
			seen[id] = true
		}
		if pg.NextCursor == nil {
			break
		}
		cur = *pg.NextCursor
	}
	if len(seen) != 7 {
		t.Fatalf("saw %d posts, want 7", len(seen))
	}
	// Cursor with mismatched params.
	r = h.do("GET", "/v1/posts?limit=2&cursor="+cur, nil, nil, true)
	if r.Status != 400 || !strings.Contains(string(r.Body), "cursor_mismatch") {
		t.Fatalf("mismatch: %d %s", r.Status, r.Body)
	}
	// Search cursor invalidation.
	r = h.do("GET", "/v1/search?q=body&limit=2", nil, nil, true)
	json.Unmarshal(r.Body, &pg)
	if pg.NextCursor == nil {
		t.Fatalf("search page: %s", r.Body)
	}
	sc := *pg.NextCursor
	r = h.do("GET", "/v1/search?q=body&limit=2&cursor="+sc, nil, nil, true)
	if r.Status != 200 {
		t.Fatalf("search page2: %d %s", r.Status, r.Body)
	}
	h.post("Another", "body", "", nil)
	r = h.do("GET", "/v1/search?q=body&limit=2&cursor="+sc, nil, nil, true)
	if r.Status != 409 || !strings.Contains(string(r.Body), "search_changed") {
		t.Fatalf("search_changed: %d %s", r.Status, r.Body)
	}
	// Comment traversal over many comments.
	for i := 0; i < 120; i++ {
		h.comment(ids[1], fmt.Sprintf("c%d", i), nil)
	}
	r = h.open("/v1/posts/"+ids[1]+"?comment_limit=50", key())
	var d storage.Discussion
	json.Unmarshal(r.Body, &d)
	if d.CommentCount != 120 || d.CommentsComplete || d.NextCommentsCursor == nil || len(d.Comments) != 50 {
		t.Fatalf("thread page1: %d %v %d", d.CommentCount, d.CommentsComplete, len(d.Comments))
	}
	total := len(d.Comments)
	cur = *d.NextCommentsCursor
	h.comment(ids[1], "after snapshot", nil)
	for {
		r = h.do("GET", "/v1/posts/"+ids[1]+"/comments?limit=50&cursor="+cur, nil, nil, true)
		var cp storage.CommentsPage
		json.Unmarshal(r.Body, &cp)
		total += len(cp.Comments)
		for _, c := range cp.Comments {
			if c.Body == "after snapshot" {
				t.Fatal("late comment leaked into snapshot")
			}
		}
		if cp.NextCursor == nil {
			if !cp.CommentsComplete {
				t.Fatal("last page not complete")
			}
			break
		}
		cur = *cp.NextCursor
	}
	if total != 120 {
		t.Fatalf("read %d comments, want 120", total)
	}
}

func TestConcurrentIdempotentWrites(t *testing.T) {
	h := newHarness(t)
	h.register()
	k := key()
	body := map[string]any{"author_agent_id": h.agent, "execution_context_id": h.ctxID, "title": "Race", "description": "D", "body": "B"}
	var wg sync.WaitGroup
	results := make([]resp, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = h.mut("/v1/posts", body, k)
		}(i)
	}
	wg.Wait()
	first := string(results[0].Body)
	for _, r := range results {
		if r.Status != 201 || string(r.Body) != first {
			t.Fatalf("concurrent idempotency: %d %s", r.Status, r.Body)
		}
	}
	ev := key()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var p struct {
				PostID string `json:"post_id"`
			}
			json.Unmarshal(results[0].Body, &p)
			results[i] = h.open("/v1/posts/"+p.PostID, ev)
		}(i)
	}
	wg.Wait()
	counted := 0
	for _, r := range results {
		if r.Status != 200 {
			t.Fatalf("concurrent open: %d %s", r.Status, r.Body)
		}
		if r.Header.Get("X-View-Counted") == "true" {
			counted++
		}
	}
	if counted != 1 {
		t.Fatalf("counted %d times", counted)
	}
}

func TestBackupExportImport(t *testing.T) {
	h := newHarness(t)
	h.register()
	p := h.post("Keep me", "body text", "ops", []string{"backup"})
	h.comment(p, "and me", nil)
	h.open("/v1/posts/"+p, key())
	dir, m, err := h.db.Backup(context.Background(), h.srv.cfg.BackupDir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Counts["entries"] != 2 || m.HighestSequence != 2 {
		t.Fatalf("manifest: %+v", m)
	}
	if _, err := storage.VerifyBackup(dir); err != nil {
		t.Fatal(err)
	}
	exp := filepath.Join(t.TempDir(), "export")
	em, err := h.db.Export(context.Background(), exp)
	if err != nil {
		t.Fatal(err)
	}
	if em.Counts["entries"] != 2 {
		t.Fatalf("export: %+v", em)
	}
	// Import into a fresh board.
	db2, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "b.db"), storage.Options{Create: true, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if _, err := db2.Import(context.Background(), exp); err != nil {
		t.Fatal(err)
	}
	st2, _ := storage.LoadState(context.Background(), db2.WriterDB())
	if st2.InstanceID == h.inst {
		t.Fatal("import kept the old instance id")
	}
	var n int
	db2.WriterDB().QueryRow(`SELECT count(*) FROM search_docs WHERE search_docs MATCH 'body'`).Scan(&n)
	if n != 1 {
		t.Fatalf("search not rebuilt on import: %d", n)
	}
	var vc int64
	db2.WriterDB().QueryRow(`SELECT view_count FROM entry_view_stats WHERE entry_sequence=1`).Scan(&vc)
	if vc != 1 {
		t.Fatalf("view count not preserved: %d", vc)
	}
	var active int
	db2.WriterDB().QueryRow(`SELECT count(*) FROM api_principals WHERE revoked_at IS NULL`).Scan(&active)
	if active != 0 {
		t.Fatal("imported principals are not revoked")
	}
	var seq int64
	db2.WriterDB().QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='entries'`).Scan(&seq)
	if seq != 2 {
		t.Fatalf("sequence watermark %d", seq)
	}
	_ = os.Remove
}
