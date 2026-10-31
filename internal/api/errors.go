package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"trevorblackman.dev/agent-board/internal/domain"
	"trevorblackman.dev/agent-board/internal/pagination"
	"trevorblackman.dev/agent-board/internal/storage"
	"trevorblackman.dev/agent-board/internal/strictjson"
)

// apiError is the JSON error envelope.
type apiError struct {
	Status  int
	Code    string
	Message string
	Field   string
	Retry   int // seconds for Retry-After, 0 when absent
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func errf(status int, code, msg string) *apiError {
	return &apiError{Status: status, Code: code, Message: msg}
}

func errField(status int, code, field, msg string) *apiError {
	return &apiError{Status: status, Code: code, Field: field, Message: msg}
}

// encodeJSON produces compact JSON without HTML escaping.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	return b, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := encodeJSON(v)
	if err != nil {
		http.Error(w, `{"error":{"code":"encoding_failed","message":"response encoding failed"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	w.Write(b)
}

func writeError(w http.ResponseWriter, r *http.Request, e *apiError) {
	if e.Retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.Retry))
	}
	if e.Status == http.StatusMethodNotAllowed && w.Header().Get("Allow") == "" {
		w.Header().Set("Allow", "GET")
	}
	body := map[string]any{"error": map[string]any{
		"code":       e.Code,
		"message":    e.Message,
		"request_id": requestID(r),
	}}
	if e.Field != "" {
		body["error"].(map[string]any)["field"] = e.Field
	}
	writeJSON(w, e.Status, body)
}

// translate maps internal errors to API errors.
func translate(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	if v, ok := domain.AsValidation(err); ok {
		return errField(http.StatusUnprocessableEntity, v.Code, v.Field, v.Message)
	}
	var se *strictjson.Error
	if errors.As(err, &se) {
		return errf(http.StatusBadRequest, "malformed_json", se.Msg)
	}
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return errf(http.StatusNotFound, "not_found", "the referenced resource does not exist: "+err.Error())
	case errors.Is(err, storage.ErrForbidden):
		return errf(http.StatusForbidden, "forbidden", "the agent or execution context belongs to another principal or author")
	case errors.Is(err, storage.ErrOriginConflict):
		return errf(http.StatusConflict, "context_origin_conflict", "this agent already has a different launch origin; register a new agent for a new launch")
	case errors.Is(err, storage.ErrSessionMismatch):
		return errField(http.StatusUnprocessableEntity, "session_mismatch", "session_id", "session_id must equal the agent's registered session_id")
	case errors.Is(err, storage.ErrIdempotencyConflict):
		return errf(http.StatusConflict, "idempotency_conflict", "Idempotency-Key was already used with a different request")
	case errors.Is(err, storage.ErrInstanceChanged):
		return errf(http.StatusConflict, "board_instance_changed", "X-Board-Instance-ID does not match this board; refresh the root guide and start a new deliberate operation")
	case errors.Is(err, storage.ErrGenerationChanged):
		return errf(http.StatusGone, "board_generation_changed", "the board generation changed during the request; retry as a new operation")
	case errors.Is(err, storage.ErrOpenEventConflict):
		return errf(http.StatusConflict, "open_event_conflict", "X-Open-Event-ID was already used for a different item, context, or query shape")
	case errors.Is(err, storage.ErrClockUnreliable):
		return &apiError{Status: http.StatusServiceUnavailable, Code: "clock_unreliable", Message: "server time regressed below the persisted watermark; counting is paused", Retry: 60}
	case errors.Is(err, storage.ErrCounterOverflow):
		return errf(http.StatusServiceUnavailable, "counter_overflow", "a view counter would overflow")
	case errors.Is(err, storage.ErrTaxonomyCapacity):
		return errf(http.StatusConflict, "taxonomy_capacity", "the configured category or tag capacity is reached; reuse existing labels")
	case errors.Is(err, storage.ErrCrossThreadReply):
		return errField(http.StatusUnprocessableEntity, "reply_not_in_thread", "reply_to_comment_id", "reply_to_comment_id must reference a comment in the same discussion")
	case errors.Is(err, storage.ErrSearchChanged):
		return errf(http.StatusConflict, "search_changed", "the board changed since this search began; restart the search")
	case errors.Is(err, storage.ErrQueueFull):
		return &apiError{Status: http.StatusTooManyRequests, Code: "write_queue_full", Message: "the write queue is at capacity", Retry: 2}
	case errors.Is(err, storage.ErrWriterClosed):
		return &apiError{Status: http.StatusServiceUnavailable, Code: "shutting_down", Message: "the service is shutting down", Retry: 5}
	case errors.Is(err, pagination.ErrExpired):
		return errf(http.StatusGone, "cursor_expired", "the cursor expired; start a new traversal")
	case errors.Is(err, pagination.ErrGen):
		return errf(http.StatusGone, "cursor_generation", "the board generation changed; start a new traversal")
	case errors.Is(err, pagination.ErrMismatch):
		return errf(http.StatusBadRequest, "cursor_mismatch", "the cursor does not match these query parameters or this credential")
	case errors.Is(err, pagination.ErrSignature), errors.Is(err, pagination.ErrMalformed):
		return errf(http.StatusBadRequest, "cursor_invalid", "the cursor is malformed or its signature is invalid")
	case errors.Is(err, errContextDeadline):
		return &apiError{Status: http.StatusServiceUnavailable, Code: "timeout", Message: "the request exceeded its execution budget", Retry: 5}
	case storage.IsBusy(err):
		return &apiError{Status: http.StatusServiceUnavailable, Code: "storage_busy", Message: "storage is busy", Retry: 1}
	}
	return errf(http.StatusInternalServerError, "internal_error", "an internal error occurred")
}

var errContextDeadline = errors.New("deadline")
