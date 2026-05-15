const Database = require('better-sqlite3');
const fs = require('fs');
const path = require('path');
const config = require('./config');

const dataDir = path.dirname(config.dbPath);
if (!fs.existsSync(dataDir)) {
  fs.mkdirSync(dataDir, { recursive: true });
}

const db = new Database(config.dbPath);
db.pragma('journal_mode = WAL');

db.exec(`
  CREATE TABLE IF NOT EXISTS history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    video_id TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    duration INTEGER NOT NULL DEFAULT 0,
    nick TEXT NOT NULL DEFAULT '',
    steamid TEXT NOT NULL DEFAULT '',
    server_ip TEXT NOT NULL DEFAULT '',
    cached INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
  );
  CREATE INDEX IF NOT EXISTS idx_history_created ON history(created_at);
  CREATE INDEX IF NOT EXISTS idx_history_video ON history(video_id);

  CREATE TABLE IF NOT EXISTS cache_entries (
    video_id TEXT PRIMARY KEY,
    title TEXT NOT NULL DEFAULT '',
    duration INTEGER NOT NULL DEFAULT 0,
    file_path TEXT NOT NULL,
    file_size INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    expires_at TEXT NOT NULL
  );

  CREATE TABLE IF NOT EXISTS spotify_cache (
    track_id TEXT PRIMARY KEY,
    video_id TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    artist TEXT NOT NULL DEFAULT '',
    duration INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
  );

  CREATE TABLE IF NOT EXISTS blocked_players (
    steamid TEXT PRIMARY KEY,
    reason TEXT NOT NULL DEFAULT '',
    blocked_at TEXT NOT NULL DEFAULT (datetime('now'))
  );
`);

// old dbs wont have this yet
try {
  db.exec("ALTER TABLE history ADD COLUMN source TEXT NOT NULL DEFAULT 'youtube'");
} catch (_) {}
try {
  db.exec("ALTER TABLE history ADD COLUMN rank TEXT NOT NULL DEFAULT 'default'");
} catch (_) {}

const stmtInsertHistory = db.prepare(`
  INSERT INTO history (video_id, title, duration, nick, steamid, server_ip, cached, source, rank)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
`);

const stmtRecentHistory = db.prepare(`
  SELECT id FROM history
  WHERE video_id = ? AND created_at > datetime('now', '-60 seconds')
  LIMIT 1
`);

const stmtDailyConversions = db.prepare(`
  SELECT COUNT(*) as count FROM history
  WHERE steamid = ? AND cached = 0 AND created_at > datetime('now', '-1 day')
`);

function addHistory({ videoId, title, duration, nick, steamid, serverIp, cached, source, rank }) {
  // same video spam within 60s is just noise
  const recent = stmtRecentHistory.get(videoId);
  if (recent) return;

  stmtInsertHistory.run(videoId, title || '', duration || 0, nick || '', steamid || '', serverIp || '', cached ? 1 : 0, source || 'youtube', rank || 'default');
}

function getDailyConversions(steamid) {
  if (!steamid) return 0;
  const row = stmtDailyConversions.get(steamid);
  return row ? row.count : 0;
}

const stmtGetHistory = db.prepare(`
  SELECT * FROM history ORDER BY created_at DESC LIMIT ? OFFSET ?
`);

const stmtCountHistory = db.prepare(`SELECT COUNT(*) as count FROM history`);

function getHistory(limit = 50, offset = 0) {
  const rows = stmtGetHistory.all(limit, offset);
  const { count } = stmtCountHistory.get();
  return { rows, total: count };
}

const stmtGetCache = db.prepare(`SELECT * FROM cache_entries WHERE video_id = ?`);
const stmtSetCache = db.prepare(`
  INSERT OR REPLACE INTO cache_entries (video_id, title, duration, file_path, file_size, created_at, expires_at)
  VALUES (?, ?, ?, ?, ?, datetime('now'), ?)
`);
const stmtDeleteCache = db.prepare(`DELETE FROM cache_entries WHERE video_id = ?`);
const stmtExpiredCache = db.prepare(`SELECT * FROM cache_entries WHERE expires_at < datetime('now') LIMIT 100`);

function getCacheEntry(videoId) {
  return stmtGetCache.get(videoId) || null;
}

function setCacheEntry({ videoId, title, duration, filePath, fileSize, expiresDays }) {
  const expiresAt = new Date(Date.now() + expiresDays * 86400000).toISOString().replace('T', ' ').slice(0, 19);
  stmtSetCache.run(videoId, title || '', duration || 0, filePath, fileSize || 0, expiresAt);
}

function deleteCacheEntry(videoId) {
  stmtDeleteCache.run(videoId);
}

function getExpiredCacheEntries() {
  return stmtExpiredCache.all();
}

const stmtGetSpotify = db.prepare(`SELECT * FROM spotify_cache WHERE track_id = ?`);
const stmtSetSpotify = db.prepare(`
  INSERT OR REPLACE INTO spotify_cache (track_id, video_id, title, artist, duration)
  VALUES (?, ?, ?, ?, ?)
`);

function getSpotifyCache(trackId) {
  return stmtGetSpotify.get(trackId) || null;
}

function setSpotifyCache({ trackId, videoId, title, artist, duration }) {
  stmtSetSpotify.run(trackId, videoId, title || '', artist || '', duration || 0);
}

const stmtBlockPlayer = db.prepare(`INSERT OR REPLACE INTO blocked_players (steamid, reason) VALUES (?, ?)`);
const stmtUnblockPlayer = db.prepare(`DELETE FROM blocked_players WHERE steamid = ?`);
const stmtIsBlocked = db.prepare(`SELECT 1 FROM blocked_players WHERE steamid = ? LIMIT 1`);
const stmtGetBlocked = db.prepare(`SELECT * FROM blocked_players ORDER BY blocked_at DESC`);

function blockPlayer(steamid, reason) {
  stmtBlockPlayer.run(steamid, reason || '');
}

function unblockPlayer(steamid) {
  stmtUnblockPlayer.run(steamid);
}

function isBlocked(steamid) {
  return !!stmtIsBlocked.get(steamid);
}

function getBlockedPlayers() {
  return stmtGetBlocked.all();
}

module.exports = {
  db,
  addHistory,
  getHistory,
  getCacheEntry,
  setCacheEntry,
  deleteCacheEntry,
  getExpiredCacheEntries,
  getSpotifyCache,
  setSpotifyCache,
  getDailyConversions,
  blockPlayer,
  unblockPlayer,
  isBlocked,
  getBlockedPlayers,
};
