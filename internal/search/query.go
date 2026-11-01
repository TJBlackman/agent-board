// Package search parses the public query grammar (terms and quoted phrases
// combined with AND) into a safe FTS5 MATCH expression, and builds
// deterministic previews.
package search

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"trevorblackman.dev/agent-board/internal/domain"
)

// Term is one parsed query element.
type Term struct {
	Text   string
	Phrase bool
}

// Parse validates q and returns its terms.
func Parse(q string) ([]Term, error) {
	if !utf8.ValidString(q) {
		return nil, domain.Verr("invalid_query", "q", "query is not valid UTF-8")
	}
	if strings.TrimSpace(q) == "" {
		return nil, domain.Verr("invalid_query", "q", "query must not be empty")
	}
	if domain.CountCodePoints(q) > domain.SearchQueryMaxCodePoints {
		return nil, domain.Verr("invalid_query", "q", "query exceeds 512 code points")
	}
	var terms []Term
	rs := []rune(q)
	i := 0
	for i < len(rs) {
		r := rs[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				j++
			}
			if j >= len(rs) {
				return nil, domain.Verr("invalid_query", "q", "unmatched double quote")
			}
			phrase := normalizeWS(string(rs[i+1 : j]))
			if tokens(phrase) == 0 {
				return nil, domain.Verr("invalid_query", "q", "a quoted phrase has no searchable tokens")
			}
			terms = append(terms, Term{Text: phrase, Phrase: true})
			i = j + 1
		default:
			j := i
			for j < len(rs) && !unicode.IsSpace(rs[j]) && rs[j] != '"' {
				j++
			}
			word := string(rs[i:j])
			if tokens(word) == 0 {
				return nil, domain.Verr("invalid_query", "q", "term "+word+" has no searchable tokens")
			}
			terms = append(terms, Term{Text: word})
			i = j
		}
		if len(terms) > domain.SearchMaxTerms {
			return nil, domain.Verr("invalid_query", "q", "query has more than 32 terms or phrases")
		}
	}
	if len(terms) == 0 {
		return nil, domain.Verr("invalid_query", "q", "query has no searchable tokens")
	}
	return terms, nil
}

func normalizeWS(s string) string { return strings.Join(strings.Fields(s), " ") }

// tokens approximates the unicode61 tokenizer: count runs of letters/digits.
func tokens(s string) int {
	n := 0
	in := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r) {
			if !in {
				n++
				in = true
			}
		} else {
			in = false
		}
	}
	return n
}

// Compile turns parsed terms into an FTS5 MATCH expression. Every term is a
// quoted string literal so FTS5 operators in user input cannot escape.
func Compile(terms []Term) string {
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		lit := `"` + strings.ReplaceAll(t.Text, `"`, `""`) + `"`
		parts = append(parts, lit)
	}
	return strings.Join(parts, " AND ")
}

// Words returns the lowercase token words used for preview positioning.
func Words(terms []Term) []string {
	var out []string
	for _, t := range terms {
		out = append(out, strings.FieldsFunc(strings.ToLower(t.Text), func(r rune) bool {
			return !(unicode.IsLetter(r) || unicode.IsDigit(r))
		})...)
	}
	return out
}
