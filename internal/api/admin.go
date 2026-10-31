package api

import (
	"context"
	"net/http"
	"time"

	"trevorblackman.dev/agent-board/internal/storage"
	"trevorblackman.dev/agent-board/internal/strictjson"
)

// adminRoutes serve operator actions on the loopback listener. There is no
// authentication beyond network reachability: the listener binds loopback
// inside the container and is reached only through docker exec.
func (s *Server) adminRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/metrics", func(w http.ResponseWriter, r *http.Request) {
		s.metrics.mu.Lock()
		defer s.metrics.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"metrics":           s.metrics,
			"write_queue_depth": s.db.Writer.Depth.Load(),
			"writes_committed":  s.db.Writer.Committed.Load(),
			"writes_failed":     s.db.Writer.Failed.Load(),
			"writer_wait_ms":    s.db.Writer.WaitNanos.Load() / 1e6,
			"board_instance_id": s.instID,
			"version":           s.cfg.Version,
		})
	})
	mux.HandleFunc("GET /admin/tokens", func(w http.ResponseWriter, r *http.Request) {
		ps, err := s.db.ListPrincipals(r.Context())
		if err != nil {
			writeError(w, r, translate(err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"principals": ps})
	})
	mux.HandleFunc("POST /admin/tokens", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Label      string `json:"label"`
			Permission string `json:"permission"`
		}
		b := make([]byte, 0, 512)
		buf := make([]byte, 512)
		for {
			n, err := r.Body.Read(buf)
			b = append(b, buf[:n]...)
			if err != nil || len(b) > 4096 {
				break
			}
		}
		if err := strictjson.Decode(b, 2, &req); err != nil {
			writeError(w, r, translate(err))
			return
		}
		p, tok, err := s.db.CreateToken(r.Context(), req.Label, req.Permission)
		if err != nil {
			writeError(w, r, errf(http.StatusBadRequest, "token_create_failed", err.Error()))
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"principal_id": p.ID, "label": p.Label, "permission": p.Permission, "token": tok})
	})
	mux.HandleFunc("DELETE /admin/tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.db.RevokePrincipal(r.Context(), r.PathValue("id")); err != nil {
			writeError(w, r, errf(http.StatusNotFound, "revoke_failed", err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
	})
	mux.HandleFunc("POST /admin/backups", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		dir, m, err := s.db.Backup(ctx, s.cfg.BackupDir)
		if err != nil {
			writeError(w, r, errf(http.StatusInternalServerError, "backup_failed", err.Error()))
			return
		}
		s.metrics.add(&s.metrics.Backups, 1)
		writeJSON(w, http.StatusCreated, map[string]any{"directory": dir, "manifest": m})
	})
	mux.HandleFunc("GET /admin/backups", func(w http.ResponseWriter, r *http.Request) {
		names, err := storage.ListBackups(s.cfg.BackupDir)
		if err != nil {
			writeError(w, r, translate(err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"directory": s.cfg.BackupDir, "backups": names})
	})
	mux.HandleFunc("POST /admin/search/rebuild", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		n, err := s.db.RebuildSearch(ctx)
		if err != nil {
			writeError(w, r, translate(err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"documents": n})
	})
	mux.HandleFunc("POST /admin/cleanup", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		res, err := s.db.Cleanup(ctx)
		if err != nil {
			writeError(w, r, translate(err))
			return
		}
		s.recordCleanup(res)
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /admin/generation/rotate", func(w http.ResponseWriter, r *http.Request) {
		gen, err := s.db.RotateGeneration(r.Context())
		if err != nil {
			writeError(w, r, translate(err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"generation_id": gen})
	})
	return mux
}
