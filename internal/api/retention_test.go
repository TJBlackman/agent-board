package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
	"trevorblackman.dev/agent-board/internal/storage"
)

func retentionStamp(t *testing.T, db *storage.DB, post string) string {
	t.Helper()
	var stamp string
	if err := db.Reads().QueryRow(`SELECT retention_activity_at FROM posts JOIN entries ON sequence=entry_sequence WHERE public_id=?`, post).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	return stamp
}

func ageRetention(t *testing.T, db *storage.DB, post string) string {
	t.Helper()
	old := domain.FormatTime(time.Now().Add(-domain.PostInactivityRetention - time.Hour))
	if err := db.Writer.Submit(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE posts SET retention_activity_at=? WHERE entry_sequence=(SELECT sequence FROM entries WHERE public_id=?)`, old, post)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return old
}

func TestWebRetentionSuccessfulOpensOnly(t *testing.T) {
	h := newWebHarness(t)
	h.register()
	p := h.post("retention title", "retentionneedle", "", nil)
	c := h.comment(p, "nested comment", nil)
	c = h.comment(p, "deep reply", &c)
	_, cookie := h.login(h.personal)
	for _, target := range []string{"/ui/posts/" + p, "/ui/posts/" + p + "/comments", "/ui/comments/" + c} {
		old := ageRetention(t, h.db, p)
		assertWeb(t, h.request("GET", target, "", []*http.Cookie{cookie}, nil), 200)
		if retentionStamp(t, h.db, p) == old {
			t.Fatalf("open did not reset: %s", target)
		}
	}
	for _, test := range []struct {
		method, target string
		status         int
		authenticated  bool
	}{
		{"HEAD", "/ui/posts/" + p, 200, true},
		{"GET", "/ui/", 200, true},
		{"GET", "/ui/search?q=retentionneedle", 200, true},
		{"GET", "/ui/stats", 200, true},
		{"GET", "/ui/posts/" + p + "?unknown=1", 400, true},
		{"GET", "/ui/posts/" + p + "/comments?cursor=invalid", 400, true},
		{"GET", "/ui/posts/" + p + "/", 308, true},
		{"GET", "/ui/posts/" + p, 303, false},
	} {
		old := ageRetention(t, h.db, p)
		var cookies []*http.Cookie
		if test.authenticated {
			cookies = []*http.Cookie{cookie}
		}
		assertWeb(t, h.request(test.method, test.target, "", cookies, nil), test.status)
		if retentionStamp(t, h.db, p) != old {
			t.Fatalf("non-open reset: %s %s", test.method, test.target)
		}
	}
	old := ageRetention(t, h.db, p)
	saved := h.srv.web.templates
	h.srv.web.templates = template.Must(template.New("broken").Parse("no page template"))
	assertWeb(t, h.request("GET", "/ui/posts/"+p, "", []*http.Cookie{cookie}, nil), 500)
	h.srv.web.templates = saved
	if retentionStamp(t, h.db, p) != old {
		t.Fatal("render failure reset activity")
	}
	stats, err := h.db.LoadStats(context.Background(), p, "post")
	if err != nil || stats.ViewCount != 0 {
		t.Fatalf("UI affected agent views: %+v %v", stats, err)
	}
}

func TestWebRetentionCleanupBetweenReadAndRender(t *testing.T) {
	h := newWebHarness(t)
	h.register()
	p := h.post("deleted private title", "private contents", "", nil)
	d, err := h.db.GetDiscussion(context.Background(), h.srv.cfg.Web.PrincipalID, p, 10)
	if err != nil {
		t.Fatal(err)
	}
	ageRetention(t, h.db, p)
	if _, err := h.db.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "https://board.example.com/ui/posts/"+p, nil)
	r = r.WithContext(context.WithValue(r.Context(), webSessionKey{}, webSession{Principal: h.srv.cfg.Web.PrincipalID, Instance: d.Instance, Generation: d.Generation}))
	w := httptest.NewRecorder()
	h.srv.renderWeb(w, r, 200, webPage{Title: d.Post.Title, View: "discussion", Discussion: d, PostID: p})
	if w.Code != 404 || strings.Contains(w.Body.String(), d.Post.Title) || strings.Contains(w.Body.String(), d.Post.Body) {
		t.Fatalf("stale content emitted: %d %s", w.Code, w.Body.String())
	}
}

func TestRetentionMutationReplayAndReadOnlyRoutes(t *testing.T) {
	h := newHarness(t)
	h.register()
	p := h.post("retention title", "retentionneedle", "", nil)
	for _, path := range []string{"/v1/posts", "/v1/search?q=retentionneedle", "/v1/posts/" + p + "/stats", "/v1/posts/" + p + "/comments"} {
		old := ageRetention(t, h.db, p)
		r := h.do("GET", path, nil, nil, true)
		if r.Status != 200 || retentionStamp(t, h.db, p) != old {
			t.Fatalf("listing reset: %s %d", path, r.Status)
		}
	}
	body := map[string]any{"author_agent_id": h.agent, "execution_context_id": h.ctxID, "body": "comment"}
	idem := key()
	r := h.mut("/v1/posts/"+p+"/comments", body, idem)
	if r.Status != 201 {
		t.Fatalf("comment: %d %s", r.Status, r.Body)
	}
	stamp := retentionStamp(t, h.db, p)
	r = h.mut("/v1/posts/"+p+"/comments", body, idem)
	if r.Status != 201 || retentionStamp(t, h.db, p) != stamp {
		t.Fatal("mutation replay reset timer")
	}
	// Search pagination must detect deletion via the content revision.
	h.post("other title", "retentionneedle", "", nil)
	r = h.do("GET", "/v1/search?q=retentionneedle&limit=1", nil, nil, true)
	var search struct {
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(r.Body, &search); err != nil {
		t.Fatal(err)
	}
	if search.NextCursor == nil {
		t.Fatal("missing search cursor")
	}
	ageRetention(t, h.db, p)
	if _, err := h.db.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	r = h.do("GET", "/v1/search?q=retentionneedle&limit=1&cursor="+*search.NextCursor, nil, nil, true)
	if r.Status != 409 {
		t.Fatalf("search cursor survived deletion: %d %s", r.Status, r.Body)
	}
}

func TestBackgroundRetentionDrainsFullPostBatch(t *testing.T) {
	h := newHarness(t)
	h.register()
	for i := 0; i < domain.PostCleanupBatch+1; i++ {
		p := h.post("idle", "idle", "", nil)
		ageRetention(t, h.db, p)
	}
	h.srv.runCleanup()
	var count int
	if err := h.db.Reads().QueryRow(`SELECT count(*) FROM posts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("background stopped early: %d %v", count, err)
	}
	if h.srv.metrics.DeletedPosts != domain.PostCleanupBatch+1 {
		t.Fatal("deletion metrics missing")
	}
	p := h.post("idle", "idle", "", nil)
	ageRetention(t, h.db, p)
	w := httptest.NewRecorder()
	h.srv.adminRoutes().ServeHTTP(w, httptest.NewRequest("POST", "http://127.0.0.1/admin/cleanup", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"posts_deleted":1`) || h.srv.metrics.DeletedPosts != domain.PostCleanupBatch+2 {
		t.Fatalf("operator cleanup: %d %s", w.Code, w.Body.String())
	}
}
