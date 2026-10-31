package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"trevorblackman.dev/agent-board/internal/domain"
	"trevorblackman.dev/agent-board/internal/search"
	"trevorblackman.dev/agent-board/internal/storage"
)

func asErr(err error, target any) bool { return errors.As(err, target) }

// rejectTrackedHeaders enforces 400 untracked_route_headers on untracked routes.
func rejectTrackedHeaders(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Execution-Context-ID") != "" || r.Header.Get("X-Open-Event-ID") != "" {
		writeError(w, r, errf(http.StatusBadRequest, "untracked_route_headers", "X-Execution-Context-ID and X-Open-Event-ID are accepted only on GET /v1/posts/{id} and GET /v1/comments/{id}; this request counted nothing"))
		return true
	}
	return false
}

func etagFor(b []byte) string {
	h := sha256.Sum256(b)
	return fmt.Sprintf(`"%x"`, h[:12])
}

func (s *Server) handleTaxonomy(w http.ResponseWriter, r *http.Request) {
	if rejectTrackedHeaders(w, r) {
		return
	}
	if _, e := parseParams(r); e != nil {
		writeError(w, r, e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), domain.FeedDBTimeout)
	defer cancel()
	t, err := s.db.LoadTaxonomy(ctx)
	if err != nil {
		writeError(w, r, translate(err))
		return
	}
	b, err := encodeJSON(t)
	if err != nil {
		writeError(w, r, translate(err))
		return
	}
	tag := etagFor(b)
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	if rejectTrackedHeaders(w, r) {
		return
	}
	p := principalOf(r)
	q, e := parseParams(r, "category", "tags", "tag_match", "scope", "sort", "limit", "preview_chars", "cursor")
	if e != nil {
		writeError(w, r, e)
		return
	}
	f, e := q.filters()
	if e != nil {
		writeError(w, r, e)
		return
	}
	limit, e := q.intRange("limit", domain.PageDefault, 1, domain.PageMax)
	if e != nil {
		writeError(w, r, e)
		return
	}
	preview, e := q.intRange("preview_chars", domain.PreviewDefault, domain.PreviewMin, domain.PreviewMax)
	if e != nil {
		writeError(w, r, e)
		return
	}
	activity := false
	switch q.str("sort") {
	case "", "created":
	case "activity":
		activity = true
	default:
		writeError(w, r, errField(http.StatusBadRequest, "invalid_parameter", "sort", "sort must be created or activity"))
		return
	}
	if q.has("cursor") && len(q.str("cursor")) > domain.CursorMaxLen {
		writeError(w, r, errField(http.StatusBadRequest, "cursor_invalid", "cursor", "cursor exceeds 2048 bytes"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), domain.FeedDBTimeout)
	defer cancel()
	page, err := s.db.Feed(ctx, storage.FeedQuery{PrincipalID: p.ID, Filters: f, Activity: activity, Limit: limit, PreviewChars: preview, Cursor: q.str("cursor")})
	if err != nil {
		writeError(w, r, translate(err))
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if rejectTrackedHeaders(w, r) {
		return
	}
	p := principalOf(r)
	q, e := parseParams(r, "q", "category", "tags", "tag_match", "scope", "sort", "limit", "preview_chars", "cursor")
	if e != nil {
		writeError(w, r, e)
		return
	}
	if !q.has("q") {
		writeError(w, r, errField(http.StatusBadRequest, "invalid_query", "q", "q is required"))
		return
	}
	terms, err := search.Parse(q.str("q"))
	if err != nil {
		writeError(w, r, translate(err))
		return
	}
	f, e := q.filters()
	if e != nil {
		writeError(w, r, e)
		return
	}
	limit, e := q.intRange("limit", domain.PageDefault, 1, domain.PageMax)
	if e != nil {
		writeError(w, r, e)
		return
	}
	preview, e := q.intRange("preview_chars", domain.PreviewDefault, domain.PreviewMin, domain.PreviewMax)
	if e != nil {
		writeError(w, r, e)
		return
	}
	recent := false
	switch q.str("sort") {
	case "", "relevance":
	case "recent":
		recent = true
	default:
		writeError(w, r, errField(http.StatusBadRequest, "invalid_parameter", "sort", "sort must be relevance or recent"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), domain.FeedDBTimeout)
	defer cancel()
	page, err := s.db.Search(ctx, storage.SearchQuery{PrincipalID: p.ID, Terms: terms, Filters: f, SortRecent: recent, Limit: limit, PreviewChars: preview, Cursor: q.str("cursor")})
	if err != nil {
		if ctx.Err() != nil {
			writeError(w, r, &apiError{Status: http.StatusServiceUnavailable, Code: "search_timeout", Message: "the search exceeded its execution budget; narrow the query", Retry: 5})
			return
		}
		writeError(w, r, translate(err))
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleComments(w http.ResponseWriter, r *http.Request) {
	if rejectTrackedHeaders(w, r) {
		return
	}
	p := principalOf(r)
	postID := r.PathValue("id")
	if !domain.IsUUID(postID) {
		writeError(w, r, errf(http.StatusNotFound, "not_found", "unknown post"))
		return
	}
	q, e := parseParams(r, "limit", "cursor")
	if e != nil {
		writeError(w, r, e)
		return
	}
	limit, e := q.intRange("limit", domain.PageDefault, 1, domain.PageMax)
	if e != nil {
		writeError(w, r, e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), domain.DiscussionDBTimeout)
	defer cancel()
	page, err := s.db.Comments(ctx, p.ID, postID, limit, q.str("cursor"))
	if err != nil {
		writeError(w, r, translate(err))
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleGetContext(w http.ResponseWriter, r *http.Request) {
	if rejectTrackedHeaders(w, r) {
		return
	}
	id := r.PathValue("id")
	if !domain.IsUUID(id) {
		writeError(w, r, errf(http.StatusNotFound, "not_found", "unknown execution context"))
		return
	}
	if _, e := parseParams(r); e != nil {
		writeError(w, r, e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), domain.FeedDBTimeout)
	defer cancel()
	c, _, err := storage.LoadContext(ctx, s.db.Reads(), id)
	if err != nil {
		writeError(w, r, translate(err))
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handlePostStats(w http.ResponseWriter, r *http.Request) { s.handleStats(w, r, "post") }
func (s *Server) handleCommentStats(w http.ResponseWriter, r *http.Request) {
	s.handleStats(w, r, "comment")
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, kind string) {
	if rejectTrackedHeaders(w, r) {
		return
	}
	id := r.PathValue("id")
	if !domain.IsUUID(id) {
		writeError(w, r, errf(http.StatusNotFound, "not_found", "unknown "+kind))
		return
	}
	if _, e := parseParams(r); e != nil {
		writeError(w, r, e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), domain.FeedDBTimeout)
	defer cancel()
	st, err := s.db.LoadStats(ctx, id, kind)
	if err != nil {
		writeError(w, r, translate(err))
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, st)
}
