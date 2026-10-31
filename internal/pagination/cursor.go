// Package pagination implements opaque signed cursors bound to the board
// generation, the principal, and the normalized query shape.
package pagination

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
)

// Kind identifies which traversal a cursor continues.
type Kind string

const (
	KindFeed     Kind = "feed"
	KindActivity Kind = "activity"
	KindComments Kind = "comments"
	KindSearch   Kind = "search"
)

// Payload is the signed cursor body.
type Payload struct {
	V         int    `json:"v"`
	Kind      Kind   `json:"k"`
	Gen       string `json:"g"`
	Principal string `json:"p"`
	FP        string `json:"f"`
	Exp       int64  `json:"e"`
	Watermark int64  `json:"w"`
	Revision  int64  `json:"r,omitempty"`
	Pos       int64  `json:"a"`
	Pos2      int64  `json:"b,omitempty"`
	Score     uint64 `json:"s,omitempty"`
	Total     int64  `json:"t,omitempty"`
}

// Errors returned by Decode. Callers map them to HTTP codes.
var (
	ErrMalformed = errors.New("cursor_invalid")
	ErrSignature = errors.New("cursor_invalid_signature")
	ErrExpired   = errors.New("cursor_expired")
	ErrMismatch  = errors.New("cursor_mismatch")
	ErrGen       = errors.New("cursor_generation")
)

// Fingerprint hashes the normalized query shape.
func Fingerprint(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return base64.RawURLEncoding.EncodeToString(h[:12])
}

// Encode signs and encodes p.
func Encode(secret []byte, p Payload) (string, error) {
	p.V = 1
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	sig := mac.Sum(nil)
	s := base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(sig)
	if len(s) > domain.CursorMaxLen {
		return "", errors.New("cursor too long")
	}
	return s, nil
}

// Decode verifies a cursor against the secret, generation, principal, kind,
// and fingerprint, and checks expiry.
func Decode(secret []byte, s string, kind Kind, gen, principal, fp string, now time.Time) (*Payload, error) {
	if len(s) == 0 || len(s) > domain.CursorMaxLen {
		return nil, ErrMalformed
	}
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return nil, ErrMalformed
	}
	body, err := base64.RawURLEncoding.DecodeString(s[:dot])
	if err != nil {
		return nil, ErrMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(s[dot+1:])
	if err != nil {
		return nil, ErrMalformed
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, ErrSignature
	}
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil || p.V != 1 {
		return nil, ErrMalformed
	}
	if p.Gen != gen {
		return nil, ErrGen
	}
	if now.Unix() >= p.Exp {
		return nil, ErrExpired
	}
	if p.Kind != kind || p.Principal != principal || p.FP != fp {
		return nil, ErrMismatch
	}
	return &p, nil
}
