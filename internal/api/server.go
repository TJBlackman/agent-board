// Package api serves the HTTP contract: routing, authentication, limits,
// handlers, the root guide, OpenAPI, and the loopback admin listener.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
	"trevorblackman.dev/agent-board/internal/storage"
)

// Config configures a Server.
type Config struct {
	Listen      string
	AdminListen string
	DataDir     string
	BackupDir   string
	BackupTime  string // HH:MM in Location, empty disables the schedule
	Location    *time.Location
	Version     string
	Log         *slog.Logger
	Web         WebConfig
	// DisableRateLimits is for tests only.
	DisableRateLimits bool
}

// Server is the board HTTP service.
type Server struct {
	cfg     Config
	db      *storage.DB
	log     *slog.Logger
	limiter *limiter
	adm     *admission
	metrics *metrics
	instID  string
	public  *http.Server
	admin   *http.Server
	stop    chan struct{}
	wg      sync.WaitGroup
	rootMu  sync.RWMutex
	rootDoc []byte
	rootTag string
	openapi []byte
	oaTag   string
	web     *webView
}

// New builds a server around an open database.
func New(cfg Config, db *storage.DB) (*Server, error) {
	if err := cfg.Web.Validate(cfg.Listen); err != nil {
		return nil, err
	}
	if err := validateAdminListen(cfg.AdminListen); err != nil {
		return nil, err
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	st, err := storage.LoadState(context.Background(), db.Reads())
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, db: db, log: cfg.Log, limiter: newLimiter(), adm: newAdmission(), metrics: newMetrics(), instID: st.InstanceID, stop: make(chan struct{})}
	s.buildDocs()
	if cfg.Web.enabled() {
		if err := s.initWeb(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.withAuth("read", s.handleReady))
	mux.HandleFunc("POST /v1/agents", s.withAuth("write-or-read", s.handleCreateAgent))
	mux.HandleFunc("POST /v1/execution-contexts", s.withAuth("write-or-read", s.handleCreateContext))
	mux.HandleFunc("GET /v1/execution-contexts/{id}", s.withAuth("read", s.withAdmission(false, s.handleGetContext)))
	mux.HandleFunc("GET /v1/taxonomy", s.withAuth("read", s.withAdmission(false, s.handleTaxonomy)))
	mux.HandleFunc("GET /v1/posts", s.withAuth("read", s.withAdmission(false, s.handleFeed)))
	mux.HandleFunc("POST /v1/posts", s.withAuth("write", s.handleCreatePost))
	mux.HandleFunc("GET /v1/posts/{id}", s.withAuth("read", s.withAdmission(false, s.handleGetPost)))
	mux.HandleFunc("HEAD /v1/posts/{id}", s.handleTrackedHead)
	mux.HandleFunc("GET /v1/posts/{id}/comments", s.withAuth("read", s.withAdmission(false, s.handleComments)))
	mux.HandleFunc("POST /v1/posts/{id}/comments", s.withAuth("write", s.handleCreateComment))
	mux.HandleFunc("GET /v1/posts/{id}/stats", s.withAuth("read", s.withAdmission(false, s.handlePostStats)))
	mux.HandleFunc("GET /v1/comments/{id}", s.withAuth("read", s.withAdmission(false, s.handleGetComment)))
	mux.HandleFunc("HEAD /v1/comments/{id}", s.handleTrackedHead)
	mux.HandleFunc("GET /v1/comments/{id}/stats", s.withAuth("read", s.withAdmission(false, s.handleCommentStats)))
	mux.HandleFunc("GET /v1/search", s.withAuth("read", s.withAdmission(true, s.handleSearch)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, errf(http.StatusNotFound, "not_found", "unknown route; read the guide at /"))
	})
	api := s.withLimits(mux)
	web := s.webHandler()
	return s.withLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWebPath(r.URL.Path) {
			web.ServeHTTP(w, r)
		} else {
			api.ServeHTTP(w, r)
		}
	}))
}

// Start listens on both addresses and starts background jobs.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	s.public = &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: domain.ReadHeaderTimeout,
		ReadTimeout:       domain.RequestReadTimeout,
		WriteTimeout:      domain.ResponseWriteTimeout,
		IdleTimeout:       domain.IdleTimeout,
		MaxHeaderBytes:    domain.HeaderBlockMaxBytes,
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.public.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("public listener failed", slog.String("error", err.Error()))
		}
	}()
	if s.cfg.AdminListen != "" {
		aln, err := net.Listen("tcp", s.cfg.AdminListen)
		if err != nil {
			// Do not leave the public listener half started.
			s.public.Close()
			s.wg.Wait()
			return err
		}
		s.admin = &http.Server{Handler: s.adminRoutes(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := s.admin.Serve(aln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log.Error("admin listener failed", slog.String("error", err.Error()))
			}
		}()
	}
	s.wg.Add(1)
	go s.background()
	s.log.Info("listening", slog.String("public", s.cfg.Listen), slog.String("admin", s.cfg.AdminListen), slog.String("instance_id", s.instID), slog.String("version", s.cfg.Version))
	return nil
}

// Shutdown drains in flight work within the configured deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	close(s.stop)
	var first error
	if s.public != nil {
		if err := s.public.Shutdown(ctx); err != nil {
			first = err
		}
	}
	if s.admin != nil {
		if err := s.admin.Shutdown(ctx); err != nil && first == nil {
			first = err
		}
	}
	s.wg.Wait()
	return first
}

// background runs cleanup every ten minutes and the daily backup.
func (s *Server) background() {
	defer s.wg.Done()
	cleanup := time.NewTicker(10 * time.Minute)
	defer cleanup.Stop()
	var backupAt time.Time
	if s.cfg.BackupTime != "" {
		backupAt = s.nextBackup(time.Now())
	}
	check := time.NewTicker(30 * time.Second)
	defer check.Stop()
	// Run one cleanup shortly after start.
	startup := time.After(20 * time.Second)
	for {
		select {
		case <-s.stop:
			return
		case <-startup:
			s.runCleanup()
		case <-cleanup.C:
			s.runCleanup()
		case now := <-check.C:
			if !backupAt.IsZero() && !now.Before(backupAt) {
				s.runBackup()
				backupAt = s.nextBackup(now.Add(time.Minute))
			}
		}
	}
}

func (s *Server) nextBackup(after time.Time) time.Time {
	t, err := time.ParseInLocation("15:04", s.cfg.BackupTime, s.cfg.Location)
	if err != nil {
		s.log.Error("invalid backup time", slog.String("value", s.cfg.BackupTime))
		return time.Time{}
	}
	loc := after.In(s.cfg.Location)
	next := time.Date(loc.Year(), loc.Month(), loc.Day(), t.Hour(), t.Minute(), 0, 0, s.cfg.Location)
	if !next.After(after) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

func (s *Server) runCleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := 0; i < 10; i++ {
		res, err := s.db.Cleanup(ctx)
		if err != nil {
			s.log.Warn("cleanup failed", slog.String("error", err.Error()))
			return
		}
		s.recordCleanup(res)
		if res.Deferred || (res.Posts < domain.PostCleanupBatch && res.Idempotency < domain.CleanupBatch && res.OpenEvents < domain.CleanupBatch && res.DailyFacts < domain.CleanupBatch) {
			return
		}
	}
}

func (s *Server) recordCleanup(res *storage.CleanupResult) {
	s.metrics.add(&s.metrics.Cleanups, res.Idempotency+res.OpenEvents+res.DailyFacts+res.Posts+res.Comments)
	s.metrics.add(&s.metrics.DeletedPosts, res.Posts)
	s.metrics.add(&s.metrics.DeletedComments, res.Comments)
	if res.Posts > 0 {
		s.log.Info("inactive discussions deleted", slog.Int64("posts", res.Posts), slog.Int64("comments", res.Comments))
	}
}

func (s *Server) runBackup() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dir, m, err := s.db.Backup(ctx, s.cfg.BackupDir)
	if err != nil {
		s.log.Error("scheduled backup failed", slog.String("error", err.Error()))
		return
	}
	s.metrics.add(&s.metrics.Backups, 1)
	s.log.Info("backup complete", slog.String("dir", dir), slog.Int64("highest_sequence", m.HighestSequence))
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), domain.FeedDBTimeout)
	defer cancel()
	st, err := storage.LoadState(ctx, s.db.Reads())
	if err != nil || st.SchemaVersion != storage.SchemaVersion || st.MaintenanceState != "normal" {
		writeError(w, r, errf(http.StatusServiceUnavailable, "not_ready", "database or schema not ready"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "schema_version": st.SchemaVersion, "content_revision": st.ContentRevision, "write_queue_depth": s.db.Writer.Depth.Load()})
}

// InstanceID returns the nonsecret board instance identifier.
func (s *Server) InstanceID() string { return s.instID }

// validateAdminListen refuses a non loopback admin address. The admin
// listener has no authentication of its own, so exposing it on any other
// interface hands out token creation to the network. Empty disables it.
func validateAdminListen(addr string) error {
	if addr == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("AGENT_BOARD_ADMIN_LISTEN: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return errors.New("AGENT_BOARD_ADMIN_LISTEN must bind a loopback address because the admin listener is unauthenticated; set it empty to disable operator commands")
	}
	return nil
}
