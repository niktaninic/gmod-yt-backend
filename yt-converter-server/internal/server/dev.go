package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/config"
	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/media"
)

//go:embed web/dev.html
var devHTML []byte

const loginHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>DEV CONSOLE // LOGIN</title>
<style>
* { box-sizing: border-box; margin: 0; padding: 0; }
body { background: #0d0d0d; color: #c8c8c8; font-family: 'Courier New', monospace; display: flex; align-items: center; justify-content: center; height: 100vh; }
.box { background: #141414; border: 1px solid #2a2a2a; padding: 32px; width: 320px; }
.title { color: #39ff14; font-size: 13px; font-weight: bold; letter-spacing: 1px; text-transform: uppercase; margin-bottom: 20px; }
input { background: #1a1a1a; border: 1px solid #333; color: #c8c8c8; padding: 7px 10px; font-family: inherit; font-size: 13px; width: 100%; outline: none; }
input:focus { border-color: #39ff14; }
button { margin-top: 12px; background: #1a1a1a; border: 1px solid #333; color: #c8c8c8; padding: 7px 12px; font-family: inherit; font-size: 12px; cursor: pointer; width: 100%; text-transform: uppercase; letter-spacing: 0.5px; }
button:hover { border-color: #39ff14; color: #39ff14; }
.err { color: #ff4040; font-size: 12px; margin-top: 10px; }
</style>
</head>
<body>
<div class="box">
  <div class="title">YT Converter // Dev Console</div>
  <form method="POST" action="/dev/login">
    <input type="password" name="token" placeholder="token" autofocus autocomplete="current-password">
    <button type="submit">enter</button>
    {{ERR}}
  </form>
</div>
</body>
</html>`

func writeLogin(w http.ResponseWriter, errHTML string) {
	_, _ = w.Write([]byte(strings.Replace(loginHTML, "{{ERR}}", errHTML, 1)))
}

// ---- sessions -----------------------------------------------------------------------

type sessionStore struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]time.Time
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{ttl: ttl, m: map[string]time.Time{}}
}

func (s *sessionStore) create() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	s.m[tok] = time.Now().Add(s.ttl)
	s.mu.Unlock()
	return tok
}

func (s *sessionStore) valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.m, tok)
		return false
	}
	return true
}

func (s *sessionStore) delete(tok string) {
	s.mu.Lock()
	delete(s.m, tok)
	s.mu.Unlock()
}

// gcLoop drops expired sessions (old code never cleaned them up).
func (s *sessionStore) gcLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.mu.Lock()
			for k, exp := range s.m {
				if now.After(exp) {
					delete(s.m, k)
				}
			}
			s.mu.Unlock()
		}
	}
}

const sessionCookie = "_dcs"

func (s *Server) sessionFrom(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) isHTTPS(r *http.Request) bool {
	return r.TLS != nil || (s.cfg.TrustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
}

func devHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
}

// sameOrigin blocks cross-site requests to state-changing endpoints in
// addition to the SameSite=Strict cookie.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (s *Server) devAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		devHeaders(w)
		if !s.sessions.valid(s.sessionFrom(r)) {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "Unauthorized")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "Cross-origin request rejected")
			return
		}
		next(w, r)
	}
}

func (s *Server) registerDev(mux *http.ServeMux) {
	mux.HandleFunc("GET /dev/login", func(w http.ResponseWriter, r *http.Request) {
		devHeaders(w)
		if s.sessions.valid(s.sessionFrom(r)) {
			http.Redirect(w, r, "/dev", http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		writeLogin(w, "")
	})

	mux.HandleFunc("POST /dev/login", func(w http.ResponseWriter, r *http.Request) {
		devHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		ip := s.clientIP(r)
		if !s.loginLimiter.Allow("login:"+ip, 10) {
			w.WriteHeader(http.StatusTooManyRequests)
			writeLogin(w, `<div class="err">too many attempts, try again later</div>`)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeLogin(w, `<div class="err">bad request</div>`)
			return
		}
		a := sha256.Sum256([]byte(r.PostForm.Get("token")))
		b := sha256.Sum256([]byte(s.cfg.DevConsoleToken))
		if subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			writeLogin(w, `<div class="err">invalid token</div>`)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookie, Value: s.sessions.create(), Path: "/dev",
			MaxAge: int(s.sessions.ttl / time.Second), HttpOnly: true,
			SameSite: http.SameSiteStrictMode, Secure: s.isHTTPS(r),
		})
		http.Redirect(w, r, "/dev", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /dev/logout", func(w http.ResponseWriter, r *http.Request) {
		devHeaders(w)
		s.sessions.delete(s.sessionFrom(r))
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/dev", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/dev/login", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /dev", func(w http.ResponseWriter, r *http.Request) {
		devHeaders(w)
		if !s.sessions.valid(s.sessionFrom(r)) {
			http.Redirect(w, r, "/dev/login", http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(devHTML)
	})

	mux.HandleFunc("GET /dev/api/stats", s.devAuth(s.devStats))
	mux.HandleFunc("GET /dev/api/events", s.devAuth(func(w http.ResponseWriter, r *http.Request) {
		limit := atoiDefault(r.URL.Query().Get("limit"), 100)
		if limit > maxEvents {
			limit = maxEvents
		}
		writeJSON(w, http.StatusOK, s.metrics.recent(limit))
	}))
	mux.HandleFunc("GET /dev/api/players", s.devAuth(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.store.PlayerStats(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, p)
	}))
	mux.HandleFunc("DELETE /dev/api/players/{steamid}", s.devAuth(func(w http.ResponseWriter, r *http.Request) {
		sid := r.PathValue("steamid")
		if sid == "" || len(sid) > 64 {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "steamid required")
			return
		}
		if err := s.store.DeletePlayerHistory(r.Context(), sid); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	mux.HandleFunc("GET /dev/api/cache", s.devAuth(func(w http.ResponseWriter, r *http.Request) {
		rows, err := s.store.ListCache(r.Context(), 200)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		size, n, _ := s.store.CacheTotalSize(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{
			"entries": rows, "totalEntries": n,
			"totalSizeMB": fmt.Sprintf("%.2f", float64(size)/1024/1024),
		})
	}))
	mux.HandleFunc("DELETE /dev/api/cache/{videoId}", s.devAuth(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("videoId")
		if !media.ValidVideoID(id) {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid video id")
			return
		}
		s.removeCached(r.Context(), id)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	mux.HandleFunc("DELETE /dev/api/cache", s.devAuth(func(w http.ResponseWriter, r *http.Request) {
		ids, err := s.store.AllCacheIDs(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		for id := range ids {
			s.removeCached(r.Context(), id)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": len(ids)})
	}))

	mux.HandleFunc("GET /dev/api/rank-limits", s.devAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"limits": s.cfg.RankLimits()})
	}))
	mux.HandleFunc("PUT /dev/api/rank-limits", s.devAuth(s.devPutRankLimits))
	mux.HandleFunc("DELETE /dev/api/rank-limits/{rank}", s.devAuth(func(w http.ResponseWriter, r *http.Request) {
		rank := r.PathValue("rank")
		if !config.ValidRank(rank) {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid rank name")
			return
		}
		next := s.cfg.RankLimits()
		delete(next, rank)
		if err := s.cfg.SaveRankLimits(next); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to write rank-limits.json: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "limits": s.cfg.RankLimits()})
	}))

	mux.HandleFunc("GET /dev/api/blocked", s.devAuth(func(w http.ResponseWriter, r *http.Request) {
		b, err := s.store.ListBlocked(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, b)
	}))
	mux.HandleFunc("POST /dev/api/blocked", s.devAuth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SteamID string `json:"steamid"`
			Reason  string `json:"reason"`
		}
		if err := readJSON(w, r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid JSON")
			return
		}
		sid := strings.TrimSpace(body.SteamID)
		if sid == "" {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "steamid required")
			return
		}
		if len(sid) > 50 {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "steamid too long")
			return
		}
		if err := s.store.Block(r.Context(), sid, media.CleanText(body.Reason, 200)); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	mux.HandleFunc("DELETE /dev/api/blocked/{steamid}", s.devAuth(func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.Unblock(r.Context(), r.PathValue("steamid")); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	mux.HandleFunc("GET /dev/api/convert-test", s.devAuth(s.devConvertTest))
	mux.HandleFunc("POST /dev/api/simulate", s.devAuth(s.devSimulate))
}

func (s *Server) devStats(w http.ResponseWriter, r *http.Request) {
	known := map[string]struct{}{}
	s.ranksMu.RLock()
	for _, k := range s.knownRanks {
		known[k] = struct{}{}
	}
	s.ranksMu.RUnlock()
	limits := s.cfg.RankLimits()
	for k := range limits {
		known[k] = struct{}{}
	}
	ranks := make([]string, 0, len(known))
	for k := range known {
		ranks = append(ranks, k)
	}
	sort.Strings(ranks)

	writeJSON(w, http.StatusOK, map[string]any{
		"system":            s.metrics.system(),
		"totals":            s.metrics.totals(),
		"avgConversionMs":   s.metrics.avgConversionMs(),
		"activeStreams":     s.metrics.activeStreams.Load(),
		"activeConversions": s.metrics.activeConv.Load(),
		"queuedConversions": s.yt.Queued(),
		"ytdlpVersion":      s.yt.Version(),
		"rankLimits":        limits,
		"availableRanks":    ranks,
	})
}

func (s *Server) devPutRankLimits(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Limits map[string]*struct {
			MaxDurationSeconds json.Number `json:"maxDurationSeconds"`
			DailyConversions   json.Number `json:"dailyConversions"`
		} `json:"limits"`
	}
	if err := readJSON(w, r, &body); err != nil || body.Limits == nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "body must be {limits:{...}}")
		return
	}
	cleaned := make(map[string]config.RankLimit, len(body.Limits))
	for rank, lim := range body.Limits {
		if !config.ValidRank(rank) {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid rank name: "+rank)
			return
		}
		if lim == nil { // null = unlimited (README said so, old code crashed on it)
			cleaned[rank] = config.RankLimit{}
			continue
		}
		dur, err1 := numberToInt(lim.MaxDurationSeconds)
		daily, err2 := numberToInt(lim.DailyConversions)
		if err1 != nil || dur < 0 {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", rank+": maxDurationSeconds must be >= 0 (0 = unlimited)")
			return
		}
		if err2 != nil || daily < -1 {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", rank+": dailyConversions must be >= -1")
			return
		}
		cleaned[rank] = config.RankLimit{MaxDurationSeconds: dur, DailyConversions: daily}
	}
	if err := s.cfg.SaveRankLimits(cleaned); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to write rank-limits.json: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "limits": s.cfg.RankLimits()})
}

func numberToInt(n json.Number) (int, error) {
	if n == "" {
		return 0, nil
	}
	if i, err := n.Int64(); err == nil {
		return int(i), nil
	}
	f, err := n.Float64()
	return int(f), err
}

// devConvertTest streams the log of a real conversion as SSE. It goes through
// the same pipeline as /api/convert, so it shares locks and the worker pool
// (the old version could run yt-dlp twice on the same tmp file).
func (s *Server) devConvertTest(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "missing url")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var mu sync.Mutex
	send := func(typ string, data any) {
		b, _ := json.Marshal(data)
		mu.Lock()
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, b)
		fl.Flush()
		mu.Unlock()
	}
	logf := func(msg string) { send("log", map[string]string{"msg": msg}) }

	logf("Starting conversion: " + raw)
	logf("Timestamp: " + time.Now().UTC().Format(time.RFC3339))

	t0 := time.Now()
	status, body := s.doConvert(r.Context(), identity{server: true}, convertRequest{URL: raw, Rank: "dev-console"}, s.clientIP(r), logf)
	if ae, ok := body.(*apiError); ok {
		send("done", map[string]any{"ok": false, "error": ae.msg, "status": status})
		return
	}
	resp := body.(convertResponse)
	logf(fmt.Sprintf("Title: %s | Duration: %ds", resp.Title, resp.Duration))
	send("done", map[string]any{"ok": true, "cached": resp.Cached, "videoId": resp.VideoID, "title": resp.Title, "durationMs": time.Since(t0).Milliseconds()})
}

// devSimulate runs a fake GMod request in-process (no HTTP loopback).
func (s *Server) devSimulate(w http.ResponseWriter, r *http.Request) {
	var req convertRequest
	if err := readJSON(w, r, &req); err != nil || strings.TrimSpace(req.URL) == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "missing url")
		return
	}
	t0 := time.Now()
	status, body := s.doConvert(r.Context(), identity{server: true}, req, s.clientIP(r), func(string) {})
	var resp any = body
	if ae, ok := body.(*apiError); ok {
		resp = errorBody{Success: false, Error: ae.msg, Code: ae.code}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "durationMs": time.Since(t0).Milliseconds(), "response": resp})
}
