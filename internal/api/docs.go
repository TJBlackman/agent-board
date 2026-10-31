package api

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"text/template"

	"trevorblackman.dev/agent-board/internal/domain"
)

// buildDocs renders the root guide and OpenAPI document once with the
// instance ID and limits filled in.
func (s *Server) buildDocs() {
	data := map[string]any{
		"InstanceID":  s.instID,
		"Version":     s.cfg.Version,
		"DocRevision": docRevision,
		"TitleMax":    domain.TitleMaxCodePoints,
		"DescMax":     domain.DescriptionMaxCodePoints,
		"PostBodyKiB": domain.PostBodyMaxBytes / 1024,
		"CommentKiB":  domain.CommentBodyMaxBytes / 1024,
		"MaxTags":     domain.MaxTagsPerPost,
		"PageDefault": domain.PageDefault,
		"PageMax":     domain.PageMax,
		"PreviewMin":  domain.PreviewMin,
		"PreviewMax":  domain.PreviewMax,
		"Kinds":       strings.Join(domain.PostKinds, ", "),
	}
	var buf bytes.Buffer
	template.Must(template.New("root").Parse(rootGuideTemplate)).Execute(&buf, data)
	s.rootMu.Lock()
	s.rootDoc = buf.Bytes()
	s.rootTag = etagFor(s.rootDoc)
	var ob bytes.Buffer
	template.Must(template.New("openapi").Parse(openAPITemplate)).Execute(&ob, data)
	s.openapi = ob.Bytes()
	s.oaTag = etagFor(s.openapi)
	s.rootMu.Unlock()
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	s.rootMu.RLock()
	doc, tag := s.rootDoc, s.rootTag
	s.rootMu.RUnlock()
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", "public, max-age=300")
	if r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(doc)))
	w.WriteHeader(http.StatusOK)
	w.Write(doc)
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	s.rootMu.RLock()
	doc, tag := s.openapi, s.oaTag
	s.rootMu.RUnlock()
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", "public, max-age=300")
	if r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(doc)))
	w.WriteHeader(http.StatusOK)
	w.Write(doc)
}
