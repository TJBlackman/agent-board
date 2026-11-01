package api

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
	"trevorblackman.dev/agent-board/internal/search"
	"trevorblackman.dev/agent-board/internal/storage"
)

//go:embed web/*.html web/*.css
var webFiles embed.FS

const webFormMaxBytes int64 = 32 * 1024

type webView struct {
	templates *template.Template
	css       []byte
	sessions  *webSessions
}

type webPage struct {
	Title, View, Error, Query, Next, NextLabel, Restart, PostID, Return, CSRF string
	Authenticated                                                             bool
	Cards                                                                     []storage.Card
	Hits                                                                      []storage.SearchCard
	Discussion                                                                *storage.Discussion
	Comments                                                                  *storage.CommentsPage
	Comment                                                                   *storage.CommentResult
	Stats                                                                     *storage.BoardStats
}

type webSessionKey struct{}

func (s *Server) initWeb() error {
	f := template.FuncMap{"time": func(raw string) string {
		t, err := domain.ParseTime(raw)
		if err != nil {
			return raw
		}
		return t.In(s.cfg.Location).Format("Jan 2, 2006 3:04 PM MST")
	}, "instant": func(t time.Time) string { return t.In(s.cfg.Location).Format("Jan 2, 2006 3:04:05 PM MST") }}
	t, err := template.New("board").Funcs(f).ParseFS(webFiles, "web/*.html")
	if err != nil {
		return err
	}
	css, err := webFiles.ReadFile("web/board.css")
	if err != nil {
		return err
	}
	s.web = &webView{templates: t, css: css, sessions: newWebSessions()}
	return nil
}

func webHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// no-referrer makes ordinary browser form POSTs send Origin: null.
	// same-origin preserves our login/logout origin check without external referrers.
	w.Header().Set("Referrer-Policy", "same-origin")
}

func (s *Server) renderWeb(w http.ResponseWriter, r *http.Request, status int, p webPage) {
	if sess, ok := r.Context().Value(webSessionKey{}).(webSession); ok {
		p.Authenticated, p.CSRF = true, sess.CSRF
	}
	var b bytes.Buffer
	if s.web == nil || s.web.templates.ExecuteTemplate(&b, "page", p) != nil {
		if s.web != nil {
			status = http.StatusInternalServerError
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			w.Write([]byte("<!doctype html><html lang=\"en\"><title>Agent board</title><h1>Agent board</h1><p>This page is unavailable.</p></html>"))
		}
		return
	}
	if status == http.StatusOK && r.Method == http.MethodGet {
		var id, kind string
		switch p.View {
		case "discussion", "comments":
			id, kind = p.PostID, "post"
		case "comment":
			id, kind = p.Comment.CommentID, "comment"
		}
		if id != "" && !s.touchWebOpen(w, r, id, kind) {
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		w.Write(b.Bytes())
	}
}

// touchWebOpen resets retention for a deliberate open. It also confirms the
// entry still exists, so content removed after the read is never emitted.
func (s *Server) touchWebOpen(w http.ResponseWriter, r *http.Request, id, kind string) bool {
	sess, ok := r.Context().Value(webSessionKey{}).(webSession)
	if !ok {
		s.webReadError(w, r, storage.ErrForbidden)
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), domain.WriteTotalTimeout)
	err := s.db.TouchWebOpen(ctx, sess.Principal, sess.Instance, sess.Generation, id, kind)
	cancel()
	if err != nil {
		s.webReadError(w, r, err)
		return false
	}
	return true
}

func (s *Server) webError(w http.ResponseWriter, r *http.Request, e *apiError) {
	if e.Retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.Retry))
	}
	restart := "/ui/"
	if r.URL.Path == "/ui/search" {
		restart = "/ui/search?q=" + url.QueryEscape(r.URL.Query().Get("q"))
	}
	if strings.HasSuffix(r.URL.Path, "/comments") {
		restart = strings.TrimSuffix(r.URL.Path, "/comments")
	}
	if strings.HasSuffix(r.URL.Path, "/json") {
		restart = strings.TrimSuffix(r.URL.Path, "/json")
	}
	s.renderWeb(w, r, e.Status, webPage{Title: http.StatusText(e.Status), View: "error", Error: e.Message, Restart: restart})
}

func (s *Server) webHandler() http.Handler {
	inner := s.withRequestLimits(http.HandlerFunc(s.routeWeb), s.webError, true)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webHeaders(w)
		if s.web == nil {
			s.webError(w, r, errf(404, "not_found", "Browser view is disabled."))
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// Own routing and canonicalization so ServeMux cannot redirect out of the UI namespace.
func (s *Server) routeWeb(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	clean := path.Clean(p)
	if !isWebPath(clean) {
		s.webError(w, r, errf(400, "invalid_path", "Invalid browser path."))
		return
	}
	canonical := clean
	if clean == "/ui" {
		canonical = "/ui/"
	}
	if canonical != p {
		if r.Method != "GET" && r.Method != "HEAD" {
			s.webError(w, r, errf(400, "invalid_path", "Use the canonical browser path."))
			return
		}
		u := &url.URL{Path: canonical, RawQuery: r.URL.RawQuery}
		http.Redirect(w, r, u.String(), http.StatusPermanentRedirect)
		return
	}
	allow := "GET, HEAD"
	if p == "/ui/login" {
		allow = "GET, HEAD, POST"
	}
	if p == "/ui/logout" {
		allow = "POST"
	}
	if r.Method != "GET" && r.Method != "HEAD" && !(r.Method == "POST" && (p == "/ui/login" || p == "/ui/logout")) || p == "/ui/logout" && r.Method != "POST" {
		w.Header().Set("Allow", allow)
		s.webError(w, r, errf(405, "method_not_allowed", "This method is not supported."))
		return
	}
	if p == "/ui/assets/board.css" {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		if r.Method != "HEAD" {
			w.Write(s.web.css)
		}
		return
	}
	if p == "/ui/login" {
		if r.Method == "POST" {
			s.webLogin(w, r)
		} else {
			s.webLoginForm(w, r)
		}
		return
	}
	sess, ok := s.authorizeWeb(w, r)
	if !ok {
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), webSessionKey{}, sess))
	if p == "/ui/logout" {
		s.webLogout(w, r)
		return
	}
	if !s.cfg.DisableRateLimits && !s.limiter.allow(sess.Principal, "read") {
		s.webError(w, r, &apiError{Status: 429, Message: "Read limit reached. Try again shortly.", Retry: 5})
		return
	}
	release, ok := s.adm.acquire(p == "/ui/search")
	if !ok {
		s.webError(w, r, &apiError{Status: 429, Message: "The board is busy. Try again shortly.", Retry: 1})
		return
	}
	defer release()
	switch p {
	case "/ui/":
		s.webFeed(w, r, sess.Principal)
	case "/ui/search":
		s.webSearch(w, r, sess.Principal)
	case "/ui/stats":
		if _, e := webParams(r); e != nil {
			s.webError(w, r, e)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), domain.FeedDBTimeout)
		defer cancel()
		v, err := s.db.LoadBoardStats(ctx, time.Now())
		if err != nil {
			s.webReadError(w, r, err)
			return
		}
		s.renderWeb(w, r, 200, webPage{Title: "Stats", View: "stats", Stats: v})
	default:
		parts := strings.Split(strings.TrimPrefix(p, "/ui/"), "/")
		if len(parts) == 2 && parts[0] == "posts" && domain.IsUUID(parts[1]) {
			s.webDiscussion(w, r, sess.Principal, parts[1])
			return
		}
		if len(parts) == 3 && parts[0] == "posts" && parts[2] == "comments" && domain.IsUUID(parts[1]) {
			s.webComments(w, r, sess.Principal, parts[1])
			return
		}
		if len(parts) == 3 && parts[0] == "posts" && parts[2] == "json" && domain.IsUUID(parts[1]) {
			s.webDiscussionJSON(w, r, parts[1])
			return
		}
		if len(parts) == 2 && parts[0] == "comments" && domain.IsUUID(parts[1]) {
			if _, e := webParams(r); e != nil {
				s.webError(w, r, e)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), domain.DiscussionDBTimeout)
			defer cancel()
			v, err := s.db.GetComment(ctx, parts[1])
			if err != nil {
				s.webReadError(w, r, err)
				return
			}
			s.renderWeb(w, r, 200, webPage{Title: "Comment", View: "comment", Comment: v})
			return
		}
		s.webError(w, r, errf(404, "not_found", "That page does not exist."))
	}
}

func (s *Server) cookieName(pre bool) string {
	prefix := "__Host-"
	if s.cfg.Web.AllowHTTP {
		prefix = ""
	}
	name := prefix + "agent-board-ui"
	if pre {
		name += "-pre"
	}
	return name
}

func (s *Server) setWebCookie(w http.ResponseWriter, pre bool, id string, expires time.Time) {
	max := int(time.Until(expires).Seconds())
	if id == "" {
		max = -1
		expires = time.Unix(1, 0)
	}
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(pre), Value: id, Path: "/", Secure: !s.cfg.Web.AllowHTTP, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: max})
}

func (s *Server) cookieID(r *http.Request, pre bool) string {
	var id string
	n := 0
	for _, c := range r.Cookies() {
		if c.Name == s.cookieName(pre) {
			id = c.Value
			n++
		}
	}
	if n != 1 {
		return ""
	}
	return id
}

func (s *Server) currentWebSession(r *http.Request) (webSession, bool, error) {
	id := s.cookieID(r, false)
	v, ok := s.web.sessions.get(false, id)
	if !ok {
		return v, false, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), domain.FeedDBTimeout)
	defer cancel()
	facts, err := s.db.LoadWebIdentity(ctx, s.cfg.Web.PrincipalID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (v.Principal != facts.PrincipalID || v.Instance != facts.InstanceID || v.Generation != facts.GenerationID) {
		s.web.sessions.remove(false, id)
		return webSession{}, false, nil
	}
	if err != nil {
		return webSession{}, false, err
	}
	return v, true, nil
}

func (s *Server) authorizeWeb(w http.ResponseWriter, r *http.Request) (webSession, bool) {
	v, ok, err := s.currentWebSession(r)
	if err != nil {
		s.webError(w, r, &apiError{Status: 503, Message: "The board is temporarily unavailable.", Retry: 5})
		return v, false
	}
	if !ok {
		s.setWebCookie(w, false, "", time.Time{})
		if r.Method == "POST" {
			s.webError(w, r, errf(401, "unauthorized", "Please sign in again."))
			return v, false
		}
		u := &url.URL{Path: "/ui/login", RawQuery: url.Values{"next": {safeWebReturn(r.URL.RequestURI())}}.Encode()}
		http.Redirect(w, r, u.String(), http.StatusSeeOther)
		return v, false
	}
	return v, true
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request, status int, message, next string) {
	id := s.cookieID(r, true)
	v, ok := s.web.sessions.get(true, id)
	if !ok {
		var err error
		id, v, err = s.web.sessions.create(true, webSession{})
		if err != nil {
			s.webError(w, r, &apiError{Status: 503, Message: "Sign-in capacity reached. Try again shortly.", Retry: 60})
			return
		}
		s.setWebCookie(w, true, id, v.Expires)
	}
	s.renderWeb(w, r, status, webPage{Title: "Sign in", View: "login", Error: message, CSRF: v.CSRF, Return: safeWebReturn(next)})
}

func (s *Server) webLoginForm(w http.ResponseWriter, r *http.Request) {
	q, e := webParams(r, "next")
	if e != nil {
		s.webError(w, r, e)
		return
	}
	_, ok, err := s.currentWebSession(r)
	if err != nil {
		s.webError(w, r, &apiError{Status: 503, Message: "The board is temporarily unavailable.", Retry: 5})
		return
	}
	if ok {
		http.Redirect(w, r, safeWebReturn(q.str("next")), http.StatusSeeOther)
		return
	}
	if !s.cfg.DisableRateLimits && !s.limiter.allow("web-pre", "write") {
		s.webError(w, r, &apiError{Status: 429, Message: "Too many sign-in requests. Try again shortly.", Retry: 5})
		return
	}
	s.loginPage(w, r, 200, "", q.str("next"))
}

func (s *Server) webOriginOK(r *http.Request) bool {
	values := r.Header.Values("Origin")
	if len(values) > 0 {
		return len(values) == 1 && values[0] == s.cfg.Web.Origin
	}
	refs := r.Header.Values("Referer")
	if len(refs) != 1 {
		return false
	}
	u, err := url.Parse(refs[0])
	return err == nil && u.User == nil && u.Scheme+"://"+u.Host == s.cfg.Web.Origin
}

func webForm(r *http.Request, fields ...string) (url.Values, *apiError) {
	if r.URL.RawQuery != "" {
		return nil, errf(400, "invalid_form", "Form values must be in the request body.")
	}
	if err := r.ParseForm(); err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			return nil, errf(413, "request_too_large", "Form body exceeds 32 KiB.")
		}
		return nil, errf(400, "invalid_form", "The form could not be read.")
	}
	allowed := make(map[string]bool)
	for _, name := range fields {
		allowed[name] = true
	}
	for name, values := range r.PostForm {
		if !allowed[name] || len(values) != 1 {
			return nil, errf(400, "invalid_form", "The form contains unexpected or repeated fields.")
		}
	}
	if len(r.PostForm["csrf"]) != 1 {
		return nil, errf(400, "invalid_form", "The form is incomplete.")
	}
	return r.PostForm, nil
}

func (s *Server) webLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.DisableRateLimits && !s.limiter.allow("web-login", "write") {
		s.webError(w, r, &apiError{Status: 429, Message: "Too many sign-in attempts. Try again shortly.", Retry: 5})
		return
	}
	f, e := webForm(r, "credential", "csrf", "next")
	if e != nil {
		s.webError(w, r, e)
		return
	}
	if !s.webOriginOK(r) {
		s.webError(w, r, errf(403, "invalid_origin", "Please submit the sign-in form from this site."))
		return
	}
	preID := s.cookieID(r, true)
	if !s.web.sessions.consumePre(preID, f.Get("csrf")) {
		s.webError(w, r, errf(403, "invalid_csrf", "The sign-in form expired. Reload it and try again."))
		return
	}
	token := f.Get("credential")
	if len(f["credential"]) != 1 || len(token) > domain.BearerMaxLen {
		s.loginPage(w, r, 401, "Credential not recognized.", f.Get("next"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), domain.FeedDBTimeout)
	defer cancel()
	p, err := s.db.Authenticate(ctx, token)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (p.ID != s.cfg.Web.PrincipalID || p.Permission != "read") {
		s.loginPage(w, r, 401, "Credential not recognized.", f.Get("next"))
		return
	}
	if err != nil {
		s.webError(w, r, &apiError{Status: 503, Message: "The board is temporarily unavailable.", Retry: 5})
		return
	}
	facts, err := s.db.LoadWebIdentity(ctx, p.ID)
	if errors.Is(err, sql.ErrNoRows) {
		s.loginPage(w, r, 401, "Credential not recognized.", f.Get("next"))
		return
	}
	if err != nil {
		s.webError(w, r, &apiError{Status: 503, Message: "The board is temporarily unavailable.", Retry: 5})
		return
	}
	id, v, err := s.web.sessions.create(false, webSession{Principal: p.ID, Instance: facts.InstanceID, Generation: facts.GenerationID})
	if err != nil {
		s.webError(w, r, &apiError{Status: 503, Message: "Sign-in capacity reached. Try again shortly.", Retry: 60})
		return
	}
	s.web.sessions.remove(false, s.cookieID(r, false))
	s.setWebCookie(w, true, "", time.Time{})
	s.setWebCookie(w, false, id, v.Expires)
	http.Redirect(w, r, safeWebReturn(f.Get("next")), http.StatusSeeOther)
}

func (s *Server) webLogout(w http.ResponseWriter, r *http.Request) {
	f, e := webForm(r, "csrf")
	if e != nil {
		s.webError(w, r, e)
		return
	}
	v := r.Context().Value(webSessionKey{}).(webSession)
	if !s.webOriginOK(r) || !csrfEqual(v.CSRF, f.Get("csrf")) {
		s.webError(w, r, errf(403, "invalid_csrf", "The logout form is invalid. Reload the page."))
		return
	}
	s.web.sessions.remove(false, s.cookieID(r, false))
	s.setWebCookie(w, false, "", time.Time{})
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

func webParams(r *http.Request, allowed ...string) (*params, *apiError) {
	v, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errf(400, "invalid_parameter", "Invalid query string.")
	}
	known := map[string]bool{}
	for _, k := range allowed {
		known[k] = true
	}
	for k, vs := range v {
		if !known[k] || len(vs) != 1 {
			return nil, errf(400, "invalid_parameter", "Unknown or repeated query parameter.")
		}
	}
	return &params{v: v}, nil
}

func nextWebLink(p string, q *params, cursor *string) string {
	if cursor == nil {
		return ""
	}
	v := url.Values{}
	for k, values := range q.v {
		v[k] = append([]string(nil), values...)
	}
	v.Set("cursor", *cursor)
	return p + "?" + v.Encode()
}

func (s *Server) webReadError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		s.webError(w, r, &apiError{Status: 503, Message: "The read took too long. Narrow the search or try again.", Retry: 5})
		return
	}
	s.webError(w, r, translate(err))
}

func (s *Server) webFeed(w http.ResponseWriter, r *http.Request, principal string) {
	q, e := webParams(r, "category", "tags", "tag_match", "scope", "sort", "limit", "preview_chars", "cursor")
	if e != nil {
		s.webError(w, r, e)
		return
	}
	f, e := q.filters()
	if e != nil {
		s.webError(w, r, e)
		return
	}
	limit, e := q.intRange("limit", domain.PageDefault, 1, domain.PageMax)
	if e != nil {
		s.webError(w, r, e)
		return
	}
	preview, e := q.intRange("preview_chars", domain.PreviewDefault, domain.PreviewMin, domain.PreviewMax)
	if e != nil {
		s.webError(w, r, e)
		return
	}
	if q.str("sort") != "" && q.str("sort") != "created" && q.str("sort") != "activity" {
		s.webError(w, r, errf(400, "invalid_parameter", "Sort must be created or activity."))
		return
	}
	if len(q.str("cursor")) > domain.CursorMaxLen {
		s.webError(w, r, errf(400, "cursor_invalid", "Invalid cursor."))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), domain.FeedDBTimeout)
	defer cancel()
	v, err := s.db.Feed(ctx, storage.FeedQuery{PrincipalID: principal, Filters: f, Activity: q.str("sort") == "activity", Limit: limit, PreviewChars: preview, Cursor: q.str("cursor")})
	if err != nil {
		s.webReadError(w, r, err)
		return
	}
	s.renderWeb(w, r, 200, webPage{Title: "Posts", View: "feed", Cards: v.Items, Next: nextWebLink("/ui/", q, v.NextCursor), NextLabel: "Older posts"})
}

func (s *Server) webSearch(w http.ResponseWriter, r *http.Request, principal string) {
	q, e := webParams(r, "q", "category", "tags", "tag_match", "scope", "sort", "limit", "preview_chars", "cursor")
	if e != nil {
		s.webError(w, r, e)
		return
	}
	if strings.TrimSpace(q.str("q")) == "" {
		s.renderWeb(w, r, 200, webPage{Title: "Search", View: "search"})
		return
	}
	terms, err := search.Parse(q.str("q"))
	if err != nil {
		s.webReadError(w, r, err)
		return
	}
	f, e := q.filters()
	if e != nil {
		s.webError(w, r, e)
		return
	}
	limit, e := q.intRange("limit", domain.PageDefault, 1, domain.PageMax)
	if e != nil {
		s.webError(w, r, e)
		return
	}
	preview, e := q.intRange("preview_chars", domain.PreviewDefault, domain.PreviewMin, domain.PreviewMax)
	if e != nil {
		s.webError(w, r, e)
		return
	}
	if q.str("sort") != "" && q.str("sort") != "recent" && q.str("sort") != "relevance" {
		s.webError(w, r, errf(400, "invalid_parameter", "Sort must be relevance or recent."))
		return
	}
	if len(q.str("cursor")) > domain.CursorMaxLen {
		s.webError(w, r, errf(400, "cursor_invalid", "Invalid cursor."))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), domain.FeedDBTimeout)
	defer cancel()
	v, err := s.db.Search(ctx, storage.SearchQuery{PrincipalID: principal, Terms: terms, Filters: f, SortRecent: q.str("sort") == "recent", Limit: limit, PreviewChars: preview, Cursor: q.str("cursor")})
	if err != nil {
		s.webReadError(w, r, err)
		return
	}
	s.renderWeb(w, r, 200, webPage{Title: "Search", View: "search", Query: q.str("q"), Hits: v.Items, Next: nextWebLink("/ui/search", q, v.NextCursor), NextLabel: "More results"})
}

func (s *Server) webDiscussion(w http.ResponseWriter, r *http.Request, principal, id string) {
	q, e := webParams(r, "comment_limit")
	if e != nil {
		s.webError(w, r, e)
		return
	}
	limit, e := q.intRange("comment_limit", domain.PageDefault, 1, domain.PageMax)
	if e != nil {
		s.webError(w, r, e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), domain.DiscussionDBTimeout)
	defer cancel()
	v, err := s.db.GetDiscussion(ctx, principal, id, limit)
	if err != nil {
		s.webReadError(w, r, err)
		return
	}
	nq := &params{v: url.Values{"limit": {strconv.Itoa(limit)}}}
	s.renderWeb(w, r, 200, webPage{Title: v.Post.Title, View: "discussion", Discussion: v, PostID: id, Next: nextWebLink("/ui/posts/"+id+"/comments", nq, v.NextCommentsCursor), NextLabel: "Next comments"})
}

// webDiscussionJSON shows the GET /v1/posts/{id} payload with every comment,
// indented for reading in a browser tab.
func (s *Server) webDiscussionJSON(w http.ResponseWriter, r *http.Request, id string) {
	if _, e := webParams(r); e != nil {
		s.webError(w, r, e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), domain.DiscussionDBTimeout)
	defer cancel()
	v, err := s.db.GetFullDiscussion(ctx, id)
	if err != nil {
		s.webReadError(w, r, err)
		return
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		s.webError(w, r, translate(err))
		return
	}
	if r.Method == http.MethodGet && !s.touchWebOpen(w, r, id, "post") {
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(b.Bytes())
	}
}

func (s *Server) webComments(w http.ResponseWriter, r *http.Request, principal, id string) {
	q, e := webParams(r, "limit", "cursor")
	if e != nil {
		s.webError(w, r, e)
		return
	}
	limit, e := q.intRange("limit", domain.PageDefault, 1, domain.PageMax)
	if e != nil {
		s.webError(w, r, e)
		return
	}
	if len(q.str("cursor")) > domain.CursorMaxLen {
		s.webError(w, r, errf(400, "cursor_invalid", "Invalid cursor."))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), domain.DiscussionDBTimeout)
	defer cancel()
	v, err := s.db.Comments(ctx, principal, id, limit, q.str("cursor"))
	if err != nil {
		s.webReadError(w, r, err)
		return
	}
	s.renderWeb(w, r, 200, webPage{Title: "Comments", View: "comments", Comments: v, PostID: id, Next: nextWebLink(r.URL.Path, q, v.NextCursor), NextLabel: "Next comments"})
}
