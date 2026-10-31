package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"trevorblackman.dev/agent-board/internal/domain"
	"trevorblackman.dev/agent-board/internal/storage"
)

// params parses a query string strictly: known names only, each at most once.
type params struct {
	v url.Values
}

func parseParams(r *http.Request, allowed ...string) (*params, *apiError) {
	q := r.URL.Query()
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for k, vs := range q {
		if !ok[k] {
			return nil, errField(http.StatusBadRequest, "unknown_parameter", k, "unknown query parameter")
		}
		if len(vs) != 1 {
			return nil, errField(http.StatusBadRequest, "duplicate_parameter", k, "each query parameter may occur once")
		}
	}
	return &params{v: q}, nil
}

func (p *params) has(k string) bool   { _, ok := p.v[k]; return ok }
func (p *params) str(k string) string { return p.v.Get(k) }

func (p *params) intRange(k string, def, min, max int) (int, *apiError) {
	if !p.has(k) {
		return def, nil
	}
	n, err := strconv.Atoi(p.str(k))
	if err != nil || n < min || n > max {
		return 0, errField(http.StatusBadRequest, "invalid_parameter", k, "must be an integer from "+strconv.Itoa(min)+" to "+strconv.Itoa(max))
	}
	return n, nil
}

func (p *params) filters() (storage.Filters, *apiError) {
	var f storage.Filters
	if p.has("category") {
		c := p.str("category")
		if err := domain.ValidateSlug("category", c); err != nil {
			return f, translate(err)
		}
		f.Category = &c
	}
	if p.has("scope") {
		s := p.str("scope")
		if err := domain.ValidateScope("scope", s); err != nil {
			return f, translate(err)
		}
		f.Scope = &s
	}
	if p.has("tags") {
		raw := p.str("tags")
		if raw == "" {
			return f, errField(http.StatusBadRequest, "invalid_parameter", "tags", "tags must not be empty when present")
		}
		tags, err := domain.ValidateTags("tags", strings.Split(raw, ","))
		if err != nil {
			return f, translate(err)
		}
		f.Tags = tags
	}
	if p.has("tag_match") {
		switch p.str("tag_match") {
		case "all":
		case "any":
			f.TagMatchAny = true
		default:
			return f, errField(http.StatusBadRequest, "invalid_parameter", "tag_match", "tag_match must be all or any")
		}
		if len(f.Tags) == 0 {
			return f, errField(http.StatusBadRequest, "invalid_parameter", "tag_match", "tag_match requires tags")
		}
	}
	return f, nil
}

// singleHeader requires exactly one value for a header.
func singleHeader(r *http.Request, name string) (string, *apiError) {
	vs := r.Header.Values(name)
	if len(vs) != 1 {
		return "", errField(http.StatusBadRequest, "missing_header", name, "exactly one "+name+" header is required")
	}
	return strings.TrimSpace(vs[0]), nil
}

func uuidHeader(r *http.Request, name string) (string, *apiError) {
	v, e := singleHeader(r, name)
	if e != nil {
		return "", e
	}
	if !domain.IsUUID(v) {
		return "", errField(http.StatusUnprocessableEntity, "invalid_uuid", name, name+" must be a canonical lowercase UUIDv4")
	}
	return v, nil
}
