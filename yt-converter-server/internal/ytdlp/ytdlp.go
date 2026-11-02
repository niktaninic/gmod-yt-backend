// Package ytdlp drives the yt-dlp binary. Everything goes through bounded
// worker pools so a burst of requests can never fork-bomb the host.
package ytdlp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/config"
)

var (
	ErrBusy     = errors.New("converter queue is full")
	ErrNotFound = errors.New("no results")
)

// TooLongError is returned when a video exceeds the allowed duration.
type TooLongError struct {
	Duration, Max int
}

func (e *TooLongError) Error() string {
	return fmt.Sprintf("Video too long (%d:%02d). Maximum allowed: %d:%02d.",
		e.Duration/60, e.Duration%60, e.Max/60, e.Max%60)
}

// LiveError is returned for live streams (they would download forever).
type LiveError struct{}

func (LiveError) Error() string { return "Live streams are not supported" }

type Info struct {
	ID       string
	Title    string
	Duration int
	IsLive   bool
}

type Result struct {
	Info
	Path string
	Size int64
}

type Runner struct {
	cfg *config.Config

	convSem chan struct{}
	metaSem chan struct{}
	queued  atomic.Int64
	active  atomic.Int64

	impersonate string
	version     string

	infoCache *ttlCache[Info]
}

func New(cfg *config.Config) *Runner {
	r := &Runner{
		cfg:       cfg,
		convSem:   make(chan struct{}, cfg.MaxConcurrentConversions),
		metaSem:   make(chan struct{}, cfg.MaxConcurrentMeta),
		infoCache: newTTLCache[Info](2048, time.Hour),
	}
	r.detect()
	return r
}

func (r *Runner) Version() string     { return r.version }
func (r *Runner) Impersonate() string { return r.impersonate }
func (r *Runner) Active() int         { return int(r.active.Load()) }
func (r *Runner) Queued() int         { return int(r.queued.Load()) }

// detect figures out the yt-dlp version and the impersonation target once.
func (r *Runner) detect() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if out, err := exec.CommandContext(ctx, r.cfg.YtdlpPath, "--version").Output(); err == nil {
		r.version = strings.TrimSpace(firstLine(out))
		slog.Info("yt-dlp detected", "version", r.version)
	} else {
		slog.Error("yt-dlp not found or not runnable", "path", r.cfg.YtdlpPath, "err", err)
	}

	switch strings.ToLower(r.cfg.YtdlpImpersonate) {
	case "", "off", "none", "0", "false":
		return
	case "auto":
	default:
		r.impersonate = r.cfg.YtdlpImpersonate
		return
	}
	out, err := exec.CommandContext(ctx, r.cfg.YtdlpPath, "--ignore-config", "--list-impersonate-targets").Output()
	if err != nil {
		slog.Warn("could not list impersonate targets", "err", err)
		return
	}
	// The old Node code matched a lowercase "chrome" prefix, but yt-dlp prints
	// "Chrome-…", so impersonation was silently never enabled.
	for _, line := range strings.Split(string(out), "\n") {
		l := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(l, "chrome") && !strings.Contains(l, "unavailable") {
			r.impersonate = "chrome"
			break
		}
	}
	if r.impersonate != "" {
		slog.Info("yt-dlp impersonation enabled", "target", r.impersonate)
	} else {
		slog.Warn("no usable chrome impersonate target (is curl_cffi installed?), running without --impersonate")
	}
}

// ---- process helpers -------------------------------------------------------

func (r *Runner) baseArgs() []string {
	a := []string{
		"--ignore-config",
		"--no-color",
		"--no-progress",
		"--socket-timeout", "20",
		"--cache-dir", r.cfg.YtdlpCacheDir,
	}
	if r.cfg.JSRuntime != "" && r.cfg.JSRuntime != "none" {
		a = append(a, "--js-runtimes", r.cfg.JSRuntime)
	}
	if r.impersonate != "" {
		a = append(a, "--impersonate", r.impersonate)
	}
	return a
}

// cookiesCopy gives every invocation its own writable copy of the cookie jar.
// yt-dlp writes cookies back on exit; sharing one file between parallel
// processes (old behaviour) could corrupt it.
func (r *Runner) cookiesCopy() (string, func()) {
	src, err := os.Open(r.cfg.CookiesFile)
	if err != nil {
		return "", func() {}
	}
	defer src.Close()
	dst, err := os.CreateTemp(r.cfg.TmpDir, "cookies-*.txt")
	if err != nil {
		slog.Warn("cookies copy failed", "err", err)
		return "", func() {}
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(dst.Name())
		return "", func() {}
	}
	dst.Close()
	name := dst.Name()
	return name, func() { os.Remove(name) }
}

type runOut struct {
	stdout, stderr []byte
	exitCode       int
}

func (r *Runner) run(ctx context.Context, args []string) (runOut, error) {
	cookies, cleanup := r.cookiesCopy()
	defer cleanup()
	full := r.baseArgs()
	if cookies != "" {
		full = append(full, "--cookies", cookies)
	}
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, r.cfg.YtdlpPath, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedBuffer{buf: &stdout, max: 8 << 20}
	cmd.Stderr = &limitedBuffer{buf: &stderr, max: 64 << 10}
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1", "PYTHONDONTWRITEBYTECODE=1")
	// own process group so a timeout also kills ffmpeg children
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()
	out := runOut{stdout: stdout.Bytes(), stderr: stderr.Bytes()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			out.exitCode = ee.ExitCode()
		} else {
			out.exitCode = -1
		}
		if ctx.Err() != nil {
			return out, fmt.Errorf("yt-dlp timed out or was cancelled: %w", ctx.Err())
		}
		return out, fmt.Errorf("yt-dlp exited with %d: %s", out.exitCode, lastLines(out.stderr, 3))
	}
	return out, nil
}

func (r *Runner) acquireMeta(ctx context.Context) (func(), error) {
	select {
	case r.metaSem <- struct{}{}:
		return func() { <-r.metaSem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ---- public API ------------------------------------------------------------

const printFmt = "%(id)s\t%(duration|0)s\t%(is_live|False)s\t%(title)s"

func parseInfoLine(line string) (Info, bool) {
	parts := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 4)
	if len(parts) < 4 || parts[0] == "" || parts[0] == "NA" {
		return Info{}, false
	}
	d, _ := strconv.ParseFloat(parts[1], 64)
	title := parts[3]
	if title == "NA" {
		title = ""
	}
	return Info{
		ID:       parts[0],
		Duration: int(math.Floor(d)),
		IsLive:   parts[2] == "True",
		Title:    title,
	}, true
}

func watchURL(id string) string { return "https://www.youtube.com/watch?v=" + id }

// Info fetches metadata only (memoised for an hour).
func (r *Runner) Info(ctx context.Context, videoID string) (Info, error) {
	if inf, ok := r.infoCache.Get(videoID); ok {
		return inf, nil
	}
	release, err := r.acquireMeta(ctx)
	if err != nil {
		return Info{}, err
	}
	defer release()

	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	out, err := r.run(ctx, []string{"--no-playlist", "--skip-download", "--print", printFmt, watchURL(videoID)})
	if err != nil {
		return Info{}, err
	}
	inf, ok := parseInfoLine(firstLine(out.stdout))
	if !ok {
		return Info{}, errors.New("could not parse yt-dlp output")
	}
	r.infoCache.Set(videoID, inf)
	return inf, nil
}

// Search returns up to n flat search results.
func (r *Runner) Search(ctx context.Context, query string, n int) ([]Info, error) {
	release, err := r.acquireMeta(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	out, err := r.run(ctx, []string{"--flat-playlist", "--skip-download", "--print", printFmt,
		fmt.Sprintf("ytsearch%d:%s", n, query)})
	if err != nil {
		return nil, err
	}
	var res []Info
	for _, line := range strings.Split(string(out.stdout), "\n") {
		if inf, ok := parseInfoLine(line); ok && len(inf.ID) == 11 {
			res = append(res, inf)
		}
	}
	if len(res) == 0 {
		return nil, ErrNotFound
	}
	return res, nil
}

// Playlist lists the entries of a YouTube playlist without resolving them.
// --print instead of --dump-json keeps stdout tiny for big playlists.
func (r *Runner) Playlist(ctx context.Context, playlistID string, max int) ([]Info, error) {
	release, err := r.acquireMeta(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	args := []string{"--flat-playlist", "--yes-playlist", "--skip-download", "--print", printFmt}
	if max > 0 {
		args = append(args, "--playlist-end", strconv.Itoa(max))
	}
	args = append(args, "https://www.youtube.com/playlist?list="+playlistID)
	out, err := r.run(ctx, args)
	if err != nil {
		return nil, err
	}
	var res []Info
	for _, line := range strings.Split(string(out.stdout), "\n") {
		if inf, ok := parseInfoLine(line); ok && len(inf.ID) == 11 {
			res = append(res, inf)
		}
	}
	if len(res) == 0 {
		return nil, ErrNotFound
	}
	return res, nil
}

// Convert downloads the audio of videoID and transcodes it to mp3 in ONE
// yt-dlp call. Metadata is printed at the pre_process stage, i.e. before the
// match filter runs, so a too-long video is rejected without any download
// and without a second metadata call (the old code did two calls).
func (r *Runner) Convert(ctx context.Context, videoID string, maxDuration int, log func(string)) (Result, error) {
	if log == nil {
		log = func(string) {}
	}
	// cheap pre-check if we already know the duration
	if inf, ok := r.infoCache.Get(videoID); ok {
		if inf.IsLive {
			return Result{}, LiveError{}
		}
		if maxDuration > 0 && inf.Duration > maxDuration {
			return Result{}, &TooLongError{Duration: inf.Duration, Max: maxDuration}
		}
	}

	if q := r.queued.Add(1); r.cfg.MaxQueuedConversions > 0 && q > int64(r.cfg.MaxQueuedConversions+r.cfg.MaxConcurrentConversions) {
		r.queued.Add(-1)
		return Result{}, ErrBusy
	}
	select {
	case r.convSem <- struct{}{}:
	case <-ctx.Done():
		r.queued.Add(-1)
		return Result{}, ctx.Err()
	}
	r.queued.Add(-1)
	r.active.Add(1)
	defer func() { r.active.Add(-1); <-r.convSem }()

	ctx, cancel := context.WithTimeout(ctx, r.cfg.ConversionTimeout)
	defer cancel()

	tmpBase := filepath.Join(r.cfg.TmpDir, videoID)
	defer cleanupGlob(tmpBase + ".*")

	filter := "!is_live"
	if maxDuration > 0 {
		filter += fmt.Sprintf(" & duration <= %d", maxDuration)
	}
	args := []string{
		"--no-playlist",
		"--print", "pre_process:" + printFmt,
		"--no-simulate",
		// --break-match-filters exits with 101 on reject. A plain --match-filters
		// exits 0 with no file, which the old code reported as a 500 error.
		"--break-match-filters", filter,
		"-f", r.cfg.AudioFormatSort,
		"-x", "--audio-format", "mp3", "--audio-quality", strconv.Itoa(r.cfg.AudioQuality),
		"--concurrent-fragments", "4",
		"--retries", "3", "--fragment-retries", "5", "--extractor-retries", "2",
		"--no-mtime", "--no-write-thumbnail",
		"-o", tmpBase + ".%(ext)s",
		watchURL(videoID),
	}
	log(fmt.Sprintf("yt-dlp: filter=%q quality=%d format=%q", filter, r.cfg.AudioQuality, r.cfg.AudioFormatSort))

	started := time.Now()
	out, runErr := r.run(ctx, args)
	inf, haveInfo := parseInfoLine(firstLine(out.stdout))
	if haveInfo {
		r.infoCache.Set(videoID, inf)
		log(fmt.Sprintf("metadata: %q, %ds, live=%v", inf.Title, inf.Duration, inf.IsLive))
	}

	if runErr != nil {
		// 101 = download cancelled by --match-filter / --break-match-filters
		if out.exitCode == 101 {
			if haveInfo && inf.IsLive {
				return Result{}, LiveError{}
			}
			d := 0
			if haveInfo {
				d = inf.Duration
			}
			return Result{}, &TooLongError{Duration: d, Max: maxDuration}
		}
		return Result{}, runErr
	}
	if haveInfo && maxDuration > 0 && inf.Duration > maxDuration {
		return Result{}, &TooLongError{Duration: inf.Duration, Max: maxDuration}
	}

	src := tmpBase + ".mp3"
	st, err := os.Stat(src)
	if err != nil || st.Size() == 0 {
		return Result{}, fmt.Errorf("yt-dlp produced no mp3 (stderr: %s)", lastLines(out.stderr, 3))
	}
	dst := filepath.Join(r.cfg.CacheDir, videoID+".mp3")
	if err := os.Rename(src, dst); err != nil {
		return Result{}, fmt.Errorf("move into cache: %w", err)
	}
	log(fmt.Sprintf("done in %s, %d KB", time.Since(started).Round(time.Millisecond), st.Size()/1024))

	if !haveInfo {
		inf = Info{ID: videoID, Title: videoID}
	}
	return Result{Info: inf, Path: dst, Size: st.Size()}, nil
}

// ---- small utils -------------------------------------------------------------

func cleanupGlob(pattern string) {
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

func firstLine(b []byte) string {
	s := string(b)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func lastLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	s := strings.Join(lines, " | ")
	if len(s) > 600 {
		s = s[len(s)-600:]
	}
	if s == "" {
		return "(no stderr)"
	}
	return s
}

type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}

// ttlCache is a tiny bounded map with expiry; good enough for metadata.
type ttlCache[V any] struct {
	mu   sync.Mutex
	max  int
	ttl  time.Duration
	data map[string]ttlItem[V]
}

type ttlItem[V any] struct {
	v   V
	exp time.Time
}

func newTTLCache[V any](max int, ttl time.Duration) *ttlCache[V] {
	return &ttlCache[V]{max: max, ttl: ttl, data: make(map[string]ttlItem[V], max)}
}

func (c *ttlCache[V]) Get(k string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.data[k]
	if !ok || time.Now().After(it.exp) {
		var zero V
		if ok {
			delete(c.data, k)
		}
		return zero, false
	}
	return it.v, true
}

func (c *ttlCache[V]) Set(k string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.data) >= c.max {
		now := time.Now()
		for key, it := range c.data { // drop expired first, else an arbitrary one
			if now.After(it.exp) {
				delete(c.data, key)
			}
		}
		if len(c.data) >= c.max {
			for key := range c.data {
				delete(c.data, key)
				break
			}
		}
	}
	c.data[k] = ttlItem[V]{v: v, exp: time.Now().Add(c.ttl)}
}
