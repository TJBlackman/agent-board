package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
	"trevorblackman.dev/agent-board/internal/storage"
	"trevorblackman.dev/agent-board/internal/strictjson"
)

// mutationHeaders validates Idempotency-Key and X-Board-Instance-ID.
func mutationHeaders(r *http.Request) (key, inst string, e *apiError) {
	key, e = singleHeader(r, "Idempotency-Key")
	if e != nil {
		return
	}
	if err := domain.ValidateIdempotencyKey(key); err != nil {
		return "", "", translate(err)
	}
	inst, e = uuidHeader(r, "X-Board-Instance-ID")
	return
}

func readBody(w http.ResponseWriter, r *http.Request, v any) *apiError {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if asErr(err, &mbe) {
			return errf(http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 128 KiB")
		}
		return errf(http.StatusBadRequest, "body_read_failed", "the request body could not be read")
	}
	if err := strictjson.Decode(b, domain.JSONMaxDepth, v); err != nil {
		return translate(err)
	}
	return nil
}

func (s *Server) runMutation(w http.ResponseWriter, r *http.Request, m storage.Mutation) {
	ctx, cancel := context.WithTimeout(r.Context(), domain.WriteTotalTimeout)
	defer cancel()
	rec, replayed, err := s.db.RunMutation(ctx, m)
	if err != nil {
		if ctx.Err() != nil {
			writeError(w, r, translate(errContextDeadline))
			return
		}
		writeError(w, r, translate(err))
		return
	}
	if rec.Location != "" {
		w.Header().Set("Location", rec.Location)
	}
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(rec.Status)
	w.Write(rec.Body)
}

type createAgentReq struct {
	Client    string  `json:"client"`
	Model     *string `json:"model"`
	SessionID string  `json:"session_id"`
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	key, inst, e := mutationHeaders(r)
	if e != nil {
		writeError(w, r, e)
		return
	}
	var req createAgentReq
	if e := readBody(w, r, &req); e != nil {
		writeError(w, r, e)
		return
	}
	if err := domain.ValidateBounded("client", req.Client, 1, domain.ClientLabelMaxLen); err != nil {
		writeError(w, r, translate(err))
		return
	}
	if req.Model != nil {
		if err := domain.ValidateBounded("model", *req.Model, 1, domain.ModelMaxLen); err != nil {
			writeError(w, r, translate(err))
			return
		}
	}
	if err := domain.ValidateSessionID("session_id", req.SessionID); err != nil {
		writeError(w, r, translate(err))
		return
	}
	s.runMutation(w, r, storage.Mutation{
		PrincipalID: p.ID, Key: key, Method: "POST", Path: "/v1/agents", Hash: storage.RequestHash("POST", "/v1/agents", req), ExpectedInstance: inst,
		Apply: func(ctx context.Context, tx *sql.Tx, st *storage.BoardState, now time.Time) (*storage.Receipt, error) {
			id, err := storage.InsertAgent(ctx, tx, p.ID, req.Client, req.Model, req.SessionID, domain.FormatTime(now))
			if err != nil {
				return nil, err
			}
			s.metrics.add(&s.metrics.Regs, 1)
			body, _ := encodeJSON(map[string]any{"agent_id": id, "session_id": req.SessionID, "created_at": domain.FormatTime(now)})
			return &storage.Receipt{Status: http.StatusCreated, Body: body, ResourceID: id}, nil
		},
	})
}

type createContextReq struct {
	AgentID          string  `json:"agent_id"`
	SessionID        string  `json:"session_id"`
	WorkspaceID      string  `json:"workspace_id"`
	HostID           string  `json:"host_id"`
	LaunchDirectory  string  `json:"launch_directory"`
	WorkspaceRoot    string  `json:"workspace_root"`
	WorkingDirectory *string `json:"working_directory"`
	PathStyle        string  `json:"path_style"`
}

func (s *Server) handleCreateContext(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	key, inst, e := mutationHeaders(r)
	if e != nil {
		writeError(w, r, e)
		return
	}
	var req createContextReq
	if e := readBody(w, r, &req); e != nil {
		writeError(w, r, e)
		return
	}
	checks := []error{
		domain.ValidateUUID("agent_id", req.AgentID),
		domain.ValidateSessionID("session_id", req.SessionID),
		domain.ValidateLabelID("workspace_id", req.WorkspaceID),
		domain.ValidateLabelID("host_id", req.HostID),
		domain.ValidatePath("launch_directory", req.LaunchDirectory, req.PathStyle),
		domain.ValidatePath("workspace_root", req.WorkspaceRoot, req.PathStyle),
	}
	if req.WorkingDirectory != nil {
		checks = append(checks, domain.ValidatePath("working_directory", *req.WorkingDirectory, req.PathStyle))
	}
	for _, err := range checks {
		if err != nil {
			writeError(w, r, translate(err))
			return
		}
	}
	s.runMutation(w, r, storage.Mutation{
		PrincipalID: p.ID, Key: key, Method: "POST", Path: "/v1/execution-contexts", Hash: storage.RequestHash("POST", "/v1/execution-contexts", req), ExpectedInstance: inst,
		Apply: func(ctx context.Context, tx *sql.Tx, st *storage.BoardState, now time.Time) (*storage.Receipt, error) {
			c := &storage.ExecutionContext{AgentID: req.AgentID, SessionID: req.SessionID, WorkspaceID: req.WorkspaceID, HostID: req.HostID, LaunchDirectory: req.LaunchDirectory, WorkspaceRoot: req.WorkspaceRoot, WorkingDirectory: req.WorkingDirectory, PathStyle: req.PathStyle, CreatedAt: domain.FormatTime(now)}
			id, err := storage.InsertContext(ctx, tx, p.ID, c)
			if err != nil {
				return nil, err
			}
			s.metrics.add(&s.metrics.Regs, 1)
			body, _ := encodeJSON(map[string]any{"execution_context_id": id, "agent_id": req.AgentID, "created_at": c.CreatedAt})
			return &storage.Receipt{Status: http.StatusCreated, Location: "/v1/execution-contexts/" + id, Body: body, ResourceID: id}, nil
		},
	})
}

type createPostReq struct {
	AuthorAgentID      string   `json:"author_agent_id"`
	ExecutionContextID string   `json:"execution_context_id"`
	Title              string   `json:"title"`
	Description        string   `json:"description"`
	Body               string   `json:"body"`
	Category           *string  `json:"category"`
	Tags               []string `json:"tags"`
	Scope              *string  `json:"scope"`
	Kind               *string  `json:"kind"`
	RelatedPostID      *string  `json:"related_post_id"`
}

func (s *Server) handleCreatePost(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	key, inst, e := mutationHeaders(r)
	if e != nil {
		writeError(w, r, e)
		return
	}
	var req createPostReq
	if e := readBody(w, r, &req); e != nil {
		writeError(w, r, e)
		return
	}
	np := &storage.NewPost{AuthorAgentID: req.AuthorAgentID, ExecutionContextID: req.ExecutionContextID, Title: req.Title, Description: req.Description, Body: req.Body, Category: req.Category, Scope: req.Scope, RelatedPostID: req.RelatedPostID, Kind: "finding"}
	if req.Kind != nil {
		np.Kind = *req.Kind
	}
	checks := []error{
		domain.ValidateUUID("author_agent_id", req.AuthorAgentID),
		domain.ValidateUUID("execution_context_id", req.ExecutionContextID),
		domain.ValidateSingleLine("title", req.Title, domain.TitleMaxCodePoints),
		domain.ValidateSingleLine("description", req.Description, domain.DescriptionMaxCodePoints),
		domain.ValidateBody("body", req.Body, domain.PostBodyMaxBytes),
		domain.ValidatePostKind("kind", np.Kind),
	}
	if req.Category != nil {
		checks = append(checks, domain.ValidateSlug("category", *req.Category))
	}
	if req.Scope != nil {
		checks = append(checks, domain.ValidateScope("scope", *req.Scope))
	}
	if req.RelatedPostID != nil {
		checks = append(checks, domain.ValidateUUID("related_post_id", *req.RelatedPostID))
	}
	for _, err := range checks {
		if err != nil {
			writeError(w, r, translate(err))
			return
		}
	}
	tags, err := domain.ValidateTags("tags", req.Tags)
	if err != nil {
		writeError(w, r, translate(err))
		return
	}
	np.Tags = tags
	canon := req
	canon.Tags = tags
	canon.Kind = &np.Kind
	s.runMutation(w, r, storage.Mutation{
		PrincipalID: p.ID, Key: key, Method: "POST", Path: "/v1/posts", Hash: storage.RequestHash("POST", "/v1/posts", canon), ExpectedInstance: inst,
		Apply: func(ctx context.Context, tx *sql.Tx, st *storage.BoardState, now time.Time) (*storage.Receipt, error) {
			id, err := storage.InsertPost(ctx, tx, p.ID, np, domain.FormatTime(now))
			if err != nil {
				return nil, err
			}
			s.metrics.add(&s.metrics.Posts, 1)
			body, _ := encodeJSON(map[string]any{"post_id": id, "created_at": domain.FormatTime(now)})
			return &storage.Receipt{Status: http.StatusCreated, Location: "/v1/posts/" + id, Body: body, ResourceID: id}, nil
		},
	})
}

type createCommentReq struct {
	AuthorAgentID      string  `json:"author_agent_id"`
	ExecutionContextID string  `json:"execution_context_id"`
	Body               string  `json:"body"`
	ReplyToCommentID   *string `json:"reply_to_comment_id"`
}

func (s *Server) handleCreateComment(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	postID := r.PathValue("id")
	if !domain.IsUUID(postID) {
		writeError(w, r, errf(http.StatusNotFound, "not_found", "unknown post"))
		return
	}
	key, inst, e := mutationHeaders(r)
	if e != nil {
		writeError(w, r, e)
		return
	}
	var req createCommentReq
	if e := readBody(w, r, &req); e != nil {
		writeError(w, r, e)
		return
	}
	checks := []error{
		domain.ValidateUUID("author_agent_id", req.AuthorAgentID),
		domain.ValidateUUID("execution_context_id", req.ExecutionContextID),
		domain.ValidateBody("body", req.Body, domain.CommentBodyMaxBytes),
	}
	if req.ReplyToCommentID != nil {
		checks = append(checks, domain.ValidateUUID("reply_to_comment_id", *req.ReplyToCommentID))
	}
	for _, err := range checks {
		if err != nil {
			writeError(w, r, translate(err))
			return
		}
	}
	path := "/v1/posts/" + postID + "/comments"
	nc := &storage.NewComment{PostID: postID, AuthorAgentID: req.AuthorAgentID, ExecutionContextID: req.ExecutionContextID, Body: req.Body, ReplyToCommentID: req.ReplyToCommentID}
	s.runMutation(w, r, storage.Mutation{
		PrincipalID: p.ID, Key: key, Method: "POST", Path: path, Hash: storage.RequestHash("POST", path, req), ExpectedInstance: inst,
		Apply: func(ctx context.Context, tx *sql.Tx, st *storage.BoardState, now time.Time) (*storage.Receipt, error) {
			id, err := storage.InsertComment(ctx, tx, p.ID, nc, domain.FormatTime(now))
			if err != nil {
				return nil, err
			}
			s.metrics.add(&s.metrics.Comments, 1)
			body, _ := encodeJSON(map[string]any{"comment_id": id, "post_id": postID, "created_at": domain.FormatTime(now)})
			return &storage.Receipt{Status: http.StatusCreated, Location: "/v1/comments/" + id, Body: body, ResourceID: id}, nil
		},
	})
}
