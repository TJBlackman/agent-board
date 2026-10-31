package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
)

// Env bundles everything resolved from flags, environment, and files.
type Env struct {
	Cfg     *Config
	API     *API
	Session *Session
}

// InitOptions are the identity flags accepted by init and implied by others.
type InitOptions struct {
	SessionFlag   string
	LaunchDir     string
	Workspace     string
	WorkspaceRoot string
	Client        string
	Model         string
}

var instanceRe = regexp.MustCompile(`(?m)^board_instance_id:\s*([0-9a-f-]{36})`)

// Connect resolves config, token, session, and the board instance ID. When
// needIdentity is true it also guarantees an agent and a launch context.
func Connect(opt InitOptions, needIdentity bool) (*Env, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	base, err := cfg.BaseURL()
	if err != nil {
		return nil, err
	}
	tok, err := Token()
	if err != nil {
		return nil, err
	}
	api := NewAPI(base, tok)
	sid, err := SessionID(opt.SessionFlag)
	if err != nil {
		if !needIdentity {
			// Discovery works without a session.
			return &Env{Cfg: cfg, API: api, Session: &Session{Contexts: map[string]string{}}}, nil
		}
		return nil, err
	}
	sess, err := LoadSession(sid)
	if err != nil {
		return nil, err
	}
	if sess.URL != "" && sess.URL != base {
		// A different board for the same session: start fresh identity.
		sess = &Session{SessionID: sid, Contexts: map[string]string{}}
	}
	sess.URL = base
	env := &Env{Cfg: cfg, API: api, Session: sess}
	if sess.BoardInstanceID == "" {
		if err := env.refreshInstance(); err != nil {
			return nil, err
		}
	}
	if needIdentity {
		if err := env.ensureAgent(opt); err != nil {
			return nil, err
		}
		if _, err := env.ensureContext(); err != nil {
			return nil, err
		}
	}
	return env, nil
}

func (e *Env) refreshInstance() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := e.API.Do(ctx, "GET", "/", nil, nil, true)
	if err != nil {
		return fmt.Errorf("board unreachable: %w", err)
	}
	if r.Status != 200 {
		return fmt.Errorf("root guide returned HTTP %d", r.Status)
	}
	m := instanceRe.FindSubmatch(r.Body)
	if m == nil {
		return errors.New("root guide did not contain board_instance_id")
	}
	inst := string(m[1])
	if e.Session.BoardInstanceID != "" && e.Session.BoardInstanceID != inst {
		// Replacement board: identity must be re-established.
		e.Session.AgentID = ""
		e.Session.Contexts = map[string]string{}
	}
	e.Session.BoardInstanceID = inst
	return e.Session.Save()
}

func (e *Env) ensureAgent(opt InitOptions) error {
	s := e.Session
	if s.LaunchDirectory == "" {
		ld := opt.LaunchDir
		if ld == "" {
			ld = os.Getenv("AGENT_BOARD_LAUNCH_DIRECTORY")
		}
		if ld == "" {
			return errors.New("launch directory unknown for this session: run `agent-board init --launch-dir <the directory the agent was started in>` once, or set AGENT_BOARD_LAUNCH_DIRECTORY")
		}
		ld = cleanAbs(ld)
		style := domain.DetectPathStyle(ld)
		if err := domain.ValidatePath("launch_directory", ld, style); err != nil {
			return err
		}
		s.LaunchDirectory, s.PathStyle = ld, style
		wr := opt.WorkspaceRoot
		if wr == "" {
			wr = os.Getenv("AGENT_BOARD_WORKSPACE_ROOT")
		}
		if wr == "" {
			wr = ld
		}
		wr = cleanAbs(wr)
		if err := domain.ValidatePath("workspace_root", wr, style); err != nil {
			return err
		}
		s.WorkspaceRoot = wr
		ws := opt.Workspace
		if ws == "" {
			ws = os.Getenv("AGENT_BOARD_WORKSPACE_ID")
		}
		if ws == "" {
			ws = e.Cfg.Workspaces[ld]
		}
		if ws == "" {
			ws = e.Cfg.Workspaces[wr]
		}
		if ws == "" {
			ws = SafeLabel(filepath.Base(strings.ReplaceAll(wr, "\\", "/")))
		}
		if err := domain.ValidateLabelID("workspace_id", ws); err != nil {
			return err
		}
		s.WorkspaceID = ws
		s.HostID = e.Cfg.ResolveHostID()
		s.Client = ClientLabel(opt.Client)
		if opt.Model != "" {
			m := opt.Model
			s.Model = &m
		} else if m := os.Getenv("AGENT_BOARD_MODEL"); m != "" {
			s.Model = &m
		}
		if err := s.Save(); err != nil {
			return err
		}
	}
	if s.AgentID != "" {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"client": s.Client, "model": s.Model, "session_id": s.SessionID})
	p := &Pending{Key: NewKey(), Kind: "agent", Method: "POST", Path: "/v1/agents", Body: body, BoardInstanceID: s.BoardInstanceID, SessionID: s.SessionID, CreatedAt: domain.FormatTime(time.Now())}
	r, err := e.mutate(p)
	if err != nil {
		return fmt.Errorf("agent registration: %w", err)
	}
	var rec struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(r.Body, &rec); err != nil || rec.AgentID == "" {
		return errors.New("agent registration returned no agent_id")
	}
	s.AgentID = rec.AgentID
	return s.Save()
}

// cleanAbs normalizes a path spelling without touching the filesystem.
func cleanAbs(p string) string {
	p = strings.TrimSpace(p)
	if domain.DetectPathStyle(p) == "windows" {
		p = strings.ReplaceAll(p, "/", "\\")
		p = strings.TrimRight(p, "\\")
		if len(p) == 2 && p[1] == ':' {
			p += "\\"
		}
		if len(p) >= 2 && p[1] == ':' {
			p = strings.ToUpper(p[:1]) + p[1:]
		}
		return p
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	return p
}

// workingDir returns the current directory in the session's path style, or
// empty when it cannot be determined.
func (e *Env) workingDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	wd = cleanAbs(wd)
	if domain.DetectPathStyle(wd) != e.Session.PathStyle {
		// Different OS view (for example WSL paths for a Windows launch): omit.
		return ""
	}
	if domain.ValidatePath("working_directory", wd, e.Session.PathStyle) != nil {
		return ""
	}
	return wd
}

// ensureContext returns the context ID for the current working directory,
// registering it when needed.
func (e *Env) ensureContext() (string, error) {
	s := e.Session
	wd := e.workingDir()
	if id, ok := s.Contexts[wd]; ok {
		return id, nil
	}
	req := map[string]any{
		"agent_id": s.AgentID, "session_id": s.SessionID, "workspace_id": s.WorkspaceID, "host_id": s.HostID,
		"launch_directory": s.LaunchDirectory, "workspace_root": s.WorkspaceRoot, "path_style": s.PathStyle,
	}
	if wd != "" {
		req["working_directory"] = wd
	} else {
		req["working_directory"] = nil
	}
	body, _ := json.Marshal(req)
	p := &Pending{Key: NewKey(), Kind: "context", Method: "POST", Path: "/v1/execution-contexts", Body: body, BoardInstanceID: s.BoardInstanceID, SessionID: s.SessionID, CreatedAt: domain.FormatTime(time.Now())}
	r, err := e.mutate(p)
	if err != nil {
		return "", fmt.Errorf("context registration: %w", err)
	}
	var rec struct {
		ID string `json:"execution_context_id"`
	}
	if err := json.Unmarshal(r.Body, &rec); err != nil || rec.ID == "" {
		return "", errors.New("context registration returned no execution_context_id")
	}
	s.Contexts[wd] = rec.ID
	return rec.ID, s.Save()
}

// mutate sends a pending mutation with retry semantics. Non retryable 4xx
// responses remove the pending record and return the error.
func (e *Env) mutate(p *Pending) (*Response, error) {
	headers := map[string]string{"Idempotency-Key": p.Key, "X-Board-Instance-ID": p.BoardInstanceID}
	if err := p.Save(); err != nil {
		return nil, err
	}
	r, err := e.API.DoWithRetry(p.Method, p.Path, headers, p.Body, p)
	if err != nil {
		if r != nil && r.ErrCode == "board_instance_changed" {
			return nil, err
		}
		return nil, fmt.Errorf("%w; the request and its key are kept, retry with: agent-board retry %s", err, p.Key)
	}
	if r.Status >= 400 {
		p.Remove()
		return nil, r.Err()
	}
	p.Remove()
	return r, nil
}
