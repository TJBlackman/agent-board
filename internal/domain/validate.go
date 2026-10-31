package domain

import (
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidationError carries the stable error code and the offending field.
type ValidationError struct {
	Code    string
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Field != "" {
		return e.Code + " (" + e.Field + "): " + e.Message
	}
	return e.Code + ": " + e.Message
}

// Verr builds a ValidationError.
func Verr(code, field, msg string) error {
	return &ValidationError{Code: code, Field: field, Message: msg}
}

var (
	slugRe      = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	idemKeyRe   = regexp.MustCompile(`^[A-Za-z0-9-]{16,128}$`)
	uuidRe      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	scopeSegRe  = regexp.MustCompile(`^[a-z0-9.-]+$`)
	winDriveRe  = regexp.MustCompile(`^[A-Za-z]:\\`)
	winUNCRe    = regexp.MustCompile(`^\\\\[^\\/]+\\[^\\/]+`)
	// PostKinds are the descriptive kinds accepted on a post.
	PostKinds     = []string{"finding", "question", "idea", "decision", "failed-attempt", "handoff"}
	hyphenLookers = "\u2010\u2011\u2012\u2013\u2014\u2015\u2212\uFE58\uFE63\uFF0D"
)

// ValidateSlug checks a category or tag label against the canonical grammar.
func ValidateSlug(field, s string) error {
	if s == "" {
		return Verr("invalid_slug", field, "label must not be empty")
	}
	if len(s) > SlugMaxLen {
		return Verr("invalid_slug", field, fmt.Sprintf("label exceeds %d characters", SlugMaxLen))
	}
	if !slugRe.MatchString(s) {
		msg := "labels use lowercase ASCII letters or digits in segments separated by single hyphens"
		if strings.ContainsAny(s, hyphenLookers) {
			msg += "; a Unicode dash lookalike was found, use ASCII hyphen"
		}
		if sug := SuggestSlug(s); sug != "" && sug != s {
			msg += "; suggested spelling: " + sug
		}
		return Verr("invalid_slug", field, msg)
	}
	return nil
}

// SuggestSlug offers a canonical spelling for an invalid label. The server
// never applies it silently.
func SuggestSlug(s string) string {
	var b strings.Builder
	lastHyphen := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	out := strings.TrimSuffix(b.String(), "-")
	if len(out) > SlugMaxLen {
		out = strings.TrimSuffix(out[:SlugMaxLen], "-")
	}
	return out
}

// ValidateTags validates and canonicalizes (sorts, dedupes) a tag list.
func ValidateTags(field string, tags []string) ([]string, error) {
	if len(tags) > MaxTagsPerPost {
		return nil, Verr("too_many_tags", field, fmt.Sprintf("at most %d tags", MaxTagsPerPost))
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(tags))
	for i, t := range tags {
		if err := ValidateSlug(fmt.Sprintf("%s[%d]", field, i), t); err != nil {
			return nil, err
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ValidateScope checks the logical project identifier grammar.
func ValidateScope(field, s string) error {
	if s == "" || len(s) > ScopeMaxLen {
		return Verr("invalid_scope", field, fmt.Sprintf("scope must be 1 to %d ASCII characters", ScopeMaxLen))
	}
	if strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") {
		return Verr("invalid_scope", field, "scope must not start or end with a slash")
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" || !scopeSegRe.MatchString(seg) {
			return Verr("invalid_scope", field, "scope segments use lowercase letters, digits, hyphen, and period, separated by single slashes")
		}
	}
	return nil
}

// ValidateSessionID enforces the session reference grammar.
func ValidateSessionID(field, s string) error {
	if !sessionIDRe.MatchString(s) {
		return Verr("invalid_session_id", field, "session_id must be 1 to 128 characters from [A-Za-z0-9._:-]")
	}
	return nil
}

// ValidateLabelID validates workspace and host identifiers (scope grammar, 1 to 128).
func ValidateLabelID(field, s string) error {
	if err := ValidateScope(field, s); err != nil {
		return Verr("invalid_identifier", field, "identifier uses the scope grammar: lowercase letters, digits, hyphen, period, slash separated segments, 1 to 128 characters")
	}
	return nil
}

// ValidateIdempotencyKey checks the header grammar.
func ValidateIdempotencyKey(s string) error {
	if !idemKeyRe.MatchString(s) {
		return Verr("invalid_idempotency_key", "Idempotency-Key", "key must be 16 to 128 characters from [A-Za-z0-9-]")
	}
	return nil
}

// IsUUID reports whether s is a canonical lowercase UUIDv4.
func IsUUID(s string) bool { return uuidRe.MatchString(s) }

// ValidateUUID validates a canonical UUIDv4 string.
func ValidateUUID(field, s string) error {
	if !IsUUID(s) {
		return Verr("invalid_uuid", field, "value must be a canonical lowercase 36 character UUIDv4")
	}
	return nil
}

// NewUUID generates an RFC 9562 UUIDv4 from crypto/rand.
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random source: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	const hex = "0123456789abcdef"
	out := make([]byte, 36)
	j := 0
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[j] = '-'
			j++
		}
		out[j] = hex[v>>4]
		out[j+1] = hex[v&0x0f]
		j += 2
	}
	return string(out), nil
}

// CountCodePoints returns the number of Unicode scalar values in s.
func CountCodePoints(s string) int { return utf8.RuneCountInString(s) }

// ValidateSingleLine validates title and description style fields.
func ValidateSingleLine(field, s string, maxCP int) error {
	if !utf8.ValidString(s) {
		return Verr("invalid_utf8", field, "value is not valid UTF-8")
	}
	if strings.TrimSpace(s) == "" {
		return Verr("empty_value", field, "value must not be empty or whitespace only")
	}
	if n := CountCodePoints(s); n > maxCP {
		return Verr("too_long", field, fmt.Sprintf("value has %d code points; the limit is %d", n, maxCP))
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return Verr("invalid_character", field, "single line values must not contain control characters or line separators")
		}
	}
	return nil
}

// ValidateBody validates a post or comment body against its byte limit.
func ValidateBody(field, s string, maxBytes int) error {
	if !utf8.ValidString(s) {
		return Verr("invalid_utf8", field, "value is not valid UTF-8")
	}
	if strings.TrimSpace(s) == "" {
		return Verr("empty_value", field, "body must not be empty or whitespace only")
	}
	if len(s) > maxBytes {
		return Verr("too_long", field, fmt.Sprintf("body is %d bytes encoded as UTF-8; the limit is %d", len(s), maxBytes))
	}
	for _, r := range s {
		if r == '\r' || r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return Verr("invalid_character", field, "bodies may contain only carriage return, line feed, and tab as control characters")
		}
	}
	return nil
}

// ValidateBounded checks a label length in code points and rejects controls.
func ValidateBounded(field, s string, min, max int) error {
	if !utf8.ValidString(s) {
		return Verr("invalid_utf8", field, "value is not valid UTF-8")
	}
	n := CountCodePoints(s)
	if n < min || n > max {
		return Verr("invalid_length", field, fmt.Sprintf("value must be %d to %d characters", min, max))
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return Verr("invalid_character", field, "value must not contain control characters")
		}
	}
	return nil
}

// ValidatePostKind checks the descriptive kind label.
func ValidatePostKind(field, k string) error {
	for _, v := range PostKinds {
		if v == k {
			return nil
		}
	}
	return Verr("invalid_kind", field, "kind must be one of "+strings.Join(PostKinds, ", "))
}

// ValidatePath validates an absolute directory path under the given path style
// without touching the server filesystem.
func ValidatePath(field, p, style string) error {
	if !utf8.ValidString(p) {
		return Verr("invalid_utf8", field, "path is not valid UTF-8")
	}
	if p == "" || len(p) > PathMaxBytes {
		return Verr("invalid_path", field, fmt.Sprintf("path must be 1 to %d UTF-8 bytes", PathMaxBytes))
	}
	for _, r := range p {
		if r == 0 || unicode.IsControl(r) {
			return Verr("invalid_path", field, "path must not contain control characters or NUL")
		}
	}
	var segs []string
	switch style {
	case "windows":
		if !winDriveRe.MatchString(p) && !winUNCRe.MatchString(p) {
			return Verr("invalid_path", field, "windows paths must be drive rooted (C:\\...) or UNC (\\\\server\\share\\...)")
		}
		if strings.Contains(p, "/") {
			return Verr("invalid_path", field, "windows paths use backslash separators only")
		}
		segs = strings.Split(p, "\\")
	case "posix":
		if !strings.HasPrefix(p, "/") {
			return Verr("invalid_path", field, "posix paths must start with /")
		}
		segs = strings.Split(p, "/")
	default:
		return Verr("invalid_path_style", "path_style", "path_style must be windows or posix")
	}
	for _, s := range segs {
		if s == "." || s == ".." {
			return Verr("invalid_path", field, "path must not contain . or .. segments")
		}
	}
	return nil
}

// DetectPathStyle guesses the style from spelling. Used by the client only.
func DetectPathStyle(p string) string {
	if winDriveRe.MatchString(p) || winUNCRe.MatchString(p) {
		return "windows"
	}
	return "posix"
}

// AsValidation reports whether err is a *ValidationError.
func AsValidation(err error) (*ValidationError, bool) {
	var v *ValidationError
	if errors.As(err, &v) {
		return v, true
	}
	return nil, false
}
