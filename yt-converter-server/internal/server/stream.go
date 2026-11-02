package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/config"
	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/media"
)

// handleStream serves cached mp3 files. http.ServeContent handles Range
// (incl. suffix ranges like "bytes=-500" that crashed the old code),
// If-Range / If-Modified-Since, HEAD and 416, and for *os.File the kernel
// sendfile(2) path is used, so file data never passes through user space.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	id, ok := strings.CutSuffix(name, ".mp3")
	if !ok || !media.ValidVideoID(id) {
		writeError(w, http.StatusBadRequest, "INVALID_URL", "Invalid stream URL")
		return
	}

	f, err := os.Open(s.mp3Path(id))
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "File not found or expired")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "File not found or expired")
		return
	}

	s.metrics.activeStreams.Add(1)
	defer s.metrics.activeStreams.Add(-1)

	h := w.Header()
	h.Set("Content-Type", "audio/mpeg")
	h.Set("Cache-Control", "public, max-age=86400, immutable")
	h.Set("ETag", fmt.Sprintf(`"%x-%x"`, st.Size(), st.ModTime().Unix()))
	h.Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	status := "ok"
	code := http.StatusOK
	if s.yt.Version() == "" {
		status, code = "degraded", http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"status":            status,
		"uptime":            int(time.Since(s.started).Seconds()),
		"ytdlp":             s.yt.Version(),
		"activeConversions": s.yt.Active(),
		"queuedConversions": s.yt.Queued(),
		"activeStreams":     s.metrics.activeStreams.Load(),
		"spotify":           s.spotify != nil,
		"goroutines":        runtime.NumGoroutine(),
		"clientTokens":      s.cfg.ClientTokensEnabled,
	})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	page := atoiDefault(r.URL.Query().Get("page"), 1)
	if page < 1 {
		page = 1
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	rows, total, err := s.store.History(r.Context(), limit, (page-1)*limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "history query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"page": page, "limit": limit, "total": total,
		"totalPages": (total + limit - 1) / limit, "entries": rows,
	})
}

func (s *Server) handleRanks(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ranks []string `json:"ranks"`
	}
	if err := readJSON(w, r, &body); err != nil || body.Ranks == nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "ranks must be an array")
		return
	}
	clean := make([]string, 0, len(body.Ranks))
	for _, rk := range body.Ranks {
		if len(clean) >= 256 {
			break
		}
		if config.ValidRank(rk) {
			clean = append(clean, rk)
		}
	}
	s.ranksMu.Lock()
	s.knownRanks = clean
	s.ranksMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stored": len(clean)})
}

// ---- cache maintenance -------------------------------------------------------------

func (s *Server) janitorLoop(ctx context.Context) {
	s.sweepTmp(true)
	s.janitor(ctx, true)
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepTmp(false)
			s.janitor(ctx, false)
			if time.Since(lastPrune) > 24*time.Hour {
				lastPrune = time.Now()
				if n, err := s.store.PruneHistory(ctx, s.cfg.HistoryDays); err == nil && n > 0 {
					slog.Info("history pruned", "rows", n)
				}
				s.store.Optimize(ctx)
			}
		}
	}
}

func (s *Server) removeCached(ctx context.Context, id string) {
	if err := os.Remove(s.mp3Path(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("cache file delete failed", "video", id, "err", err)
	}
	_ = s.store.DeleteCache(ctx, id)
}

func (s *Server) janitor(ctx context.Context, startup bool) {
	// 1. expired entries, in batches until none are left (old code: max 100/h)
	removed := 0
	for {
		ids, err := s.store.ExpiredCacheIDs(ctx, 500)
		if err != nil || len(ids) == 0 {
			break
		}
		for _, id := range ids {
			s.removeCached(ctx, id)
			removed++
		}
	}
	if removed > 0 {
		slog.Info("cache cleanup: expired entries removed", "count", removed)
	}

	// 2. orphans: files without a DB row (and DB rows without a file)
	known, err := s.store.AllCacheIDs(ctx)
	if err == nil {
		entries, _ := os.ReadDir(s.cfg.CacheDir)
		onDisk := make(map[string]struct{}, len(entries))
		orphans := 0
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			id, ok := strings.CutSuffix(name, ".mp3")
			if !ok || !media.ValidVideoID(id) {
				// leftovers from the old Node version (.tmp.mp3, .webm, .part, cookies copy)
				if strings.Contains(name, ".tmp") || strings.HasSuffix(name, ".part") || name == ".cookies_tmp.txt" {
					_ = os.Remove(filepath.Join(s.cfg.CacheDir, name))
				}
				continue
			}
			onDisk[id] = struct{}{}
			if _, ok := known[id]; !ok {
				if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 10*time.Minute {
					_ = os.Remove(filepath.Join(s.cfg.CacheDir, name))
					orphans++
				}
			}
		}
		for id := range known {
			if _, ok := onDisk[id]; !ok {
				_ = s.store.DeleteCache(ctx, id)
			}
		}
		if orphans > 0 {
			slog.Info("cache cleanup: orphan files removed", "count", orphans)
		}
	}

	s.enforceCacheSize(ctx)
	if startup {
		size, n, _ := s.store.CacheTotalSize(ctx)
		slog.Info("cache ready", "entries", n, "sizeMB", size>>20)
	}
}

// enforceCacheSize evicts least recently used tracks above MAX_CACHE_SIZE_MB.
func (s *Server) enforceCacheSize(ctx context.Context) {
	if s.cfg.MaxCacheSizeMB <= 0 {
		return
	}
	limit := s.cfg.MaxCacheSizeMB << 20
	size, _, err := s.store.CacheTotalSize(ctx)
	if err != nil || size <= limit {
		return
	}
	evicted := 0
	for size > limit {
		ids, err := s.store.LRUCacheIDs(ctx, 50)
		if err != nil || len(ids) == 0 {
			break
		}
		before := evicted
		for _, id := range ids {
			s.conv.mu.Lock()
			_, busy := s.conv.flights[id]
			s.conv.mu.Unlock()
			if busy {
				continue
			}
			if st, err := os.Stat(s.mp3Path(id)); err == nil {
				size -= st.Size()
			}
			s.removeCached(ctx, id)
			evicted++
			if size <= limit {
				break
			}
		}
		if evicted == before {
			break // everything left is in use
		}
		size, _, _ = s.store.CacheTotalSize(ctx)
	}
	if evicted > 0 {
		slog.Info("cache size limit enforced", "evicted", evicted, "sizeMB", size>>20)
	}
}

// sweepTmp clears half-finished downloads. On startup everything goes.
func (s *Server) sweepTmp(all bool) {
	entries, err := os.ReadDir(s.cfg.TmpDir)
	if err != nil {
		return
	}
	maxAge := 2 * s.cfg.ConversionTimeout
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if all || time.Since(info.ModTime()) > maxAge {
			_ = os.RemoveAll(filepath.Join(s.cfg.TmpDir, e.Name()))
		}
	}
}

func atoiDefault(s string, def int) int {
	n := 0
	if s == "" || len(s) > 9 {
		return def
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}
