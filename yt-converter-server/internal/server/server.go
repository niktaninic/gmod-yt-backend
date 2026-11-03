// Package server wires the HTTP API, the stream endpoint, the dev console and
// background maintenance together.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/config"
	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/spotify"
	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/store"
	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/ytdlp"
)

type Server struct {
	cfg     *config.Config
	store   *store.Store
	yt      *ytdlp.Runner
	spotify *spotify.Client // nil when not configured

	metrics *Metrics
	conv    *converter

	floodLimiter *limiterSet
	workLimiter  *limiterSet
	loginLimiter *limiterSet

	ranksMu    sync.RWMutex
	knownRanks []string

	sessions *sessionStore

	baseCtx context.Context
	started time.Time
}

func New(ctx context.Context, cfg *config.Config, st *store.Store, yt *ytdlp.Runner) *Server {
	s := &Server{
		cfg:          cfg,
		store:        st,
		yt:           yt,
		metrics:      newMetrics(),
		floodLimiter: newLimiterSet(time.Minute),
		workLimiter:  newLimiterSet(cfg.RateLimitWindow),
		loginLimiter: newLimiterSet(15 * time.Minute),
		sessions:     newSessionStore(8 * time.Hour),
		baseCtx:      ctx,
		started:      time.Now(),
	}
	if cfg.SpotifyEnabled() {
		s.spotify = spotify.New(cfg.SpotifyClientID, cfg.SpotifyClientSecret)
	}
	s.conv = newConverter(s)

	go s.floodLimiter.gcLoop(ctx)
	go s.workLimiter.gcLoop(ctx)
	go s.loginLimiter.gcLoop(ctx)
	go s.sessions.gcLoop(ctx)
	go s.janitorLoop(ctx)
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/convert", s.withAuth(s.handleConvert))
	mux.HandleFunc("POST /api/info", s.withAuth(s.handleInfo))
	mux.HandleFunc("POST /api/playlist", s.withAuth(s.handlePlaylist))
	mux.HandleFunc("POST /api/prefetch", s.withAuth(s.serverOnly(s.handlePrefetch)))
	mux.HandleFunc("GET /api/history", s.withAuth(s.serverOnly(s.handleHistory)))
	mux.HandleFunc("POST /api/ranks", s.withAuth(s.serverOnly(s.handleRanks)))
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /stream/{file}", s.handleStream)
	mux.HandleFunc("HEAD /stream/{file}", s.handleStream)

	if s.cfg.DevConsoleEnabled {
		s.registerDev(mux)
		slog.Info("dev console enabled", "path", "/dev")
	}

	return s.recoverer(s.logger(mux))
}

// ---- middleware --------------------------------------------------------------

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
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
	w.bytes += int64(n)
	return n, err
}

// ReadFrom keeps the sendfile(2) fast path of the underlying writer.
func (w *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		w.bytes += n
		return n, err
	}
	n, err := io.Copy(w.ResponseWriter, r)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/api/health" {
			return
		}
		lvl := slog.LevelInfo
		if strings.HasPrefix(r.URL.Path, "/stream/") || strings.HasPrefix(r.URL.Path, "/dev/api/") {
			lvl = slog.LevelDebug
		}
		if sw.status >= 500 {
			lvl = slog.LevelWarn
		}
		slog.Log(r.Context(), lvl, "http",
			"method", r.Method, "path", r.URL.Path, "status", sw.status,
			"bytes", sw.bytes, "ms", time.Since(start).Milliseconds(), "ip", s.clientIP(r))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				slog.Error("panic in handler", "err", v, "path", r.URL.Path, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "INTERNAL", "Internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---- helpers -------------------------------------------------------------------

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" && net.ParseIP(v) != nil {
			return v
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			// right-most entry is the one our trusted proxy appended
			if v := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(v) != nil {
				return v
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

type errorBody struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Code    string `json:"code"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Success: false, Error: msg, Code: code})
}

func writeRetryAfter(w http.ResponseWriter, d time.Duration) {
	sec := int(d.Seconds())
	if sec < 1 {
		sec = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(sec))
}

// readJSON decodes a small JSON body. GMod's HTTP() sends JSON with whatever
// Content-Type we set, so the header is not enforced here.
func readJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}
