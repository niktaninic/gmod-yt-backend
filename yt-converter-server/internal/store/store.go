// Package store wraps the SQLite database (history, cache index, spotify
// mapping, blocked players). The schema stays compatible with databases
// created by the previous Node.js version.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const tsLayout = "2006-01-02 15:04:05"

type Store struct {
	db *sql.DB

	blockedMu sync.RWMutex
	blocked   map[string]struct{}
}

type HistoryEntry struct {
	ID        int64  `json:"id"`
	VideoID   string `json:"video_id"`
	Title     string `json:"title"`
	Duration  int    `json:"duration"`
	Nick      string `json:"nick"`
	SteamID   string `json:"steamid"`
	ServerIP  string `json:"server_ip"`
	Cached    int    `json:"cached"`
	Source    string `json:"source"`
	Rank      string `json:"rank"`
	CreatedAt string `json:"created_at"`
}

type CacheEntry struct {
	VideoID   string `json:"video_id"`
	Title     string `json:"title"`
	Duration  int    `json:"duration"`
	FileSize  int64  `json:"file_size"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

type SpotifyEntry struct {
	TrackID  string
	VideoID  string
	Title    string
	Artist   string
	Duration int
}

type BlockedPlayer struct {
	SteamID   string `json:"steamid"`
	Reason    string `json:"reason"`
	BlockedAt string `json:"blocked_at"`
}

type PlayerStat struct {
	Nick             string `json:"nick"`
	SteamID          string `json:"steamid"`
	Plays            int    `json:"plays"`
	NewConversions   int    `json:"new_conversions"`
	DailyConversions int    `json:"daily_conversions"`
	Rank             string `json:"rank"`
	LastSeen         string `json:"last_seen"`
}

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a small pool is plenty and avoids lock churn.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(0)

	s := &Store{db: db, blocked: map[string]struct{}{}}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := s.loadBlocked(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			video_id TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			duration INTEGER NOT NULL DEFAULT 0,
			nick TEXT NOT NULL DEFAULT '',
			steamid TEXT NOT NULL DEFAULT '',
			server_ip TEXT NOT NULL DEFAULT '',
			cached INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS cache_entries (
			video_id TEXT PRIMARY KEY,
			title TEXT NOT NULL DEFAULT '',
			duration INTEGER NOT NULL DEFAULT 0,
			file_path TEXT NOT NULL,
			file_size INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			expires_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS spotify_cache (
			track_id TEXT PRIMARY KEY,
			video_id TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			artist TEXT NOT NULL DEFAULT '',
			duration INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS blocked_players (
			steamid TEXT PRIMARY KEY,
			reason TEXT NOT NULL DEFAULT '',
			blocked_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}

	// columns added over time; old databases may not have them yet
	addCols := []struct{ table, col, def string }{
		{"history", "source", "TEXT NOT NULL DEFAULT 'youtube'"},
		{"history", "rank", "TEXT NOT NULL DEFAULT 'default'"},
		{"cache_entries", "last_access", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, c := range addCols {
		ok, err := s.hasColumn(c.table, c.col)
		if err != nil {
			return err
		}
		if !ok {
			if _, err := s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.col, c.def)); err != nil {
				return err
			}
		}
	}

	idx := []string{
		`CREATE INDEX IF NOT EXISTS idx_history_created ON history(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_history_video ON history(video_id)`,
		// daily-limit + leaderboard queries used to full-scan the table
		`CREATE INDEX IF NOT EXISTS idx_history_steamid_created ON history(steamid, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_cache_expires ON cache_entries(expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_cache_access ON cache_entries(last_access)`,
	}
	for _, q := range idx {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) hasColumn(table, col string) (bool, error) {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if strings.EqualFold(name, col) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func now() string { return time.Now().UTC().Format(tsLayout) }

// ---- history -------------------------------------------------------------

// AddHistory inserts a row unless the same player played the same video in
// the last 60 seconds (the old code deduped globally, hiding other players).
func (s *Store) AddHistory(ctx context.Context, h HistoryEntry) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO history (video_id, title, duration, nick, steamid, server_ip, cached, source, rank, created_at)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM history
			WHERE video_id = ? AND steamid = ? AND created_at > datetime('now', '-60 seconds')
		)`,
		h.VideoID, h.Title, h.Duration, h.Nick, h.SteamID, h.ServerIP, h.Cached, h.Source, h.Rank, now(),
		h.VideoID, h.SteamID)
	return err
}

// DailyConversions counts fresh (non-cached) conversions in the last 24h.
func (s *Store) DailyConversions(ctx context.Context, steamid string) (int, error) {
	if steamid == "" {
		return 0, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM history
		WHERE steamid = ? AND created_at > datetime('now', '-1 day') AND cached = 0`, steamid).Scan(&n)
	return n, err
}

func (s *Store) History(ctx context.Context, limit, offset int) ([]HistoryEntry, int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, video_id, title, duration, nick, steamid, server_ip, cached, source, rank, created_at
		FROM history ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]HistoryEntry, 0, limit)
	for rows.Next() {
		var h HistoryEntry
		if err := rows.Scan(&h.ID, &h.VideoID, &h.Title, &h.Duration, &h.Nick, &h.SteamID, &h.ServerIP, &h.Cached, &h.Source, &h.Rank, &h.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM history`).Scan(&total); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (s *Store) PlayerStats(ctx context.Context) ([]PlayerStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH agg AS (
			SELECT steamid,
			       COUNT(*) AS plays,
			       SUM(cached = 0) AS new_conversions,
			       SUM(cached = 0 AND created_at > datetime('now', '-1 day')) AS daily_conversions,
			       MAX(created_at) AS last_seen,
			       MAX(id) AS last_id
			FROM history
			WHERE steamid != ''
			GROUP BY steamid
			ORDER BY plays DESC
			LIMIT 25
		)
		SELECT h.nick, agg.steamid, agg.plays, agg.new_conversions, agg.daily_conversions, h.rank, agg.last_seen
		FROM agg JOIN history h ON h.id = agg.last_id
		ORDER BY agg.plays DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlayerStat{}
	for rows.Next() {
		var p PlayerStat
		if err := rows.Scan(&p.Nick, &p.SteamID, &p.Plays, &p.NewConversions, &p.DailyConversions, &p.Rank, &p.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) DeletePlayerHistory(ctx context.Context, steamid string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM history WHERE steamid = ?`, steamid)
	return err
}

func (s *Store) PruneHistory(ctx context.Context, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM history WHERE created_at < datetime('now', ?)`, fmt.Sprintf("-%d days", days))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- cache index ---------------------------------------------------------

func (s *Store) GetCache(ctx context.Context, videoID string) (*CacheEntry, error) {
	var c CacheEntry
	err := s.db.QueryRowContext(ctx, `
		SELECT video_id, title, duration, file_size, created_at, expires_at
		FROM cache_entries WHERE video_id = ? AND expires_at > ?`, videoID, now()).
		Scan(&c.VideoID, &c.Title, &c.Duration, &c.FileSize, &c.CreatedAt, &c.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) PutCache(ctx context.Context, c CacheEntry, filePath string, ttl time.Duration) error {
	t := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO cache_entries (video_id, title, duration, file_path, file_size, created_at, expires_at, last_access)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.VideoID, c.Title, c.Duration, filePath, c.FileSize, t.Format(tsLayout), t.Add(ttl).Format(tsLayout), t.Unix())
	return err
}

// TouchCache slides the expiry on every hit, so popular tracks never expire
// (old behaviour: re-download every CACHE_DAYS even if played daily).
func (s *Store) TouchCache(ctx context.Context, videoID string, ttl time.Duration) error {
	t := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE cache_entries SET expires_at = ?, last_access = ? WHERE video_id = ?`,
		t.Add(ttl).Format(tsLayout), t.Unix(), videoID)
	return err
}

func (s *Store) DeleteCache(ctx context.Context, videoID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cache_entries WHERE video_id = ?`, videoID)
	return err
}

func (s *Store) ExpiredCacheIDs(ctx context.Context, limit int) ([]string, error) {
	return s.ids(ctx, `SELECT video_id FROM cache_entries WHERE expires_at <= ? LIMIT ?`, now(), limit)
}

// LRUCacheIDs returns least recently used entries first.
func (s *Store) LRUCacheIDs(ctx context.Context, limit int) ([]string, error) {
	return s.ids(ctx, `SELECT video_id FROM cache_entries ORDER BY CASE WHEN last_access = 0 THEN strftime('%s', created_at) ELSE last_access END ASC LIMIT ?`, limit)
}

func (s *Store) AllCacheIDs(ctx context.Context) (map[string]struct{}, error) {
	ids, err := s.ids(ctx, `SELECT video_id FROM cache_entries`)
	if err != nil {
		return nil, err
	}
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m, nil
}

func (s *Store) CacheTotalSize(ctx context.Context) (int64, int, error) {
	var size sql.NullInt64
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(file_size), 0), COUNT(*) FROM cache_entries`).Scan(&size, &n)
	return size.Int64, n, err
}

func (s *Store) ListCache(ctx context.Context, limit int) ([]CacheEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT video_id, title, duration, file_size, created_at, expires_at
		FROM cache_entries ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CacheEntry{}
	for rows.Next() {
		var c CacheEntry
		if err := rows.Scan(&c.VideoID, &c.Title, &c.Duration, &c.FileSize, &c.CreatedAt, &c.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ids(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---- spotify -------------------------------------------------------------

func (s *Store) GetSpotify(ctx context.Context, trackID string) (*SpotifyEntry, error) {
	var e SpotifyEntry
	err := s.db.QueryRowContext(ctx, `SELECT track_id, video_id, title, artist, duration FROM spotify_cache WHERE track_id = ?`, trackID).
		Scan(&e.TrackID, &e.VideoID, &e.Title, &e.Artist, &e.Duration)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) PutSpotify(ctx context.Context, e SpotifyEntry) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO spotify_cache (track_id, video_id, title, artist, duration, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, e.TrackID, e.VideoID, e.Title, e.Artist, e.Duration, now())
	return err
}

// ---- blocked players (mirrored in memory, checked on every request) -------

func (s *Store) loadBlocked() error {
	ids, err := s.ids(context.Background(), `SELECT steamid FROM blocked_players`)
	if err != nil {
		return err
	}
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	s.blockedMu.Lock()
	s.blocked = m
	s.blockedMu.Unlock()
	return nil
}

func (s *Store) IsBlocked(steamid string) bool {
	if steamid == "" {
		return false
	}
	s.blockedMu.RLock()
	_, ok := s.blocked[steamid]
	s.blockedMu.RUnlock()
	return ok
}

func (s *Store) Block(ctx context.Context, steamid, reason string) error {
	if _, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO blocked_players (steamid, reason, blocked_at) VALUES (?, ?, ?)`, steamid, reason, now()); err != nil {
		return err
	}
	s.blockedMu.Lock()
	s.blocked[steamid] = struct{}{}
	s.blockedMu.Unlock()
	return nil
}

func (s *Store) Unblock(ctx context.Context, steamid string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM blocked_players WHERE steamid = ?`, steamid); err != nil {
		return err
	}
	s.blockedMu.Lock()
	delete(s.blocked, steamid)
	s.blockedMu.Unlock()
	return nil
}

func (s *Store) ListBlocked(ctx context.Context) ([]BlockedPlayer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT steamid, reason, blocked_at FROM blocked_players ORDER BY blocked_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BlockedPlayer{}
	for rows.Next() {
		var b BlockedPlayer
		if err := rows.Scan(&b.SteamID, &b.Reason, &b.BlockedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Optimize runs PRAGMA optimize; cheap, call periodically.
func (s *Store) Optimize(ctx context.Context) {
	_, _ = s.db.ExecContext(ctx, `PRAGMA optimize`)
}
