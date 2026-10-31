package storage

import (
	"context"
	"database/sql"
	"errors"

	"trevorblackman.dev/agent-board/internal/domain"
)

// Agent is a registered agent instance.
type Agent struct {
	ID          string
	PrincipalID string
	Client      string
	Model       *string
	SessionID   string
	CreatedAt   string
}

// ExecutionContext is an immutable launch/action context.
type ExecutionContext struct {
	ID               string  `json:"execution_context_id"`
	AgentID          string  `json:"agent_id"`
	SessionID        string  `json:"session_id"`
	WorkspaceID      string  `json:"workspace_id"`
	HostID           string  `json:"host_id"`
	LaunchDirectory  string  `json:"launch_directory"`
	WorkspaceRoot    string  `json:"workspace_root"`
	WorkingDirectory *string `json:"working_directory"`
	PathStyle        string  `json:"path_style"`
	CreatedAt        string  `json:"created_at"`
}

// Sentinel errors mapped to HTTP codes by the API layer.
var (
	ErrNotFound        = errors.New("not_found")
	ErrForbidden       = errors.New("forbidden")
	ErrOriginConflict  = errors.New("context_origin_conflict")
	ErrSessionMismatch = errors.New("session_mismatch")
)

// InsertAgent creates the agent row inside tx. The ID is generated with a
// bounded collision retry.
func InsertAgent(ctx context.Context, tx *sql.Tx, principalID, client string, model *string, sessionID, createdAt string) (string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		id, err := domain.NewUUID()
		if err != nil {
			return "", err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO agents(id, principal_id, client, model, session_id, created_at) VALUES (?,?,?,?,?,?)`,
			id, principalID, client, model, sessionID, createdAt)
		if err == nil {
			return id, nil
		}
		if !IsUniqueViolation(err) {
			return "", err
		}
	}
	return "", errors.New("uuid collision retry exhausted")
}

// LoadAgent returns the agent row or ErrNotFound.
func LoadAgent(ctx context.Context, q Querier, id string) (*Agent, error) {
	a := &Agent{}
	var model sql.NullString
	err := q.QueryRowContext(ctx, `SELECT id, principal_id, client, model, session_id, created_at FROM agents WHERE id=?`, id).
		Scan(&a.ID, &a.PrincipalID, &a.Client, &model, &a.SessionID, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if model.Valid {
		a.Model = &model.String
	}
	return a, nil
}

// InsertContext registers an immutable context for an owned agent, enforcing
// session equality and the immutable origin binding stored on the agent row.
// The agent row update serializes concurrent first registrations because the
// whole thing runs on the single writer connection.
func InsertContext(ctx context.Context, tx *sql.Tx, principalID string, c *ExecutionContext) (string, error) {
	var owner, session string
	var ows, oh, old, owr, ops sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT principal_id, session_id, origin_workspace_id, origin_host_id, origin_launch_directory, origin_workspace_root, origin_path_style FROM agents WHERE id=?`, c.AgentID).
		Scan(&owner, &session, &ows, &oh, &old, &owr, &ops)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if owner != principalID {
		return "", ErrForbidden
	}
	if session != c.SessionID {
		return "", ErrSessionMismatch
	}
	if ows.Valid {
		if ows.String != c.WorkspaceID || oh.String != c.HostID || old.String != c.LaunchDirectory || owr.String != c.WorkspaceRoot || ops.String != c.PathStyle {
			return "", ErrOriginConflict
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET origin_workspace_id=?, origin_host_id=?, origin_launch_directory=?, origin_workspace_root=?, origin_path_style=? WHERE id=? AND origin_workspace_id IS NULL`,
			c.WorkspaceID, c.HostID, c.LaunchDirectory, c.WorkspaceRoot, c.PathStyle, c.AgentID); err != nil {
			return "", err
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		id, err := domain.NewUUID()
		if err != nil {
			return "", err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO execution_contexts(id, agent_id, session_id, workspace_id, host_id, launch_directory, workspace_root, working_directory, path_style, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			id, c.AgentID, c.SessionID, c.WorkspaceID, c.HostID, c.LaunchDirectory, c.WorkspaceRoot, c.WorkingDirectory, c.PathStyle, c.CreatedAt)
		if err == nil {
			return id, nil
		}
		if !IsUniqueViolation(err) {
			return "", err
		}
	}
	return "", errors.New("uuid collision retry exhausted")
}

// LoadContext returns a context with its owning principal.
func LoadContext(ctx context.Context, q Querier, id string) (*ExecutionContext, string, error) {
	c := &ExecutionContext{}
	var wd sql.NullString
	var principal string
	err := q.QueryRowContext(ctx, `SELECT c.id, c.agent_id, c.session_id, c.workspace_id, c.host_id, c.launch_directory, c.workspace_root, c.working_directory, c.path_style, c.created_at, a.principal_id
		FROM execution_contexts c JOIN agents a ON a.id = c.agent_id WHERE c.id=?`, id).
		Scan(&c.ID, &c.AgentID, &c.SessionID, &c.WorkspaceID, &c.HostID, &c.LaunchDirectory, &c.WorkspaceRoot, &wd, &c.PathStyle, &c.CreatedAt, &principal)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	if wd.Valid {
		c.WorkingDirectory = &wd.String
	}
	return c, principal, nil
}

// VerifyAuthorContext checks that the context belongs to the author and the
// author belongs to the principal. Returns ErrNotFound or ErrForbidden.
func VerifyAuthorContext(ctx context.Context, q Querier, principalID, agentID, contextID string) error {
	a, err := LoadAgent(ctx, q, agentID)
	if err != nil {
		return err
	}
	if a.PrincipalID != principalID {
		return ErrForbidden
	}
	c, owner, err := LoadContext(ctx, q, contextID)
	if err != nil {
		return err
	}
	if owner != principalID || c.AgentID != agentID {
		return ErrForbidden
	}
	return nil
}
