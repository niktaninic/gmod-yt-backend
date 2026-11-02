// Package config loads runtime configuration from the environment (and an
// optional .env file) and owns the hot-reloadable per-rank limits.
package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RankLimit caps what a single ULX rank may do. Zero / negative values mean
// "no limit" for that particular field.
type RankLimit struct {
	MaxDurationSeconds int `json:"maxDurationSeconds"`
	DailyConversions   int `json:"dailyConversions"`
}

// Effective is the resolved limit set for a single request.
type Effective struct {
	MaxDurationSeconds int // 0 = unlimited
	DailyConversions   int // 0 = unlimited
}

type Config struct {
	ListenAddr string
	Port       int

	APISecret  string
	TrustProxy bool

	CacheDays          int
	MaxCacheSizeMB     int64
	HistoryDays        int
	MaxDurationSeconds int

	RateLimitWindow    time.Duration
	RateLimitMax       int
	RateLimitServerMax int

	MaxConcurrentConversions int
	MaxQueuedConversions     int
	MaxConcurrentMeta        int
	ConversionTimeout        time.Duration
	MaxPlaylistItems         int

	YtdlpPath        string
	YtdlpImpersonate string
	JSRuntime        string
	CookiesFile      string
	AudioQuality     int
	AudioFormatSort  string

	ClientTokensEnabled bool
	ClientTokenMaxTTL   time.Duration

	DevConsoleEnabled bool
	DevConsoleToken   string

	SpotifyClientID     string
	SpotifyClientSecret string

	DataDir        string
	CacheDir       string
	TmpDir         string
	DBPath         string
	RankLimitsPath string
	YtdlpCacheDir  string

	mu         sync.RWMutex
	rankLimits map[string]*RankLimit
}

var placeholderSecrets = map[string]bool{
	"":                                true,
	"change-me":                       true,
	"change-me-to-a-strong-secret":    true,
	"dev-secret-change-in-production": true,
}

var rankRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

// ValidRank reports whether r is an acceptable rank / usergroup name.
func ValidRank(r string) bool { return rankRe.MatchString(r) }

// Load reads configuration. envFile may be empty.
func Load(envFile string) (*Config, error) {
	if envFile != "" {
		if err := loadDotEnv(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", envFile, err)
		}
	}

	dataDir := envStr("DATA_DIR", "./data")
	cacheDir := envStr("CACHE_DIR", "./cache")

	c := &Config{
		ListenAddr: envStr("LISTEN_ADDR", "0.0.0.0"),
		Port:       envInt("PORT", 9999),

		APISecret:  strings.TrimSpace(os.Getenv("API_SECRET")),
		TrustProxy: envBool("TRUST_PROXY", false),

		CacheDays:          envInt("CACHE_DAYS", 30),
		MaxCacheSizeMB:     int64(envInt("MAX_CACHE_SIZE_MB", 0)),
		HistoryDays:        envInt("HISTORY_DAYS", 90),
		MaxDurationSeconds: envInt("MAX_DURATION_SECONDS", 600),

		RateLimitWindow:    time.Duration(envInt("RATE_LIMIT_WINDOW_MS", 60000)) * time.Millisecond,
		RateLimitMax:       envInt("RATE_LIMIT_MAX", 10),
		RateLimitServerMax: envInt("RATE_LIMIT_SERVER_MAX", 120),

		MaxConcurrentConversions: envInt("MAX_CONCURRENT_CONVERSIONS", 3),
		MaxQueuedConversions:     envInt("MAX_QUEUED_CONVERSIONS", 50),
		MaxConcurrentMeta:        envInt("MAX_CONCURRENT_META", 6),
		ConversionTimeout:        time.Duration(envInt("CONVERSION_TIMEOUT_SECONDS", 300)) * time.Second,
		MaxPlaylistItems:         envInt("MAX_PLAYLIST_ITEMS", 500),

		YtdlpPath:        envStr("YTDLP_PATH", "yt-dlp"),
		YtdlpImpersonate: envStr("YTDLP_IMPERSONATE", "auto"),
		JSRuntime:        envStr("YTDLP_JS_RUNTIME", "node"),
		CookiesFile:      envStr("COOKIES_FILE", "./cookies/cookies.txt"),
		AudioQuality:     envInt("AUDIO_QUALITY", 5),
		AudioFormatSort:  envStr("AUDIO_FORMAT", "bestaudio[abr<=192]/bestaudio/best"),

		ClientTokensEnabled: envBool("CLIENT_TOKENS", true),
		ClientTokenMaxTTL:   time.Duration(envInt("CLIENT_TOKEN_MAX_TTL_SECONDS", 86400)) * time.Second,

		DevConsoleEnabled: envBool("DEV_CONSOLE", false),
		DevConsoleToken:   strings.TrimSpace(os.Getenv("DEV_CONSOLE_TOKEN")),

		SpotifyClientID:     strings.TrimSpace(os.Getenv("SPOTIFY_CLIENT_ID")),
		SpotifyClientSecret: strings.TrimSpace(os.Getenv("SPOTIFY_CLIENT_SECRET")),

		DataDir:  dataDir,
		CacheDir: cacheDir,
	}

	c.TmpDir = filepath.Join(cacheDir, ".tmp")
	c.DBPath = filepath.Join(dataDir, "history.db")
	c.RankLimitsPath = filepath.Join(dataDir, "rank-limits.json")
	c.YtdlpCacheDir = envStr("YTDLP_CACHE_DIR", filepath.Join(dataDir, "yt-dlp-cache"))

	if err := c.validate(); err != nil {
		return nil, err
	}

	for _, d := range []string{c.DataDir, c.CacheDir, c.TmpDir, c.YtdlpCacheDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	if err := c.ReloadRankLimits(); err != nil {
		slog.Warn("rank-limits.json could not be loaded, continuing without rank limits", "err", err)
	}
	return c, nil
}

func (c *Config) validate() error {
	if placeholderSecrets[c.APISecret] {
		return errors.New("API_SECRET is empty or still a placeholder value; set a long random secret in .env (e.g. `openssl rand -hex 32`)")
	}
	if len(c.APISecret) < 16 {
		slog.Warn("API_SECRET is shorter than 16 characters, consider a longer one")
	}
	if c.DevConsoleEnabled && (placeholderSecrets[c.DevConsoleToken] || len(c.DevConsoleToken) < 12) {
		slog.Warn("DEV_CONSOLE=1 but DEV_CONSOLE_TOKEN is missing, a placeholder or shorter than 12 chars; dev console disabled")
		c.DevConsoleEnabled = false
	}
	if c.AudioQuality < 0 || c.AudioQuality > 9 {
		return fmt.Errorf("AUDIO_QUALITY must be 0..9, got %d", c.AudioQuality)
	}
	if c.MaxConcurrentConversions < 1 {
		c.MaxConcurrentConversions = 1
	}
	if c.MaxConcurrentMeta < 1 {
		c.MaxConcurrentMeta = 1
	}
	if c.MaxQueuedConversions < 0 {
		c.MaxQueuedConversions = 0
	}
	if c.RateLimitWindow <= 0 {
		c.RateLimitWindow = time.Minute
	}
	if c.CacheDays < 1 {
		c.CacheDays = 1
	}
	if c.MaxDurationSeconds < 0 {
		c.MaxDurationSeconds = 0
	}
	if c.ConversionTimeout < 30*time.Second {
		c.ConversionTimeout = 30 * time.Second
	}
	return nil
}

func (c *Config) SpotifyEnabled() bool {
	return c.SpotifyClientID != "" && c.SpotifyClientSecret != ""
}

// ---- rank limits ---------------------------------------------------------

// ReloadRankLimits re-reads data/rank-limits.json. A missing file means no limits.
func (c *Config) ReloadRankLimits() error {
	raw, err := os.ReadFile(c.RankLimitsPath)
	limits := map[string]*RankLimit{}
	if err == nil {
		if err := json.Unmarshal(raw, &limits); err != nil {
			return fmt.Errorf("parse rank-limits.json: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for k := range limits {
		if !ValidRank(k) {
			delete(limits, k)
		}
	}
	c.mu.Lock()
	c.rankLimits = limits
	c.mu.Unlock()
	return nil
}

// RankLimits returns a copy suitable for JSON output. Explicit `null` entries
// (= unlimited) are rendered as zeroes so the dev console can display them.
func (c *Config) RankLimits() map[string]RankLimit {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]RankLimit, len(c.rankLimits))
	for k, v := range c.rankLimits {
		if v == nil {
			out[k] = RankLimit{}
		} else {
			out[k] = *v
		}
	}
	return out
}

// SaveRankLimits atomically writes the file and swaps the in-memory copy.
func (c *Config) SaveRankLimits(limits map[string]RankLimit) error {
	data, err := json.MarshalIndent(limits, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.RankLimitsPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.RankLimitsPath); err != nil {
		return err
	}
	next := make(map[string]*RankLimit, len(limits))
	for k, v := range limits {
		v := v
		next[k] = &v
	}
	c.mu.Lock()
	c.rankLimits = next
	c.mu.Unlock()
	return nil
}

// LimitsFor resolves the limits for a rank:
//   - rank configured        -> its own values (0 / negative = unlimited)
//   - rank configured `null` -> unlimited
//   - rank not configured    -> global MAX_DURATION_SECONDS, no daily cap
func (c *Config) LimitsFor(rank string) Effective {
	c.mu.RLock()
	lim, ok := c.rankLimits[rank]
	c.mu.RUnlock()
	if !ok {
		return Effective{MaxDurationSeconds: c.MaxDurationSeconds}
	}
	if lim == nil {
		return Effective{}
	}
	e := Effective{MaxDurationSeconds: lim.MaxDurationSeconds, DailyConversions: lim.DailyConversions}
	if e.MaxDurationSeconds < 0 {
		e.MaxDurationSeconds = 0
	}
	if e.DailyConversions < 0 {
		e.DailyConversions = 0
	}
	return e
}

// ---- env helpers ---------------------------------------------------------

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// envInt keeps an explicit 0 (the old code turned AUDIO_QUALITY=0 into 5).
func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		slog.Warn("invalid integer in env, using default", "key", key, "value", v, "default", def)
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// loadDotEnv sets variables from a KEY=VALUE file without overriding the
// real environment (docker compose env_file wins).
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}
