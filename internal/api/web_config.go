package api

import (
	"errors"
	"net"
	"net/url"
	"strings"

	"trevorblackman.dev/agent-board/internal/domain"
)

// WebConfig enables the personal browser view when PrincipalID and Origin are set.
type WebConfig struct {
	PrincipalID string
	Origin      string
	// AllowHTTP is only for a loopback-bound development server.
	AllowHTTP bool
}

func (c WebConfig) enabled() bool { return c.PrincipalID != "" }

// Validate runs before opening the database, and again for direct API callers.
func (c WebConfig) Validate(listen string) error {
	if c.PrincipalID == "" && c.Origin == "" && !c.AllowHTTP {
		return nil
	}
	if !domain.IsUUID(c.PrincipalID) {
		return errors.New("AGENT_BOARD_WEB_PRINCIPAL_ID must be a canonical UUIDv4")
	}
	u, err := url.Parse(c.Origin)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return errors.New("AGENT_BOARD_WEB_ORIGIN must be an origin without path, credentials, query, or fragment")
	}
	if u.Scheme == "https" && !c.AllowHTTP {
		return nil
	}
	if u.Scheme != "http" || !c.AllowHTTP {
		return errors.New("web origin must use HTTPS (HTTP requires explicit loopback development mode)")
	}
	host, _, err := net.SplitHostPort(listen)
	ip := net.ParseIP(host)
	originIP := net.ParseIP(u.Hostname())
	if err != nil || ip == nil || !ip.IsLoopback() || (originIP == nil || !originIP.IsLoopback()) && u.Hostname() != "localhost" {
		return errors.New("HTTP web development requires loopback origin and listener")
	}
	return nil
}

func isWebPath(p string) bool { return p == "/ui" || strings.HasPrefix(p, "/ui/") }

// safeWebReturn only permits normalized, local private UI destinations.
func safeWebReturn(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 8192 || u.IsAbs() || u.Host != "" || u.User != nil || u.Fragment != "" || strings.ContainsAny(raw, "\\\r\n") || !strings.HasPrefix(u.Path, "/ui/") {
		return "/ui/"
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return "/ui/"
		}
	}
	if strings.Contains(u.Path, "//") || u.Path == "/ui/login" || u.Path == "/ui/logout" {
		return "/ui/"
	}
	return u.String()
}
