package search

import (
	"strings"
	"unicode"
)

// Normalize collapses all whitespace, including Unicode line separators, to
// single spaces for preview presentation. It does not modify stored text.
func Normalize(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == ' ' || r == ' '
	}), " ")
}

// Truncate cuts s to at most n code points and reports whether it was cut.
func Truncate(s string, n int) (string, bool) {
	rs := []rune(s)
	if len(rs) <= n {
		return s, false
	}
	return string(rs[:n]), true
}

// Excerpt returns a bounded passage around the first occurrence of any word
// in text, or the beginning of text when no word is found. Deterministic.
func Excerpt(text string, words []string, maxCP int) (string, bool) {
	norm := Normalize(text)
	rs := []rune(norm)
	if len(rs) <= maxCP {
		return norm, false
	}
	lower := strings.ToLower(norm)
	best := -1
	for _, w := range words {
		if w == "" {
			continue
		}
		if idx := strings.Index(lower, w); idx >= 0 && (best < 0 || idx < best) {
			best = idx
		}
	}
	if best < 0 {
		s, _ := Truncate(norm, maxCP)
		return s, true
	}
	// Convert byte index to rune index.
	ri := len([]rune(norm[:best]))
	start := ri - maxCP/3
	if start < 0 {
		start = 0
	}
	// Start at a word boundary.
	for start > 0 && start < len(rs) && rs[start] != ' ' {
		start--
	}
	if start > 0 && rs[start] == ' ' {
		start++
	}
	end := start + maxCP
	if end > len(rs) {
		end = len(rs)
		start = end - maxCP
		if start < 0 {
			start = 0
		}
	}
	return string(rs[start:end]), true
}

// ContainsAny reports whether text (case folded) contains any of words.
func ContainsAny(text string, words []string) bool {
	lower := strings.ToLower(text)
	for _, w := range words {
		if w != "" && strings.Contains(lower, w) {
			return true
		}
	}
	return false
}
