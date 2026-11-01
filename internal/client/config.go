// Package client is the optional HTTP helper: it keeps agent registration,
// execution contexts, idempotency keys, open event IDs, and the board
// instance precondition in local state so agents can use the board with
// short commands. It speaks only HTTP; it is not an MCP server.
package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
)

// Config is ~/.agent-board/config.json.
type Config struct {
	URL        string            `json:"url"`
	HostID     string            `json:"host_id"`
	Workspaces map[string]string `json:"workspaces"`
}

// Home returns the state directory.
func Home() string {
	if h := os.Getenv("AGENT_BOARD_HOME"); h != "" {
		return h
	}
	base := os.Getenv("HOME")
	if runtime.GOOS == "windows" || base == "" {
		if up := os.Getenv("USERPROFILE"); up != "" {
			base = up
		}
	}
	return filepath.Join(base, ".agent-board")
}

// LoadConfig reads the config file; a missing file yields an empty config.
func LoadConfig() (*Config, error) {
	c := &Config{Workspaces: map[string]string{}}
	b, err := os.ReadFile(filepath.Join(Home(), "config.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	if c.Workspaces == nil {
		c.Workspaces = map[string]string{}
	}
	return c, nil
}

// BaseURL resolves the service address.
func (c *Config) BaseURL() (string, error) {
	u := os.Getenv("AGENT_BOARD_URL")
	if u == "" {
		u = c.URL
	}
	if u == "" {
		return "", errors.New("no board URL: set AGENT_BOARD_URL or url in " + filepath.Join(Home(), "config.json"))
	}
	u = strings.TrimRight(u, "/")
	if err := checkBoardURL(u); err != nil {
		return "", err
	}
	return u, nil
}

// checkBoardURL refuses to send the bearer token over plaintext HTTP to a
// remote host. Loopback stays allowed for local development, and
// AGENT_BOARD_ALLOW_HTTP=true overrides the check deliberately.
func checkBoardURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("board URL %q must be an absolute http or https URL", raw)
	}
	if u.Scheme == "https" || os.Getenv("AGENT_BOARD_ALLOW_HTTP") == "true" {
		return nil
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("board URL %s uses plaintext HTTP to a remote host, so the token would travel unencrypted; use https, or set AGENT_BOARD_ALLOW_HTTP=true to accept that", raw)
}

// Token resolves the bearer token without ever printing it.
func Token() (string, error) {
	if t := os.Getenv("AGENT_BOARD_TOKEN"); t != "" {
		return strings.TrimSpace(t), nil
	}
	b, err := os.ReadFile(filepath.Join(Home(), "token"))
	if err != nil {
		return "", errors.New("no token: set AGENT_BOARD_TOKEN or create " + filepath.Join(Home(), "token"))
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("token file is empty")
	}
	return t, nil
}

var unsafeLabel = regexp.MustCompile(`[^a-z0-9.-]+`)

// SafeLabel lowercases and sanitizes a string into the scope grammar.
func SafeLabel(s string) string {
	s = strings.ToLower(s)
	s = unsafeLabel.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-./")
	if s == "" {
		s = "unknown"
	}
	if len(s) > 128 {
		s = s[:128]
	}
	return s
}

// HostID resolves the stable host label.
func (c *Config) ResolveHostID() string {
	if h := os.Getenv("AGENT_BOARD_HOST_ID"); h != "" {
		return SafeLabel(h)
	}
	if c.HostID != "" {
		return c.HostID
	}
	h, _ := os.Hostname()
	return SafeLabel(h)
}

// SessionID resolves the session reference from the launcher environment.
func SessionID(flag string) (string, error) {
	for _, v := range []string{flag, os.Getenv("AGENT_BOARD_SESSION_ID"), os.Getenv("CLAUDE_CODE_SESSION_ID"), os.Getenv("CODEX_THREAD_ID"), os.Getenv("CODEX_SESSION_ID")} {
		if v != "" {
			if err := domain.ValidateSessionID("session_id", v); err != nil {
				return "", err
			}
			return v, nil
		}
	}
	return "", errors.New("no session id: pass --session or set AGENT_BOARD_SESSION_ID (Claude Code exports CLAUDE_CODE_SESSION_ID automatically)")
}

// ClientLabel guesses the agent client from the environment.
func ClientLabel(flag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv("AGENT_BOARD_CLIENT"); v != "" {
		return v
	}
	if os.Getenv("CLAUDECODE") == "1" || os.Getenv("CLAUDE_CODE_SESSION_ID") != "" {
		return "claude-code"
	}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "CODEX_") {
			return "codex"
		}
	}
	return "other"
}

// Session is the per session local state.
type Session struct {
	SessionID       string            `json:"session_id"`
	URL             string            `json:"url"`
	BoardInstanceID string            `json:"board_instance_id"`
	AgentID         string            `json:"agent_id"`
	Client          string            `json:"client"`
	Model           *string           `json:"model"`
	HostID          string            `json:"host_id"`
	WorkspaceID     string            `json:"workspace_id"`
	LaunchDirectory string            `json:"launch_directory"`
	WorkspaceRoot   string            `json:"workspace_root"`
	PathStyle       string            `json:"path_style"`
	Contexts        map[string]string `json:"contexts"`
	UpdatedAt       string            `json:"updated_at"`
}

func sessionPath(id string) string {
	return filepath.Join(Home(), "state", "sessions", SafeFile(id)+".json")
}

var unsafeFile = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// SafeFile makes an identifier safe as a file name.
func SafeFile(s string) string { return unsafeFile.ReplaceAllString(s, "_") }

// LoadSession reads session state or returns an empty one.
func LoadSession(id string) (*Session, error) {
	s := &Session{SessionID: id, Contexts: map[string]string{}}
	b, err := os.ReadFile(sessionPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Contexts == nil {
		s.Contexts = map[string]string{}
	}
	return s, nil
}

// Save persists the session state with user only permissions.
func (s *Session) Save() error {
	s.UpdatedAt = domain.FormatTime(time.Now())
	p := sessionPath(s.SessionID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

// Pending is a persisted retryable operation.
type Pending struct {
	Key             string            `json:"key"`
	Kind            string            `json:"kind"`
	Method          string            `json:"method"`
	Path            string            `json:"path"`
	Body            json.RawMessage   `json:"body,omitempty"`
	Headers         map[string]string `json:"headers"`
	BoardInstanceID string            `json:"board_instance_id"`
	SessionID       string            `json:"session_id"`
	CreatedAt       string            `json:"created_at"`
	Attempts        int               `json:"attempts"`
	LastError       string            `json:"last_error,omitempty"`
}

func pendingPath(key string) string {
	return filepath.Join(Home(), "state", "pending", SafeFile(key)+".json")
}

// Save writes the pending record.
func (p *Pending) Save() error {
	path := pendingPath(p.Key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	return os.WriteFile(path, b, 0o600)
}

// Remove deletes the pending record.
func (p *Pending) Remove() { os.Remove(pendingPath(p.Key)) }

// LoadPending reads one pending record.
func LoadPending(key string) (*Pending, error) {
	b, err := os.ReadFile(pendingPath(key))
	if err != nil {
		return nil, err
	}
	var p Pending
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ListPending returns all pending records.
func ListPending() ([]*Pending, error) {
	dir := filepath.Join(Home(), "state", "pending")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Pending
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			if p, err := LoadPending(strings.TrimSuffix(e.Name(), ".json")); err == nil {
				out = append(out, p)
			}
		}
	}
	return out, nil
}
