package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/config"
	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/media"
	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/store"
	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/ytdlp"
)

// ---- request / response types ------------------------------------------------------

type convertRequest struct {
	URL      string `json:"url"`
	Nick     string `json:"nick"`
	SteamID  string `json:"steamid"`
	Rank     string `json:"rank"`
	ServerIP string `json:"server_ip"`
	PlayerIP string `json:"player_ip"`
}

type convertResponse struct {
	Success   bool   `json:"success"`
	StreamURL string `json:"stream_url"`
	VideoID   string `json:"video_id"`
	TrackID   string `json:"track_id,omitempty"`
	Source    string `json:"source"`
	Title     string `json:"title"`
	Duration  int    `json:"duration"`
	Cached    bool   `json:"cached"`
}

// apiError carries an HTTP status plus a machine readable code that the Lua
// side maps to its own error ids (no more string matching on "too long").
type apiError struct {
	status int
	code   string
	msg    string
	retry  time.Duration
}

func (e *apiError) Error() string { return e.msg }

func errBad(code, msg string) *apiError {
	return &apiError{status: http.StatusBadRequest, code: code, msg: msg}
}

// caller is the resolved request context for one conversion.
type caller struct {
	id       identity
	nick     string
	steamID  string
	rank     string
	serverIP string
	playerIP string
	ip       string
	limits   config.Effective
	prefetch bool
}

func (s *Server) newCaller(id identity, req convertRequest, ip string) caller {
	c := caller{id: id, ip: ip, nick: media.CleanText(req.Nick, 64)}
	if id.server {
		// trusted: the GMod server tells us who the player is
		if media.ValidSteamID(req.SteamID) {
			c.steamID = req.SteamID
		}
		if config.ValidRank(req.Rank) {
			c.rank = req.Rank
		}
		if media.ValidIP(req.ServerIP) {
			c.serverIP = req.ServerIP
		}
		if media.ValidIP(req.PlayerIP) {
			c.playerIP = req.PlayerIP
		}
	} else {
		c.steamID = id.steamID
		c.rank = id.rank
		c.playerIP = ip
	}
	if c.rank == "" {
		c.rank = "default"
	}
	if c.playerIP == "" {
		c.playerIP = ip
	}
	c.limits = s.cfg.LimitsFor(c.rank)
	return c
}

// ---- conversion engine ------------------------------------------------------------

type flight struct {
	done   chan struct{}
	res    audio
	err    error
	maxDur int
}

type audio struct {
	VideoID  string
	Title    string
	Duration int
	Size     int64
}

type converter struct {
	s *Server

	mu      sync.Mutex
	flights map[string]*flight

	inflightMu sync.Mutex
	inflight   map[string]int // steamid -> fresh conversions in progress

	spotifyGroup singleflight.Group

	touchMu sync.Mutex
	touched map[string]time.Time

	playlistMu    sync.Mutex
	playlists     map[string]*playlistResult
	playlistGroup singleflight.Group
}

// touch slides the cache expiry, at most once per 10 minutes per track, so a
// hot track doesn't cost a DB write on every play.
func (cv *converter) touch(ctx context.Context, videoID string) {
	now := time.Now()
	cv.touchMu.Lock()
	if t, ok := cv.touched[videoID]; ok && now.Sub(t) < 10*time.Minute {
		cv.touchMu.Unlock()
		return
	}
	if len(cv.touched) > 100000 {
		cv.touched = map[string]time.Time{}
	}
	cv.touched[videoID] = now
	cv.touchMu.Unlock()
	_ = cv.s.store.TouchCache(ctx, videoID, time.Duration(cv.s.cfg.CacheDays)*24*time.Hour)
}

func newConverter(s *Server) *converter {
	return &converter{s: s, flights: map[string]*flight{}, inflight: map[string]int{}, touched: map[string]time.Time{}, playlists: map[string]*playlistResult{}}
}

func (s *Server) mp3Path(videoID string) string {
	return filepath.Join(s.cfg.CacheDir, videoID+".mp3")
}

// lookupCache returns a cache hit if the DB row and the file both exist.
func (cv *converter) lookupCache(ctx context.Context, videoID string) (*audio, error) {
	s := cv.s
	e, err := s.store.GetCache(ctx, videoID)
	if err != nil || e == nil {
		return nil, err
	}
	if _, err := os.Stat(s.mp3Path(videoID)); err != nil {
		_ = s.store.DeleteCache(ctx, videoID)
		return nil, nil
	}
	return &audio{VideoID: videoID, Title: e.Title, Duration: e.Duration, Size: e.FileSize}, nil
}

func tooLong(dur, max int) *apiError {
	return &apiError{status: http.StatusBadRequest, code: "TOO_LONG",
		msg: (&ytdlp.TooLongError{Duration: dur, Max: max}).Error()}
}

// ensure returns the mp3 for videoID, converting it if needed. Concurrent
// callers for the same id share one yt-dlp run. fresh is true only for the
// caller that actually ran the conversion.
func (cv *converter) ensure(ctx context.Context, videoID string, maxDur int, logf func(string)) (a audio, fresh bool, err error) {
	for attempt := 0; attempt < 2; attempt++ {
		if hit, err := cv.lookupCache(ctx, videoID); err != nil {
			return audio{}, false, err
		} else if hit != nil {
			// limits are enforced on cache hits too; previously a "user" could
			// play a 2h track as soon as an admin had converted it once
			if maxDur > 0 && hit.Duration > maxDur {
				return audio{}, false, tooLong(hit.Duration, maxDur)
			}
			cv.touch(ctx, videoID)
			return *hit, false, nil
		}

		cv.mu.Lock()
		if f, ok := cv.flights[videoID]; ok {
			cv.mu.Unlock()
			logf("conversion already running for " + videoID + ", waiting for it")
			select {
			case <-f.done:
			case <-ctx.Done():
				return audio{}, false, ctx.Err()
			}
			if f.err == nil {
				if maxDur > 0 && f.res.Duration > maxDur {
					return audio{}, false, tooLong(f.res.Duration, maxDur)
				}
				return f.res, false, nil
			}
			var tl *ytdlp.TooLongError
			if errors.As(f.err, &tl) && f.maxDur > 0 && (maxDur == 0 || maxDur > f.maxDur) {
				continue // leader had a stricter limit than we do; try ourselves
			}
			return audio{}, false, f.err
		}
		f := &flight{done: make(chan struct{}), maxDur: maxDur}
		cv.flights[videoID] = f
		cv.mu.Unlock()

		f.res, f.err = cv.run(videoID, maxDur, logf)

		cv.mu.Lock()
		delete(cv.flights, videoID)
		cv.mu.Unlock()
		close(f.done)
		return f.res, f.err == nil, f.err
	}
	return audio{}, false, tooLong(0, maxDur)
}

// run executes yt-dlp detached from the HTTP request: if the first player
// disconnects, everyone waiting for the same track still gets it.
func (cv *converter) run(videoID string, maxDur int, logf func(string)) (audio, error) {
	s := cv.s
	s.metrics.activeConv.Add(1)
	defer s.metrics.activeConv.Add(-1)

	res, err := s.yt.Convert(s.baseCtx, videoID, maxDur, logf)
	if err != nil {
		return audio{}, err
	}
	a := audio{VideoID: videoID, Title: res.Title, Duration: res.Duration, Size: res.Size}
	if err := s.store.PutCache(context.Background(), store.CacheEntry{
		VideoID: videoID, Title: res.Title, Duration: res.Duration, FileSize: res.Size,
	}, res.Path, time.Duration(s.cfg.CacheDays)*24*time.Hour); err != nil {
		slog.Error("cache index write failed", "video", videoID, "err", err)
	}
	if s.cfg.MaxCacheSizeMB > 0 {
		go s.enforceCacheSize(context.Background())
	}
	return a, nil
}

// reserve/release track fresh conversions per player so concurrent requests
// can't slip past the daily limit before history rows are written.
func (cv *converter) reserve(steamID string) {
	if steamID == "" {
		return
	}
	cv.inflightMu.Lock()
	cv.inflight[steamID]++
	cv.inflightMu.Unlock()
}

func (cv *converter) release(steamID string) {
	if steamID == "" {
		return
	}
	cv.inflightMu.Lock()
	if cv.inflight[steamID] <= 1 {
		delete(cv.inflight, steamID)
	} else {
		cv.inflight[steamID]--
	}
	cv.inflightMu.Unlock()
}

func (cv *converter) inflightFor(steamID string) int {
	cv.inflightMu.Lock()
	defer cv.inflightMu.Unlock()
	return cv.inflight[steamID]
}

// ---- spotify resolution ---------------------------------------------------------------

type resolved struct {
	VideoID string
	Title   string
}

func (s *Server) resolveSpotify(ctx context.Context, trackID string, logf func(string)) (resolved, error) {
	if e, err := s.store.GetSpotify(ctx, trackID); err == nil && e != nil && media.ValidVideoID(e.VideoID) {
		logf("spotify mapping cached: " + trackID + " -> " + e.VideoID)
		return resolved{VideoID: e.VideoID, Title: e.Title}, nil
	}
	if s.spotify == nil {
		return resolved{}, errBad("SPOTIFY_DISABLED", "Spotify not configured on this server")
	}
	v, err, _ := s.conv.spotifyGroup.Do(trackID, func() (any, error) {
		ctx, cancel := context.WithTimeout(s.baseCtx, 60*time.Second)
		defer cancel()
		t0 := time.Now()
		tr, err := s.spotify.GetTrack(ctx, trackID)
		if err != nil {
			return nil, err
		}
		query := tr.Title()
		logf(fmt.Sprintf("spotify track: %q (%ds), searching YouTube", query, tr.Duration))
		results, err := s.yt.Search(ctx, query, 5)
		if err != nil {
			return nil, err
		}
		best := pickBestMatch(results, tr.Duration)
		logf(fmt.Sprintf("spotify -> youtube: %s -> %s (%q, %ds) in %s", trackID, best.ID, best.Title, best.Duration, time.Since(t0).Round(time.Millisecond)))
		_ = s.store.PutSpotify(context.Background(), store.SpotifyEntry{
			TrackID: trackID, VideoID: best.ID, Title: tr.Title(), Artist: tr.Artists, Duration: tr.Duration,
		})
		s.metrics.spotifyResolves.Add(1)
		return resolved{VideoID: best.ID, Title: tr.Title()}, nil
	})
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			return resolved{}, ae
		}
		slog.Warn("spotify resolution failed", "track", trackID, "err", err)
		return resolved{}, &apiError{status: http.StatusBadGateway, code: "SPOTIFY_FAILED", msg: "Spotify resolution failed"}
	}
	return v.(resolved), nil
}

// pickBestMatch prefers results whose length is close to the Spotify track;
// the old code always took the first hit (often a music video with a long
// intro or a 1h loop).
func pickBestMatch(results []ytdlp.Info, target int) ytdlp.Info {
	best := results[0]
	bestScore := math.MaxFloat64
	for i, r := range results {
		if r.IsLive {
			continue
		}
		diff := 600.0
		if r.Duration > 0 && target > 0 {
			diff = math.Abs(float64(r.Duration - target))
		}
		score := diff + float64(i)*4 // mild bias towards YouTube's own ranking
		if score < bestScore {
			best, bestScore = r, score
		}
	}
	return best
}

// ---- handlers -----------------------------------------------------------------------------

func (s *Server) handleConvert(w http.ResponseWriter, r *http.Request) {
	var req convertRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid JSON body")
		return
	}
	status, body := s.doConvert(r.Context(), identityFrom(r), req, s.clientIP(r), func(string) {}, false)
	if ae, ok := body.(*apiError); ok {
		if ae.retry > 0 {
			writeRetryAfter(w, ae.retry)
		}
		writeError(w, status, ae.code, ae.msg)
		return
	}
	writeJSON(w, status, body)
}

// doConvert is shared by /api/convert, the dev console simulator and the
// SSE test endpoint.
func (s *Server) doConvert(ctx context.Context, id identity, req convertRequest, ip string, logf func(string), prefetch bool) (int, any) {
	t0 := time.Now()
	s.metrics.apiCalls.Add(1)
	c := s.newCaller(id, req, ip)
	c.prefetch = prefetch
	url := strings.TrimSpace(req.URL)

	fail := func(err error, videoID string) (int, any) {
		var ae *apiError
		var tl *ytdlp.TooLongError
		switch {
		case errors.As(err, &ae):
		case errors.As(err, &tl):
			ae = tooLong(tl.Duration, tl.Max)
		case errors.Is(err, ytdlp.LiveError{}):
			ae = errBad("LIVE_UNSUPPORTED", "Live streams are not supported")
		case errors.Is(err, ytdlp.ErrBusy):
			ae = &apiError{status: http.StatusServiceUnavailable, code: "BUSY", msg: "Converter is busy, try again shortly", retry: 15 * time.Second}
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			ae = &apiError{status: http.StatusGatewayTimeout, code: "TIMEOUT", msg: "Conversion timed out"}
		default:
			slog.Warn("conversion failed", "video", videoID, "err", err)
			ae = &apiError{status: http.StatusInternalServerError, code: "CONVERSION_FAILED", msg: "Conversion failed"}
		}
		if ae.code != "RATE_LIMITED" && ae.code != "DAILY_LIMIT" && ae.code != "BLOCKED" {
			s.metrics.recordError(videoID, ae.msg, c, time.Since(t0))
		}
		logf("ERROR: " + ae.msg)
		return ae.status, ae
	}

	if url == "" {
		return fail(errBad("INVALID_URL", `Missing "url" parameter`), "")
	}
	if len(url) > 2048 {
		return fail(errBad("INVALID_URL", "URL too long"), "")
	}
	if s.store.IsBlocked(c.steamID) {
		return fail(&apiError{status: http.StatusForbidden, code: "BLOCKED", msg: "Blocked"}, "")
	}

	source := "youtube"
	videoID := media.YouTubeVideoID(url)
	trackID := ""
	title := ""
	if videoID == "" {
		trackID = media.SpotifyTrackID(url)
		if trackID == "" {
			return fail(errBad("INVALID_URL", "Invalid URL. Supported: YouTube, Spotify"), "")
		}
		source = "spotify"
		// spotify resolution costs a search, count it as work
		if e, _ := s.store.GetSpotify(ctx, trackID); e == nil {
			if key, max := s.workKey(c.id, c.ip); !s.workLimiter.Allow(key, max) {
				return fail(&apiError{status: http.StatusTooManyRequests, code: "RATE_LIMITED", msg: "Too many requests, please try again later", retry: s.cfg.RateLimitWindow / 4}, "")
			}
		}
		res, err := s.resolveSpotify(ctx, trackID, logf)
		if err != nil {
			return fail(err, "")
		}
		videoID, title = res.VideoID, res.Title
	}
	logf("video id: " + videoID)

	maxDur := c.limits.MaxDurationSeconds

	// fast path: cached
	if hit, err := s.conv.lookupCache(ctx, videoID); err == nil && hit != nil {
		if maxDur > 0 && hit.Duration > maxDur {
			return fail(tooLong(hit.Duration, maxDur), videoID)
		}
		s.conv.touch(ctx, videoID)
		logf(fmt.Sprintf("cache hit: %q (%ds, %d KB)", hit.Title, hit.Duration, hit.Size/1024))
		return s.finish(ctx, c, *hit, title, source, trackID, false, t0)
	}

	// fresh work beyond this point: rate limit + daily limit
	if key, max := s.workKey(c.id, c.ip); !s.workLimiter.Allow(key, max) {
		return fail(&apiError{status: http.StatusTooManyRequests, code: "RATE_LIMITED", msg: "Too many requests, please try again later", retry: s.cfg.RateLimitWindow / 4}, videoID)
	}
	if c.steamID != "" && c.limits.DailyConversions > 0 {
		n, err := s.store.DailyConversions(ctx, c.steamID)
		if err != nil {
			return fail(err, videoID)
		}
		if n+s.conv.inflightFor(c.steamID) >= c.limits.DailyConversions {
			return fail(&apiError{status: http.StatusTooManyRequests, code: "DAILY_LIMIT",
				msg: fmt.Sprintf("Daily conversion limit reached (%d/day for rank %q)", c.limits.DailyConversions, c.rank)}, videoID)
		}
	}

	s.conv.reserve(c.steamID)
	a, fresh, err := s.conv.ensure(ctx, videoID, maxDur, logf)
	s.conv.release(c.steamID)
	if err != nil {
		return fail(err, videoID)
	}
	return s.finish(ctx, c, a, title, source, trackID, fresh, t0)
}

func (s *Server) finish(ctx context.Context, c caller, a audio, title, source, trackID string, fresh bool, t0 time.Time) (int, any) {
	if title == "" {
		title = a.Title
	}
	if c.prefetch && !fresh {
		// already cached, nothing happened worth recording
		return http.StatusOK, convertResponse{Success: true, StreamURL: "/stream/" + a.VideoID + ".mp3", VideoID: a.VideoID, Source: source, Title: title, Duration: a.Duration, Cached: true}
	}
	if err := s.store.AddHistory(ctx, store.HistoryEntry{
		VideoID: a.VideoID, Title: title, Duration: a.Duration, Nick: c.nick, SteamID: c.steamID,
		ServerIP: c.serverIP, Cached: boolInt(!fresh), Source: source, Rank: c.rank,
	}); err != nil {
		slog.Warn("history insert failed", "err", err)
	}
	s.metrics.recordConversion(a.VideoID, title, source, c, !fresh, time.Since(t0))
	return http.StatusOK, convertResponse{
		Success:   true,
		StreamURL: "/stream/" + a.VideoID + ".mp3",
		VideoID:   a.VideoID,
		TrackID:   trackID,
		Source:    source,
		Title:     title,
		Duration:  a.Duration,
		Cached:    !fresh,
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// /api/info: metadata only
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	var req convertRequest
	if err := readJSON(w, r, &req); err != nil || strings.TrimSpace(req.URL) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_URL", `Missing "url" parameter`)
		return
	}
	s.metrics.apiCalls.Add(1)
	id := identityFrom(r)
	c := s.newCaller(id, req, s.clientIP(r))
	maxDur := c.limits.MaxDurationSeconds
	allowed := func(d int) bool { return maxDur <= 0 || d <= maxDur }

	if key, max := s.workKey(id, c.ip); !s.workLimiter.Allow(key, max) {
		writeRetryAfter(w, s.cfg.RateLimitWindow/4)
		writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests, please try again later")
		return
	}

	if trackID := media.SpotifyTrackID(req.URL); trackID != "" {
		if s.spotify == nil {
			writeError(w, http.StatusBadRequest, "SPOTIFY_DISABLED", "Spotify not configured on this server")
			return
		}
		tr, err := s.spotify.GetTrack(r.Context(), trackID)
		if err != nil {
			writeError(w, http.StatusBadGateway, "SPOTIFY_FAILED", "Failed to fetch info")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"source": "spotify", "track_id": trackID, "title": tr.Title(), "artist": tr.Artists,
			"track_name": tr.Name, "duration": tr.Duration, "duration_formatted": media.FormatDuration(tr.Duration),
			"allowed": allowed(tr.Duration), "max_duration": maxDur,
		})
		return
	}

	videoID := media.YouTubeVideoID(req.URL)
	if videoID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_URL", "Invalid URL. Supported: YouTube, Spotify")
		return
	}
	var inf ytdlp.Info
	if e, _ := s.store.GetCache(r.Context(), videoID); e != nil {
		inf = ytdlp.Info{ID: videoID, Title: e.Title, Duration: e.Duration}
	} else {
		var err error
		inf, err = s.yt.Info(r.Context(), videoID)
		if err != nil {
			slog.Warn("info failed", "video", videoID, "err", err)
			writeError(w, http.StatusInternalServerError, "INFO_FAILED", "Failed to fetch info")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source": "youtube", "video_id": videoID, "title": inf.Title, "duration": inf.Duration,
		"duration_formatted": media.FormatDuration(inf.Duration), "is_live": inf.IsLive,
		"allowed": allowed(inf.Duration) && !inf.IsLive, "max_duration": maxDur,
	})
}

type playlistTrack struct {
	URL      string `json:"url"`
	VideoID  string `json:"video_id,omitempty"`
	TrackID  string `json:"track_id,omitempty"`
	Title    string `json:"title"`
	Duration int    `json:"duration"`
}

type playlistResult struct {
	Source     string          `json:"source"`
	PlaylistID string          `json:"playlist_id"`
	Tracks     []playlistTrack `json:"tracks"`
	expires    time.Time
}

// playlists are cached for 10 minutes and fetched once for concurrent
// callers: every GMod client resolves the same playlist URL.
func (s *Server) cachedPlaylist(key string, fetch func(ctx context.Context) (*playlistResult, error)) (*playlistResult, bool, error) {
	s.conv.playlistMu.Lock()
	if p, ok := s.conv.playlists[key]; ok && time.Now().Before(p.expires) {
		s.conv.playlistMu.Unlock()
		return p, true, nil
	}
	s.conv.playlistMu.Unlock()

	v, err, _ := s.conv.playlistGroup.Do(key, func() (any, error) {
		ctx, cancel := context.WithTimeout(s.baseCtx, 2*time.Minute)
		defer cancel()
		p, err := fetch(ctx)
		if err != nil {
			return nil, err
		}
		p.expires = time.Now().Add(10 * time.Minute)
		s.conv.playlistMu.Lock()
		if len(s.conv.playlists) > 500 {
			s.conv.playlists = map[string]*playlistResult{}
		}
		s.conv.playlists[key] = p
		s.conv.playlistMu.Unlock()
		return p, nil
	})
	if err != nil {
		return nil, false, err
	}
	return v.(*playlistResult), false, nil
}

func (s *Server) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	var req convertRequest
	if err := readJSON(w, r, &req); err != nil || strings.TrimSpace(req.URL) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_URL", `Missing "url" parameter`)
		return
	}
	s.metrics.apiCalls.Add(1)
	id := identityFrom(r)

	var key string
	var fetch func(ctx context.Context) (*playlistResult, error)

	if pid := media.SpotifyPlaylistID(req.URL); pid != "" {
		if s.spotify == nil {
			writeError(w, http.StatusBadRequest, "SPOTIFY_DISABLED", "Spotify not configured on this server")
			return
		}
		key = "sp:" + pid
		fetch = func(ctx context.Context) (*playlistResult, error) {
			tracks, err := s.spotify.PlaylistTracks(ctx, pid, s.cfg.MaxPlaylistItems)
			if err != nil {
				return nil, err
			}
			out := make([]playlistTrack, 0, len(tracks))
			for _, t := range tracks {
				out = append(out, playlistTrack{URL: "spotify:track:" + t.ID, TrackID: t.ID, Title: t.Title(), Duration: t.Duration})
			}
			return &playlistResult{Source: "spotify", PlaylistID: pid, Tracks: out}, nil
		}
	} else if pid := media.YouTubePlaylistID(req.URL); pid != "" {
		key = "yt:" + pid
		fetch = func(ctx context.Context) (*playlistResult, error) {
			items, err := s.yt.Playlist(ctx, pid, s.cfg.MaxPlaylistItems)
			if err != nil {
				return nil, err
			}
			out := make([]playlistTrack, 0, len(items))
			for _, it := range items {
				out = append(out, playlistTrack{URL: "https://www.youtube.com/watch?v=" + it.ID, VideoID: it.ID, Title: it.Title, Duration: it.Duration})
			}
			return &playlistResult{Source: "youtube", PlaylistID: pid, Tracks: out}, nil
		}
	} else {
		writeError(w, http.StatusBadRequest, "INVALID_URL", "Invalid playlist URL. Supported: YouTube playlist, Spotify playlist")
		return
	}

	s.conv.playlistMu.Lock()
	p, hit := s.conv.playlists[key]
	hit = hit && time.Now().Before(p.expires)
	s.conv.playlistMu.Unlock()
	if !hit {
		if k, max := s.workKey(id, s.clientIP(r)); !s.workLimiter.Allow(k, max) {
			writeRetryAfter(w, s.cfg.RateLimitWindow/4)
			writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests, please try again later")
			return
		}
	}

	res, _, err := s.cachedPlaylist(key, fetch)
	if err != nil || res == nil || len(res.Tracks) == 0 {
		slog.Warn("playlist fetch failed", "playlist", key, "err", err)
		writeError(w, http.StatusBadGateway, "PLAYLIST_FAILED", "Failed to fetch playlist")
		return
	}

	// drop entries this caller could never play, otherwise the radio stops
	// in the middle of the playlist on the first too-long / deleted video
	c := s.newCaller(id, req, s.clientIP(r))
	tracks, skipped := filterPlaylist(res.Tracks, c.limits.MaxDurationSeconds)
	if len(tracks) == 0 {
		writeError(w, http.StatusBadRequest, "TOO_LONG", "No playable tracks in this playlist (all too long or unavailable)")
		return
	}

	// start at the video the link points to (watch?v=X&list=Y), not at #1
	start := 1
	if vid := media.YouTubeVideoID(req.URL); vid != "" {
		for i, t := range tracks {
			if t.VideoID == vid {
				start = i + 1
				break
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"source":      res.Source,
		"playlist_id": res.PlaylistID,
		"tracks":      tracks,
		"skipped":     skipped,
		"start_index": start,
	})
}

var unavailableTitles = map[string]bool{
	"[Deleted video]": true, "[Private video]": true, "[Unavailable video]": true,
}

func filterPlaylist(in []playlistTrack, maxDur int) ([]playlistTrack, int) {
	out := make([]playlistTrack, 0, len(in))
	for _, t := range in {
		if unavailableTitles[t.Title] {
			continue
		}
		if maxDur > 0 && t.Duration > maxDur {
			continue
		}
		out = append(out, t)
	}
	return out, len(in) - len(out)
}

// handlePrefetch converts a track in the background so the next playlist
// item starts instantly. Server key only; it only uses an idle worker and
// never queues behind real requests.
func (s *Server) handlePrefetch(w http.ResponseWriter, r *http.Request) {
	var req convertRequest
	if err := readJSON(w, r, &req); err != nil || strings.TrimSpace(req.URL) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_URL", `Missing "url" parameter`)
		return
	}
	if s.yt.Active()+s.yt.Queued() >= s.cfg.MaxConcurrentConversions {
		writeJSON(w, http.StatusAccepted, map[string]any{"success": true, "queued": false, "reason": "busy"})
		return
	}
	id, ip := identityFrom(r), s.clientIP(r)
	go func() {
		ctx, cancel := context.WithTimeout(s.baseCtx, s.cfg.ConversionTimeout+time.Minute)
		defer cancel()
		status, body := s.doConvert(ctx, id, req, ip, func(string) {}, true)
		if ae, ok := body.(*apiError); ok && status >= 500 {
			slog.Debug("prefetch failed", "url", req.URL, "code", ae.code)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"success": true, "queued": true})
}
