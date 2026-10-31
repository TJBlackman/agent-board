package api

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strconv"

	"trevorblackman.dev/agent-board/internal/domain"
	"trevorblackman.dev/agent-board/internal/storage"
)

// handleTrackedHead rejects HEAD on the two tracked routes.
func (s *Server) handleTrackedHead(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "GET")
	writeError(w, r, errf(http.StatusMethodNotAllowed, "method_not_allowed", "HEAD is not supported on tracked full-content routes; use GET"))
}

type trackedHeaders struct {
	contextID string
	eventID   string
	instance  string
}

func trackedHeadersOf(r *http.Request) (*trackedHeaders, *apiError) {
	for _, h := range []string{"If-None-Match", "If-Modified-Since", "If-Match", "If-Unmodified-Since", "Range", "If-Range"} {
		if r.Header.Get(h) != "" {
			return nil, errField(http.StatusBadRequest, "unsupported_read_condition", h, "conditional and range requests are not supported on tracked routes")
		}
	}
	th := &trackedHeaders{}
	var e *apiError
	if th.contextID, e = uuidHeader(r, "X-Execution-Context-ID"); e != nil {
		return nil, e
	}
	if th.eventID, e = uuidHeader(r, "X-Open-Event-ID"); e != nil {
		return nil, e
	}
	if th.instance, e = uuidHeader(r, "X-Board-Instance-ID"); e != nil {
		return nil, e
	}
	return th, nil
}

func fingerprint(method, target, shape, contextID string) []byte {
	h := sha256.New()
	for _, p := range []string{method, target, shape, contextID} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return h.Sum(nil)
}

// finishTracked submits the statistics transaction and writes the prepared
// response only after the commit (or verified matching receipt).
func (s *Server) finishTracked(w http.ResponseWriter, r *http.Request, th *trackedHeaders, ownerSession, workspace string, seq int64, gen, inst string, fp, body []byte) {
	p := principalOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), domain.WriteTotalTimeout)
	defer cancel()
	counted, err := s.db.CountOpen(ctx, storage.OpenRequest{PrincipalID: p.ID, OpenEventID: th.eventID, ContextID: th.contextID, SessionID: ownerSession, WorkspaceID: workspace, EntrySeq: seq, ExpectedInstance: th.instance, ExpectedGen: gen, Fingerprint: fp})
	if err != nil {
		if ctx.Err() != nil {
			writeError(w, r, translate(errContextDeadline))
			return
		}
		writeError(w, r, translate(err))
		return
	}
	if counted {
		s.metrics.add(&s.metrics.Opens, 1)
	} else {
		s.metrics.add(&s.metrics.Dedup, 1)
	}
	if inst != th.instance {
		// Defensive: the transaction already compared; never emit on mismatch.
		writeError(w, r, translate(storage.ErrInstanceChanged))
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Open-Event-ID", th.eventID)
	w.Header().Set("X-View-Counted", strconv.FormatBool(counted))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// verifyContextOwner checks the viewing context belongs to the principal and
// returns its session and workspace for daily qualification.
func (s *Server) verifyContextOwner(ctx context.Context, principalID, contextID string) (session, workspace string, e *apiError) {
	c, owner, err := storage.LoadContext(ctx, s.db.Reads(), contextID)
	if err != nil {
		if err == storage.ErrNotFound {
			return "", "", errField(http.StatusNotFound, "not_found", "X-Execution-Context-ID", "unknown execution context")
		}
		return "", "", translate(err)
	}
	if owner != principalID {
		return "", "", errf(http.StatusForbidden, "forbidden", "the execution context belongs to another principal")
	}
	return c.SessionID, c.WorkspaceID, nil
}

func (s *Server) handleGetPost(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	postID := r.PathValue("id")
	if !domain.IsUUID(postID) {
		writeError(w, r, errf(http.StatusNotFound, "not_found", "unknown post"))
		return
	}
	th, e := trackedHeadersOf(r)
	if e != nil {
		writeError(w, r, e)
		return
	}
	q, e := parseParams(r, "comment_limit")
	if e != nil {
		writeError(w, r, e)
		return
	}
	climit, e := q.intRange("comment_limit", domain.PageDefault, 1, domain.PageMax)
	if e != nil {
		writeError(w, r, e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), domain.TrackedOpenTimeout)
	defer cancel()
	session, workspace, e := s.verifyContextOwner(ctx, p.ID, th.contextID)
	if e != nil {
		writeError(w, r, e)
		return
	}
	rctx, rcancel := context.WithTimeout(ctx, domain.DiscussionDBTimeout)
	d, err := s.db.GetDiscussion(rctx, p.ID, postID, climit)
	rcancel()
	if err != nil {
		writeError(w, r, translate(err))
		return
	}
	body, err := encodeJSON(d)
	if err != nil {
		writeError(w, r, translate(err))
		return
	}
	fp := fingerprint("GET", "/v1/posts/"+postID, "comment_limit="+strconv.Itoa(climit), th.contextID)
	s.finishTracked(w, r, th, session, workspace, d.Seq, d.Generation, d.Instance, fp, body)
}

func (s *Server) handleGetComment(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	commentID := r.PathValue("id")
	if !domain.IsUUID(commentID) {
		writeError(w, r, errf(http.StatusNotFound, "not_found", "unknown comment"))
		return
	}
	th, e := trackedHeadersOf(r)
	if e != nil {
		writeError(w, r, e)
		return
	}
	if _, e := parseParams(r); e != nil {
		writeError(w, r, e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), domain.TrackedOpenTimeout)
	defer cancel()
	session, workspace, e := s.verifyContextOwner(ctx, p.ID, th.contextID)
	if e != nil {
		writeError(w, r, e)
		return
	}
	rctx, rcancel := context.WithTimeout(ctx, domain.DiscussionDBTimeout)
	c, err := s.db.GetComment(rctx, commentID)
	rcancel()
	if err != nil {
		writeError(w, r, translate(err))
		return
	}
	body, err := encodeJSON(c)
	if err != nil {
		writeError(w, r, translate(err))
		return
	}
	fp := fingerprint("GET", "/v1/comments/"+commentID, "", th.contextID)
	s.finishTracked(w, r, th, session, workspace, c.Seq, c.Generation, c.Instance, fp, body)
}
