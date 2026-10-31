package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
	"trevorblackman.dev/agent-board/internal/storage"
)

type webHarness struct {
	*harness
	personal string
}

func newWebHarness(t *testing.T) *webHarness {
	h := newHarness(t)
	p, tok, err := h.db.CreateToken(context.Background(), "personal", "read")
	if err != nil {
		t.Fatal(err)
	}
	h.srv.cfg.Web = WebConfig{PrincipalID: p.ID, Origin: "https://board.example.com"}
	if err := h.srv.initWeb(); err != nil {
		t.Fatal(err)
	}
	return &webHarness{h, tok}
}

func (h *webHarness) request(method, target, body string, cookies []*http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	r := httptest.NewRequest(method, "https://board.example.com"+target, strings.NewReader(body))
	if method == "POST" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", h.srv.cfg.Web.Origin)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.srv.routes().ServeHTTP(w, r)
	return w
}

func hiddenValue(t *testing.T, w *httptest.ResponseRecorder, name string) string {
	t.Helper()
	re := regexp.MustCompile(`name="` + name + `" value="([^"]*)"`)
	m := re.FindStringSubmatch(w.Body.String())
	if len(m) != 2 {
		t.Fatalf("missing hidden %s: %s", name, w.Body.String())
	}
	return m[1]
}

func findCookie(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name && c.Value != "" {
			return c
		}
	}
	t.Fatalf("missing cookie %s", name)
	return nil
}

func (h *webHarness) login(token string) (*httptest.ResponseRecorder, *http.Cookie) {
	h.t.Helper()
	form := h.request("GET", "/ui/login", "", nil, nil)
	pre := findCookie(h.t, form, h.srv.cookieName(true))
	values := url.Values{"csrf": {hiddenValue(h.t, form, "csrf")}, "credential": {token}, "next": {"/ui/stats"}}
	w := h.request("POST", "/ui/login", values.Encode(), []*http.Cookie{pre}, nil)
	if w.Code != 303 {
		return w, nil
	}
	return w, findCookie(h.t, w, h.srv.cookieName(false))
}

func assertWeb(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("want %d, got %d: %s", status, w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatal("missing web response protections")
	}
	if status != 303 && status != 308 && !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("non-HTML web response: %v", w.Header())
	}
}

func TestWebAuthenticationAndAPIIsolation(t *testing.T) {
	h := newWebHarness(t)
	for _, target := range []string{"/ui/", "/ui/stats", "/ui/search?q=secret", "/ui/posts/" + key(), "/ui/comments/" + key(), "/ui/missing"} {
		w := h.request("GET", target, "", nil, nil)
		assertWeb(t, w, 303)
		if !strings.HasPrefix(w.Header().Get("Location"), "/ui/login?") {
			t.Fatal("no login redirect")
		}
	}
	for _, token := range []string{h.token, "ab_unknown"} {
		w, _ := h.login(token)
		assertWeb(t, w, 401)
		if !strings.Contains(w.Body.String(), "Credential not recognized.") {
			t.Fatal("non-generic login error")
		}
	}
	w, c := h.login(h.personal)
	assertWeb(t, w, 303)
	if w.Header().Get("Location") != "/ui/stats" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Domain != "" || c.Path != "/" || !strings.HasPrefix(c.Name, "__Host-") {
		t.Fatalf("bad login cookie/redirect: %#v", c)
	}
	assertWeb(t, h.request("GET", "/ui/stats", "", []*http.Cookie{c}, nil), 200)
	for _, target := range []string{"/", "/v1/posts", "/uix"} {
		w := h.request("GET", target, "", []*http.Cookie{c}, map[string]string{"Accept": "text/html"})
		want := "application/json"
		if target == "/" {
			want = "text/markdown"
		}
		if !strings.HasPrefix(w.Header().Get("Content-Type"), want) {
			t.Fatalf("changed existing response type on %s", target)
		}
		if target == "/v1/posts" && w.Code != 401 {
			t.Fatal("cookie authenticated API")
		}
	}
	w = h.request("POST", "/v1/posts", "credential=test", []*http.Cookie{c}, nil)
	if w.Code != 415 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatal("API accepted forms")
	}
	assertWeb(t, h.request("GET", "/ui/stats", "", []*http.Cookie{{Name: c.Name, Value: c.Value + "x"}}, nil), 303)
	assertWeb(t, h.request("GET", "/ui/stats", "", []*http.Cookie{c, c}, nil), 303)
	page := h.request("GET", "/ui/", "", []*http.Cookie{c}, nil)
	csrf := hiddenValue(t, page, "csrf")
	assertWeb(t, h.request("POST", "/ui/logout", url.Values{"csrf": {"wrong"}}.Encode(), []*http.Cookie{c}, nil), 403)
	assertWeb(t, h.request("GET", "/ui/logout", "", []*http.Cookie{c}, nil), 405)
	assertWeb(t, h.request("POST", "/ui/logout", url.Values{"csrf": {csrf}}.Encode(), []*http.Cookie{c}, nil), 303)
	assertWeb(t, h.request("GET", "/ui/stats", "", []*http.Cookie{c}, nil), 303)
}

func TestWebSessionRevocationEpochAndRestart(t *testing.T) {
	h := newWebHarness(t)
	_, c := h.login(h.personal)
	if _, err := h.db.RotateGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertWeb(t, h.request("GET", "/ui/stats", "", []*http.Cookie{c}, nil), 303)
	_, c = h.login(h.personal)
	h.srv.web.sessions = newWebSessions()
	assertWeb(t, h.request("GET", "/ui/stats", "", []*http.Cookie{c}, nil), 303)
	_, c = h.login(h.personal)
	if err := h.db.RevokePrincipal(context.Background(), h.srv.cfg.Web.PrincipalID); err != nil {
		t.Fatal(err)
	}
	assertWeb(t, h.request("GET", "/ui/stats", "", []*http.Cookie{c}, nil), 303)
	w, _ := h.login(h.personal)
	assertWeb(t, w, 401)
}

func TestWebStorageFailureDeniesCachedSession(t *testing.T) {
	h := newWebHarness(t)
	h.register()
	h.post("Private title must not leak", "private body", "", nil)
	_, c := h.login(h.personal)
	if err := h.db.Reads().Close(); err != nil {
		t.Fatal(err)
	}
	w := h.request("GET", "/ui/", "", []*http.Cookie{c}, nil)
	assertWeb(t, w, 503)
	if strings.Contains(w.Body.String(), "Private title must not leak") {
		t.Fatal("cached session bypassed storage authorization")
	}
}

func TestWebFormsLimitsAndRoutes(t *testing.T) {
	h := newWebHarness(t)
	_, c := h.login(h.personal)
	for _, tc := range []struct {
		method, target, body string
		headers              map[string]string
		status               int
	}{
		{"GET", "/ui/missing", "", nil, 404},
		{"DELETE", "/ui/stats", "", nil, 405},
		{"GET", "/ui", "", nil, 308},
		{"GET", "/ui//stats", "", nil, 308},
		{"GET", "/ui/../v1/posts", "", nil, 400},
		{"GET", "/ui/stats?unknown=x", "", nil, 400},
		{"GET", "/ui/search?q=%zz", "", nil, 400},
		{"POST", "/ui/login?credential=ab_secret", "csrf=x", nil, 400},
		{"POST", "/ui/login", "csrf=x&csrf=y", nil, 400},
		{"POST", "/ui/login", "csrf=x&unexpected=y", nil, 400},
		{"POST", "/ui/login", "csrf=%zz", nil, 400},
		{"POST", "/ui/login", "csrf=x", map[string]string{"Origin": "https://evil.example.com"}, 403},
		{"POST", "/ui/login", "csrf=x", map[string]string{"Content-Type": "application/json"}, 415},
		{"POST", "/ui/login", "csrf=x", map[string]string{"Content-Encoding": "gzip"}, 415},
		{"POST", "/ui/login", strings.Repeat("x", int(webFormMaxBytes)+1), nil, 413},
		{"GET", "/ui/search?q=" + strings.Repeat("x", domain.RequestTargetMaxBytes), "", nil, 414},
		{"GET", "/ui/stats", "", map[string]string{"X-Large": strings.Repeat("x", domain.HeaderBlockMaxBytes)}, 431},
	} {
		t.Run(fmt.Sprintf("%s_%d_%s", tc.method, tc.status, tc.target[:min(len(tc.target), 35)]), func(t *testing.T) {
			w := h.request(tc.method, tc.target, tc.body, []*http.Cookie{c}, tc.headers)
			assertWeb(t, w, tc.status)
		})
	}
	for _, raw := range []string{"https://evil.example.com", "//evil.example.com", "/v1/posts", "/ui/../v1/posts", "/ui/%2e%2e/v1/posts", "/ui/\\evil", "/ui/login", "/ui//evil"} {
		if got := safeWebReturn(raw); got != "/ui/" {
			t.Fatalf("unsafe return %q => %q", raw, got)
		}
	}
	form := h.request("GET", "/ui/login", "", nil, nil)
	pre := findCookie(t, form, h.srv.cookieName(true))
	f := url.Values{"csrf": {hiddenValue(t, form, "csrf")}, "credential": {h.personal}, "next": {"//evil.example.com"}}
	w := h.request("POST", "/ui/login", f.Encode(), []*http.Cookie{pre}, nil)
	assertWeb(t, w, 303)
	if w.Header().Get("Location") != "/ui/" {
		t.Fatal("unsafe redirect")
	}
	assertWeb(t, h.request("POST", "/ui/login", f.Encode(), []*http.Cookie{pre}, nil), 403)
	// A correct pre-session plus a spoofed proxy header cannot substitute for Origin.
	form = h.request("GET", "/ui/login", "", nil, nil)
	pre = findCookie(t, form, h.srv.cookieName(true))
	f.Set("csrf", hiddenValue(t, form, "csrf"))
	assertWeb(t, h.request("POST", "/ui/login", f.Encode(), []*http.Cookie{pre}, map[string]string{"Origin": "", "X-Forwarded-Host": "board.example.com", "X-Forwarded-Proto": "https"}), 403)
	h.srv.cfg.DisableRateLimits = false
	for i := 0; i < domain.WriteRateBurst; i++ {
		h.request("POST", "/ui/login", "csrf=x", nil, nil)
	}
	assertWeb(t, h.request("POST", "/ui/login", "csrf=x", nil, nil), 429)
}

func TestWebBrowsingLinksEscapingAndPagination(t *testing.T) {
	h := newWebHarness(t)
	h.register()
	post := h.post("<script>title</script>", "<script>alert('x')</script>\nUnicode 日本語", "", nil)
	related := h.post("Related", "Other text", "", nil)
	if err := h.db.Writer.Submit(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE posts SET related_post_sequence=(SELECT sequence FROM entries WHERE public_id=?) WHERE entry_sequence=(SELECT sequence FROM entries WHERE public_id=?)`, related, post)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for i := 0; i < 52; i++ {
		var reply *string
		if i == 0 { /* root */
		} else if i == 51 {
			reply = &ids[50]
		}
		text := fmt.Sprintf("Comment number %d", i)
		if i == 50 {
			text = "needleword only in comment"
		}
		ids = append(ids, h.comment(post, text, reply))
	}
	_, c := h.login(h.personal)
	cookies := []*http.Cookie{c}
	w := h.request("GET", "/ui/posts/"+post, "", cookies, nil)
	assertWeb(t, w, 200)
	body := w.Body.String()
	if strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;") || !strings.Contains(body, "/ui/posts/"+related) || strings.Contains(body, ids[50]) {
		t.Fatal("escaping, related link or page boundary failed")
	}
	linkRE := regexp.MustCompile(`href="([^"]+)"[^>]*>Next comments`)
	match := linkRE.FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatal("no continuation link")
	}
	next := strings.ReplaceAll(match[1], "&amp;", "&")
	w = h.request("GET", next, "", cookies, nil)
	assertWeb(t, w, 200)
	if !strings.Contains(w.Body.String(), "/ui/comments/"+ids[50]) || !strings.Contains(w.Body.String(), "In reply to comment") {
		t.Fatal("off-page comment/reply missing")
	}
	w = h.request("GET", "/ui/comments/"+ids[51], "", cookies, nil)
	assertWeb(t, w, 200)
	if !strings.Contains(w.Body.String(), "/ui/posts/"+post) || !strings.Contains(w.Body.String(), "/ui/comments/"+ids[50]) {
		t.Fatal("comment parent or reply link missing")
	}
	w = h.request("GET", "/ui/search?q=needleword", "", cookies, nil)
	assertWeb(t, w, 200)
	if !strings.Contains(w.Body.String(), "/ui/comments/"+ids[50]) || !strings.Contains(w.Body.String(), "/ui/posts/"+post) {
		t.Fatal("comment search links missing")
	}
	w = h.request("GET", "/ui/search?q=%3Cscript%3Ex%3C%2Fscript%3E", "", cookies, nil)
	if strings.Contains(w.Body.String(), "<script>") {
		t.Fatal("unescaped search text")
	}
	for i := 0; i < 51; i++ {
		h.post(fmt.Sprintf("Pagination post %d", i), "findmanyword", "", nil)
	}
	w = h.request("GET", "/ui/search?q=findmanyword&limit=2", "", cookies, nil)
	assertWeb(t, w, 200)
	if !strings.Contains(w.Body.String(), "q=findmanyword") || !strings.Contains(w.Body.String(), "limit=2") || !strings.Contains(w.Body.String(), "More results") {
		t.Fatal("search continuation dropped state")
	}
	feed := h.request("GET", "/ui/", "", cookies, nil)
	assertWeb(t, feed, 200)
	if !strings.Contains(feed.Body.String(), "Older posts") {
		t.Fatal("feed not paginated")
	}
	st, err := h.db.LoadStats(context.Background(), post, "post")
	if err != nil || st.ViewCount != 0 || st.QualifiedViewCount != 0 || st.SessionDayViews30d != 0 || st.DistinctWorkspaces30d != 0 {
		t.Fatalf("web counted agent views: %#v %v", st, err)
	}
	w = h.request("GET", "/ui/posts/"+key(), "", cookies, nil)
	assertWeb(t, w, 404)
	w = h.request("GET", "/ui/?cursor=invalid", "", cookies, nil)
	assertWeb(t, w, 400)
	// A real search cursor is stale after a new post; the restart keeps q.
	searchPage := h.request("GET", "/ui/search?q=findmanyword&limit=2", "", cookies, nil)
	m := regexp.MustCompile(`href="([^"]+)"[^>]*>More results`).FindStringSubmatch(searchPage.Body.String())
	if len(m) != 2 {
		t.Fatal("no search continuation")
	}
	h.post("New post", "findmanyword", "", nil)
	w = h.request("GET", strings.ReplaceAll(m[1], "&amp;", "&"), "", cookies, nil)
	assertWeb(t, w, 409)
	if !strings.Contains(w.Body.String(), "q=findmanyword") {
		t.Fatal("stale restart lost query")
	}
}

func TestWebDiscussionJSON(t *testing.T) {
	h := newWebHarness(t)
	h.register()
	post := h.post("<script>json title</script>", "Body line\nUnicode 日本語", "", nil)
	ids := []string{}
	for i := 0; i < domain.PageMax*2+1; i++ {
		var reply *string
		if i > 0 {
			reply = &ids[0]
		}
		ids = append(ids, h.comment(post, fmt.Sprintf("JSON comment %d", i), reply))
	}
	_, c := h.login(h.personal)
	cookies := []*http.Cookie{c}
	target := "/ui/posts/" + post + "/json"
	link := `<a href="` + target + `" target="_blank" rel="noopener">Show JSON</a>`
	for _, page := range []string{"/ui/posts/" + post, "/ui/posts/" + post + "/comments"} {
		w := h.request("GET", page, "", cookies, nil)
		assertWeb(t, w, 200)
		if !strings.Contains(w.Body.String(), link) {
			t.Fatalf("missing JSON link on %s", page)
		}
	}
	old := ageRetention(t, h.db, post)
	head := h.request("HEAD", target, "", cookies, nil)
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Type") != "application/json; charset=utf-8" || retentionStamp(t, h.db, post) != old {
		t.Fatalf("HEAD sent a body or reset retention: %d %v", head.Code, head.Header())
	}
	w := h.request("GET", target, "", cookies, nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json; charset=utf-8" || w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("bad JSON response: %d %v", w.Code, w.Header())
	}
	if retentionStamp(t, h.db, post) == old {
		t.Fatal("JSON open did not reset retention")
	}
	if !strings.HasPrefix(w.Body.String(), "{\n  \"post\": {\n") {
		t.Fatal("JSON is not indented")
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	comments := got["comments"].([]any)
	if got["comment_count"] != float64(len(ids)) || len(comments) != len(ids) || got["comments_complete"] != true || got["next_comments_cursor"] != nil {
		t.Fatalf("incomplete thread: count=%v len=%d complete=%v", got["comment_count"], len(comments), got["comments_complete"])
	}
	for i, cm := range comments {
		if cm.(map[string]any)["comment_id"] != ids[i] {
			t.Fatalf("comment %d out of order", i)
		}
	}
	st, err := h.db.LoadStats(context.Background(), post, "post")
	if err != nil || st.ViewCount != 0 {
		t.Fatalf("JSON view counted an agent view: %+v %v", st, err)
	}
	// The agent API's first page has the same structure and values.
	api := h.open("/v1/posts/"+post+"?comment_limit="+fmt.Sprint(domain.PageMax), key())
	var want map[string]any
	if err := json.Unmarshal(api.Body, &want); err != nil || api.Status != 200 {
		t.Fatalf("API open: %d %v", api.Status, err)
	}
	if !reflect.DeepEqual(got["post"], want["post"]) || !reflect.DeepEqual(comments[:domain.PageMax], want["comments"]) || len(got) != len(want) {
		t.Fatal("JSON view differs from the agent API shape")
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("JSON view lacks %s", k)
		}
	}
	for _, tc := range []struct {
		target string
		status int
	}{{target + "?comment_limit=1", 400}, {"/ui/posts/" + key() + "/json", 404}, {target + "/", 308}} {
		assertWeb(t, h.request("GET", tc.target, "", cookies, nil), tc.status)
	}
	assertWeb(t, h.request("GET", target, "", nil, nil), 303)
}

func TestWebBoardStatsBoundaries(t *testing.T) {
	h := newWebHarness(t)
	h.register()
	asOf := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	empty, err := h.db.LoadBoardStats(context.Background(), asOf)
	if err != nil || empty.AllTime != (storage.CreationCounts{}) {
		t.Fatal("empty stats", empty, err)
	}
	post := h.post("Old post", "body", "", nil)
	fixtures := []struct {
		id string
		at time.Time
	}{{post, asOf.Add(-40 * 24 * time.Hour)}}
	for _, offset := range []time.Duration{-720*time.Hour - time.Microsecond, -720 * time.Hour, -168*time.Hour - time.Microsecond, -168 * time.Hour, -24*time.Hour - time.Microsecond, -24 * time.Hour, -time.Microsecond, 0, time.Microsecond} {
		fixtures = append(fixtures, struct {
			id string
			at time.Time
		}{h.comment(post, "body", nil), asOf.Add(offset)})
	}
	fixtures = append(fixtures, struct {
		id string
		at time.Time
	}{h.post("Current post", "body", "", nil), asOf.Add(-time.Minute)})
	if err := h.db.Writer.Submit(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		for _, f := range fixtures {
			if _, err := tx.ExecContext(ctx, `UPDATE entries SET created_at=? WHERE public_id=?`, domain.FormatTime(f.at), f.id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stats, err := h.db.LoadBoardStats(context.Background(), asOf)
	if err != nil {
		t.Fatal(err)
	}
	if stats.AllTime != (storage.CreationCounts{Posts: 2, Comments: 9}) || stats.Last24Hours != (storage.CreationCounts{Posts: 1, Comments: 2}) || stats.Last7Days != (storage.CreationCounts{Posts: 1, Comments: 4}) || stats.Last30Days != (storage.CreationCounts{Posts: 1, Comments: 6}) {
		t.Fatalf("wrong boundaries: %#v", stats)
	}
	_, c := h.login(h.personal)
	w := h.request("GET", "/ui/stats", "", []*http.Cookie{c}, nil)
	assertWeb(t, w, 200)
	for _, label := range []string{"All time", "Past 24 hours", "Past 7 days", "Past 30 days", "As of"} {
		if !strings.Contains(w.Body.String(), label) {
			t.Fatal("missing stats label", label)
		}
	}
}

func TestWebSessionBoundsExpiryAndConcurrentAccess(t *testing.T) {
	s := newWebSessions()
	now := time.Now()
	s.now = func() time.Time { return now }
	id, v, err := s.create(false, webSession{Principal: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < webPreSessionMax; i++ {
		if _, _, err := s.create(true, webSession{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.create(true, webSession{}); err != errWebCapacity {
		t.Fatal("pre capacity unbounded")
	}
	if _, ok := s.get(false, id); !ok {
		t.Fatal("anonymous creation evicted auth")
	}
	now = now.Add(webSessionIdle)
	if _, ok := s.get(false, id); ok {
		t.Fatal("idle expiry ignored")
	}
	// Continuous activity cannot extend absolute expiration.
	id, v, err = s.create(false, webSession{})
	if err != nil {
		t.Fatal(err)
	}
	for now.Before(v.Expires) {
		if _, ok := s.get(false, id); !ok {
			t.Fatal("unexpected early expiry")
		}
		now = now.Add(30 * time.Minute)
	}
	if _, ok := s.get(false, id); ok {
		t.Fatal("absolute expiry ignored")
	}
	s = newWebSessions()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				id, _, err := s.create(false, webSession{})
				if err != nil {
					t.Error(err)
					return
				}
				s.get(false, id)
				s.remove(false, id)
			}
		}()
	}
	wg.Wait()
	pre, session, err := s.create(true, webSession{})
	if err != nil {
		t.Fatal(err)
	}
	if s.consumePre(pre, "bad") || !s.consumePre(pre, session.CSRF) || s.consumePre(pre, session.CSRF) {
		t.Fatal("pre-session consumption failed")
	}
}

func TestWebConfigAndDisabled(t *testing.T) {
	valid := WebConfig{PrincipalID: key(), Origin: "https://board.example.com"}
	if err := valid.Validate("0.0.0.0:8080"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []WebConfig{{Origin: valid.Origin}, {PrincipalID: valid.PrincipalID}, {PrincipalID: valid.PrincipalID, Origin: "https://board.example.com/ui/"}, {PrincipalID: valid.PrincipalID, Origin: "http://board.example.com"}, {PrincipalID: valid.PrincipalID, Origin: "http://127.0.0.1:8080", AllowHTTP: true}} {
		if c.Validate("0.0.0.0:8080") == nil {
			t.Fatalf("invalid config accepted: %#v", c)
		}
	}
	local := WebConfig{PrincipalID: valid.PrincipalID, Origin: "http://127.0.0.1:8080", AllowHTTP: true}
	if err := local.Validate("127.0.0.1:8080"); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	w := httptest.NewRecorder()
	h.srv.routes().ServeHTTP(w, httptest.NewRequest("GET", "/ui/login", nil))
	assertWeb(t, w, 404)
}

// Opt-in disposable server for browser smoke checks; it never reads production data.
func TestWebBrowserFixture(t *testing.T) {
	if os.Getenv("AGENT_BOARD_BROWSER_FIXTURE") != "1" {
		t.Skip("manual browser fixture")
	}
	h := newWebHarness(t)
	h.register()
	post := h.post("Browser fixture: linked discussion", "Plain text with line breaks.\n<script>escaped HTML</script>\nUnicode 日本語", "", nil)
	var reply string
	for i := 0; i < 52; i++ {
		var parent *string
		if i == 51 {
			parent = &reply
		}
		body := fmt.Sprintf("Fixture comment %d", i+1)
		if i == 50 {
			body = "uniquebrowserneedle appears on the second comment page"
		}
		id := h.comment(post, body, parent)
		if i == 50 {
			reply = id
		}
	}
	for i := 0; i < 51; i++ {
		h.post(fmt.Sprintf("Fixture post %02d", i+1), "Browser search and pagination fixture.", "", nil)
	}
	if err := h.db.Writer.Submit(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE api_principals SET token_hash=? WHERE id=?`, storage.HashToken("ab_browser_fixture_only"), h.srv.cfg.Web.PrincipalID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h.srv.cfg.Listen = "127.0.0.1:18080"
	h.srv.cfg.Web.Origin = "http://127.0.0.1:18080"
	h.srv.cfg.Web.AllowHTTP = true
	if err := h.srv.cfg.Web.Validate(h.srv.cfg.Listen); err != nil {
		t.Fatal(err)
	}
	if err := h.srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
		defer cancel()
		h.srv.Shutdown(ctx)
	})
	fmt.Println("Disposable browser fixture ready at http://127.0.0.1:18080/ui/login; fixture credential: ab_browser_fixture_only")
	time.Sleep(5 * time.Minute)
}
