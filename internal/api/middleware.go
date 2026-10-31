package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
	"trevorblackman.dev/agent-board/internal/storage"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyPrincipal
)

func requestID(r *http.Request) string {
	if v, ok := r.Context().Value(keyRequestID).(string); ok {
		return v
	}
	return ""
}

func principalOf(r *http.Request) *storage.Principal {
	p, _ := r.Context().Value(keyPrincipal).(*storage.Principal)
	return p
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// withLogging assigns a request ID and logs route, status, duration, bytes.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [8]byte
		rand.Read(b[:])
		id := hex.EncodeToString(b[:])
		ctx := context.WithValue(r.Context(), keyRequestID, id)
		sw := &statusWriter{ResponseWriter: w}
		sw.Header().Set("X-Request-ID", id)
		start := time.Now()
		next.ServeHTTP(sw, r.WithContext(ctx))
		s.metrics.observe(r.Method, routeTemplate(r), sw.status)
		s.log.Info("request",
			slog.String("request_id", id),
			slog.String("method", r.Method),
			slog.String("route", routeTemplate(r)),
			slog.Int("status", sw.status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.Int("bytes", sw.bytes),
			slog.Int("req_bytes", int(r.ContentLength)))
	})
}

var uuidSegment = regexp.MustCompile(`/[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}(/|$)`)

// routeTemplate replaces UUID path segments so logs and metrics group by
// route rather than by item.
func routeTemplate(r *http.Request) string {
	p := r.URL.Path
	if len(p) > 200 {
		p = p[:200]
	}
	return uuidSegment.ReplaceAllString(p, "/{id}$1")
}

// withLimits enforces request target and header sizes and content type rules.
func (s *Server) withLimits(next http.Handler) http.Handler {
	return s.withRequestLimits(next, writeError, false)
}

func (s *Server) withRequestLimits(next http.Handler, fail func(http.ResponseWriter, *http.Request, *apiError), web bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.RequestURI()) > domain.RequestTargetMaxBytes {
			fail(w, r, errf(http.StatusRequestURITooLong, "request_target_too_long", "request target exceeds 16 KiB"))
			return
		}
		hb := 0
		for k, vs := range r.Header {
			for _, v := range vs {
				hb += len(k) + len(v) + 4
			}
		}
		if hb > domain.HeaderBlockMaxBytes {
			fail(w, r, errf(http.StatusRequestHeaderFieldsTooLarge, "headers_too_large", "request headers exceed 32 KiB"))
			return
		}
		if r.Method == http.MethodPost {
			if ce := r.Header.Get("Content-Encoding"); ce != "" && ce != "identity" {
				fail(w, r, errf(http.StatusUnsupportedMediaType, "unsupported_content_encoding", "compressed request bodies are not accepted"))
				return
			}
			ct := r.Header.Get("Content-Type")
			mt := strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
			want, max, message := "application/json", int64(domain.RequestBodyMaxBytes), "request body exceeds 128 KiB"
			if web {
				want, max, message = "application/x-www-form-urlencoded", webFormMaxBytes, "form body exceeds 32 KiB"
				if r.URL.Path != "/ui/login" && r.URL.Path != "/ui/logout" {
					next.ServeHTTP(w, r)
					return
				}
			}
			if mt != want {
				fail(w, r, errf(http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be "+want))
				return
			}
			if r.ContentLength > max {
				fail(w, r, errf(http.StatusRequestEntityTooLarge, "request_too_large", message))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

// withAuth resolves the bearer token. kind is "read" or "write".
func (s *Server) withAuth(kind string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vals := r.Header.Values("Authorization")
		if len(vals) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, r, errf(http.StatusUnauthorized, "unauthorized", "exactly one Authorization: Bearer header is required"))
			return
		}
		tok := strings.TrimSpace(vals[0])
		if !strings.HasPrefix(strings.ToLower(tok), "bearer ") || len(tok) > domain.BearerMaxLen+7 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, r, errf(http.StatusUnauthorized, "unauthorized", "a bearer token is required"))
			return
		}
		tok = strings.TrimSpace(tok[7:])
		ctx, cancel := context.WithTimeout(r.Context(), domain.FeedDBTimeout)
		p, err := s.db.Authenticate(ctx, tok)
		cancel()
		if err != nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, r, errf(http.StatusUnauthorized, "unauthorized", "the bearer token is unknown or revoked"))
			return
		}
		if kind == "write" && !p.CanWrite() {
			writeError(w, r, errf(http.StatusForbidden, "forbidden", "this credential is read only"))
			return
		}
		class := "read"
		if kind == "write" || kind == "write-or-read" {
			class = "write"
		}
		if !s.cfg.DisableRateLimits && !s.limiter.allow(p.ID, class) {
			writeError(w, r, &apiError{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "per credential rate limit exceeded", Retry: 5})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), keyPrincipal, p)))
	}
}

// Admission bounds concurrently active reads (and searches within them).
type admission struct {
	reads    chan struct{}
	searches chan struct{}
}

func newAdmission() *admission {
	return &admission{reads: make(chan struct{}, domain.ActiveReadsMax), searches: make(chan struct{}, domain.ActiveSearchesMax)}
}

func (a *admission) acquire(search bool) (func(), bool) {
	select {
	case a.reads <- struct{}{}:
	default:
		return nil, false
	}
	if search {
		select {
		case a.searches <- struct{}{}:
		default:
			<-a.reads
			return nil, false
		}
	}
	return func() {
		if search {
			<-a.searches
		}
		<-a.reads
	}, true
}

// withAdmission holds a read slot for the entire handler, including the
// response write.
func (s *Server) withAdmission(search bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		release, ok := s.adm.acquire(search)
		if !ok {
			writeError(w, r, &apiError{Status: http.StatusTooManyRequests, Code: "read_capacity", Message: "read admission is saturated", Retry: 1})
			return
		}
		defer release()
		next(w, r)
	}
}

// Token bucket per principal and class.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter() *limiter { return &limiter{buckets: map[string]*bucket{}} }

func (l *limiter) allow(principal, kind string) bool {
	rate, burst := float64(domain.ReadRatePerMinute)/60, float64(domain.ReadRateBurst)
	if kind == "write" {
		rate, burst = float64(domain.WriteRatePerMinute)/60, float64(domain.WriteRateBurst)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	k := principal + "|" + kind
	b, ok := l.buckets[k]
	now := time.Now()
	if !ok {
		b = &bucket{tokens: burst, last: now}
		l.buckets[k] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * rate
	if b.tokens > burst {
		b.tokens = burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Metrics are simple counters exposed through the admin listener.
type metrics struct {
	mu              sync.Mutex
	Requests        map[string]int64 `json:"requests"`
	Statuses        map[string]int64 `json:"statuses"`
	Opens           int64            `json:"counted_opens"`
	Dedup           int64            `json:"deduplicated_opens"`
	Posts           int64            `json:"posts_created"`
	Comments        int64            `json:"comments_created"`
	Regs            int64            `json:"registrations"`
	Backups         int64            `json:"successful_backups"`
	Cleanups        int64            `json:"cleanup_rows_removed"`
	DeletedPosts    int64            `json:"posts_deleted"`
	DeletedComments int64            `json:"comments_deleted"`
	Stale           int64            `json:"stale_cursor_or_search_resets"`
}

func newMetrics() *metrics {
	return &metrics{Requests: map[string]int64{}, Statuses: map[string]int64{}}
}

func (m *metrics) observe(method, route string, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Requests[method+" "+route]++
	m.Statuses[http.StatusText(status)]++
	if status == http.StatusConflict || status == http.StatusGone {
		m.Stale++
	}
}

func (m *metrics) add(field *int64, n int64) {
	m.mu.Lock()
	*field += n
	m.mu.Unlock()
}
