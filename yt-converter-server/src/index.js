const express = require('express');
const rateLimit = require('express-rate-limit');
const path = require('path');
const fs = require('fs');
const config = require('./config');
const authMiddleware = require('./auth');
const converter = require('./converter');
const youtube = require('./youtube');
const spotify = require('./spotify');
const database = require('./database');
const { startCleanupInterval } = require('./cache');
const metrics = require('./metrics');
const { registerDevRoutes } = require('./dev');

const app = express();
app.use(express.json());

// request logger — logs method, path, IP, and any body fields useful for debugging
app.use((req, _res, next) => {
  const b = req.body || {};
  const extra = [];
  if (b.steamid !== undefined) extra.push(`steamid=${b.steamid}`);
  if (b.rank !== undefined) extra.push(`rank=${b.rank}`);
  if (b.nick !== undefined) extra.push(`nick=${b.nick}`);
  if (b.player_ip !== undefined) extra.push(`player_ip=${b.player_ip}`);
  if (b.url !== undefined) extra.push(`url=${String(b.url).slice(0, 80)}`);
  console.log(`[REQ] ${req.method} ${req.path} ip=${req.ip}${extra.length ? ' | ' + extra.join(' ') : ''}`);
  next();
});

app.use(express.static(path.join(__dirname, 'public')));

// if gmod sends server_ip, rate limit by that instead of proxy ip noise
const apiLimiter = rateLimit({
  windowMs: config.rateLimitWindowMs,
  max: config.rateLimitMax,
  standardHeaders: true,
  legacyHeaders: false,
  message: { error: 'Too many requests, please try again later' },
  keyGenerator: (req) => req.body?.server_ip || req.ip,
});

// one yt id at a time, everyone else waits
const conversionLocks = new Map();

// keep metrics updated with current lock count
function _syncConvLocks() { metrics.setActiveConversions(conversionLocks.size); }

function _validRank(r) {
  return (typeof r === 'string' && /^[a-zA-Z0-9_-]{1,32}$/.test(r)) ? r : 'default';
}

// accept player IP from body (sent by GMod server which knows the player's IP)
// only used for logging — not a security boundary
function _validPlayerIp(ip) {
  if (typeof ip !== 'string' || ip.length > 45) return null;
  // IPv4 — validate each octet is 0-255
  const v4 = ip.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
  if (v4) {
    return v4.slice(1).every(o => parseInt(o, 10) <= 255) ? ip : null;
  }
  // IPv6 — must contain at least one colon
  if (ip.includes(':') && /^[0-9a-fA-F:]{2,39}$/.test(ip)) return ip;
  return null;
}

// STEAM_0:0:12345678, STEAM_1:1:12345678, [U:1:12345], 76561198000000000
function _validSteamId(s) {
  if (typeof s !== 'string' || s.length > 25) return null;
  if (/^STEAM_[0-9]:[01]:\d{1,10}$/.test(s)) return s;
  if (/^\[U:1:\d{1,10}\]$/.test(s)) return s;
  if (/^7656119\d{10}$/.test(s)) return s;
  return null;
}

app.post('/api/convert', apiLimiter, authMiddleware, async (req, res) => {
  try {
    const { url, nick, steamid, server_ip } = req.body || {};
    const rank = _validRank(req.body?.rank);
    const playerIp = _validPlayerIp(req.body?.player_ip) || req.ip;
    const validSteamid = _validSteamId(steamid);
    // null limits = rank not configured = unlimited
    const limits = config.rankLimits[rank] || null;

    if (!url) {
      return res.status(400).json({ error: 'Missing "url" parameter' });
    }

    // block check — before any heavy work
    if (validSteamid && database.isBlocked(validSteamid)) {
      return res.status(403).json({ error: 'Blocked' });
    }

    let videoId = youtube.extractVideoId(url);
    let source = 'youtube';
    let resolvedTitle = null;
    let trackId = null;

    if (!videoId) {
      trackId = spotify.extractTrackId(url);
      if (!trackId) {
        return res.status(400).json({ error: 'Invalid URL. Supported: YouTube, Spotify' });
      }

      if (!spotify.isConfigured()) {
        return res.status(400).json({ error: 'Spotify not configured on this server' });
      }

      source = 'spotify';

      const spCached = database.getSpotifyCache(trackId);
      if (spCached) {
        videoId = spCached.video_id;
        resolvedTitle = spCached.title;
      } else {
        try {
          const resolved = await spotify.resolveToYouTube(trackId);
          videoId = resolved.videoId;
          resolvedTitle = resolved.title;
          database.setSpotifyCache({
            trackId,
            videoId: resolved.videoId,
            title: resolved.title,
            artist: resolved.artist,
            duration: resolved.spotifyDuration,
          });
        } catch (err) {
          console.error(`Spotify resolution failed for ${trackId}:`, err.message);
          return res.status(500).json({ error: 'Spotify resolution failed' });
        }
      }
    }

    const cached = database.getCacheEntry(videoId);
    if (cached) {
      const filePath = path.join(config.cacheDir, `${videoId}.mp3`);
      if (fs.existsSync(filePath)) {
        database.addHistory({
          videoId,
          title: resolvedTitle || cached.title,
          duration: cached.duration,
          nick: nick || '',
          steamid: validSteamid || '',
          serverIp: server_ip || '',
          cached: true,
          source,
          rank,
        });

        metrics.recordConversion({ videoId, title: resolvedTitle || cached.title, durationMs: 0, cached: true, source, nick, steamid: validSteamid, serverIp: server_ip, playerIp });
        return res.json({
          success: true,
          stream_url: `/stream/${videoId}.mp3`,
          video_id: videoId,
          ...(trackId && { track_id: trackId, source: 'spotify' }),
          title: resolvedTitle || cached.title,
          duration: cached.duration,
          cached: true,
        });
      } else {
        database.deleteCacheEntry(videoId);
      }
    }

    // same id already converting, piggyback
    if (conversionLocks.has(videoId)) {
      try {
        await conversionLocks.get(videoId);
      } catch (_) {}

      const retryCache = database.getCacheEntry(videoId);
      if (retryCache) {
        const filePath = path.join(config.cacheDir, `${videoId}.mp3`);
        if (fs.existsSync(filePath)) {
          database.addHistory({
            videoId,
            title: resolvedTitle || retryCache.title,
            duration: retryCache.duration,
            nick: nick || '',
            steamid: validSteamid || '',
            serverIp: server_ip || '',
            cached: true,
            source,
            rank,
          });
          metrics.recordConversion({ videoId, title: resolvedTitle || retryCache.title, durationMs: 0, cached: true, source, nick, steamid: validSteamid, serverIp: server_ip, playerIp });
          return res.json({
            success: true,
            stream_url: `/stream/${videoId}.mp3`,
            video_id: videoId,
            ...(trackId && { track_id: trackId, source: 'spotify' }),
            title: resolvedTitle || retryCache.title,
            duration: retryCache.duration,
            cached: true,
          });
        }
      }
    }

    // rank: daily conversion limit check
    // cache hits already returned above, so this only runs for fresh conversions
    if (validSteamid && limits && limits.dailyConversions > 0) {
      const todayCount = database.getDailyConversions(validSteamid);
      if (todayCount >= limits.dailyConversions) {
        return res.status(429).json({ error: `Daily conversion limit reached (${limits.dailyConversions}/day for rank "${rank}")` });
      }
    }

    const conversionPromise = (async () => {
      const t0 = Date.now();
      const { title, duration, filePath } = await converter.convertAudio(videoId, limits ? limits.maxDurationSeconds : null);
      const ytdlpMs = Date.now() - t0;
      const stats = fs.statSync(filePath);

      database.setCacheEntry({
        videoId,
        title,
        duration,
        filePath,
        fileSize: stats.size,
        expiresDays: config.cacheDays,
      });

      return { title, duration, ytdlpMs };
    })();

    conversionLocks.set(videoId, conversionPromise);
    _syncConvLocks();

    let result;
    try {
      result = await conversionPromise;
    } catch (err) {
      metrics.recordConversion({ videoId, error: err.message || String(err), source, nick, steamid, serverIp: server_ip, playerIp });
      if (err.status) {
        return res.status(err.status).json({ error: err.message });
      }
      console.error(`Conversion error for ${videoId}:`, err.message || err);
      return res.status(500).json({ error: 'Conversion failed' });
    } finally {
      // guard against deleting a new lock that started for the same id within 1s
      const ref = conversionPromise;
      setTimeout(() => { if (conversionLocks.get(videoId) === ref) { conversionLocks.delete(videoId); _syncConvLocks(); } }, 1000);
    }

    metrics.recordConversion({ videoId, title: resolvedTitle || result.title, ytdlpMs: result.ytdlpMs, cached: false, source, nick, steamid: validSteamid, serverIp: server_ip, playerIp });

    database.addHistory({
      videoId,
      title: resolvedTitle || result.title,
      duration: result.duration,
      nick: nick || '',
      steamid: validSteamid || '',
      serverIp: server_ip || '',
      cached: false,
      source,
      rank,
    });

    res.json({
      success: true,
      stream_url: `/stream/${videoId}.mp3`,
      video_id: videoId,
      ...(trackId && { track_id: trackId, source: 'spotify' }),
      title: resolvedTitle || result.title,
      duration: result.duration,
      cached: false,
    });
  } catch (err) {
    console.error('Unexpected error:', err);
    res.status(500).json({ error: 'Internal server error' });
  }
});

// metadata only
app.post('/api/info', apiLimiter, authMiddleware, async (req, res) => {
  try {
    const { url } = req.body || {};
    if (!url) return res.status(400).json({ error: 'Missing "url" parameter' });

    const trackId = spotify.extractTrackId(url);
    if (trackId) {
      if (!spotify.isConfigured()) {
        return res.status(400).json({ error: 'Spotify not configured on this server' });
      }

      const track = await spotify.getTrackInfo(trackId);
      const duration = track.duration;
      return res.json({
        source: 'spotify',
        track_id: trackId,
        title: `${track.artists} - ${track.name}`,
        artist: track.artists,
        track_name: track.name,
        duration,
        duration_formatted: `${Math.floor(duration / 60)}:${String(duration % 60).padStart(2, '0')}`,
        allowed: duration <= config.maxDurationSeconds,
        max_duration: config.maxDurationSeconds,
      });
    }

    const videoId = youtube.extractVideoId(url);
    if (!videoId) return res.status(400).json({ error: 'Invalid URL. Supported: YouTube, Spotify' });

    const info = await converter.getVideoInfo(videoId);
    const duration = Math.floor(info.duration || 0);

    res.json({
      source: 'youtube',
      video_id: videoId,
      title: info.title || '',
      duration,
      duration_formatted: `${Math.floor(duration / 60)}:${String(duration % 60).padStart(2, '0')}`,
      allowed: duration <= config.maxDurationSeconds,
      max_duration: config.maxDurationSeconds,
    });
  } catch (err) {
    console.error('Info error:', err.message || err);
    res.status(500).json({ error: 'Failed to fetch info' });
  }
});

// playlist metadata only, no conversion here
app.post('/api/playlist', apiLimiter, authMiddleware, async (req, res) => {
  try {
    const { url } = req.body || {};
    if (!url) return res.status(400).json({ error: 'Missing "url" parameter' });

    const spotifyPlaylistId = spotify.extractPlaylistId(url);
    if (spotifyPlaylistId) {
      if (!spotify.isConfigured()) {
        return res.status(400).json({ error: 'Spotify not configured on this server' });
      }

      const tracks = await spotify.getPlaylistTracks(spotifyPlaylistId);
      return res.json({
        source: 'spotify',
        playlist_id: spotifyPlaylistId,
        tracks: tracks.map(t => ({
          url: `spotify:track:${t.trackId}`,
          track_id: t.trackId,
          title: `${t.artists} - ${t.name}`,
          duration: t.duration,
        })),
      });
    }

    const ytPlaylistId = youtube.extractPlaylistId(url);
    if (ytPlaylistId) {
      const items = await converter.getPlaylistItems(`https://www.youtube.com/playlist?list=${ytPlaylistId}`);
      return res.json({
        source: 'youtube',
        playlist_id: ytPlaylistId,
        tracks: items.map(item => ({
          url: `https://www.youtube.com/watch?v=${item.videoId}`,
          video_id: item.videoId,
          title: item.title,
          duration: item.duration,
        })),
      });
    }

    return res.status(400).json({ error: 'Invalid playlist URL. Supported: YouTube playlist, Spotify playlist' });
  } catch (err) {
    console.error('Playlist error:', err.message || err);
    res.status(500).json({ error: 'Failed to fetch playlist' });
  }
});

app.get('/api/history', authMiddleware, (req, res) => {
  const page = Math.max(1, parseInt(req.query.page, 10) || 1);
  const limit = Math.min(100, Math.max(1, parseInt(req.query.limit, 10) || 50));
  const offset = (page - 1) * limit;

  const { rows, total } = database.getHistory(limit, offset);
  res.json({
    page,
    limit,
    total,
    totalPages: Math.ceil(total / limit),
    entries: rows,
  });
});

app.get('/api/health', (_req, res) => {
  res.json({ status: 'ok', uptime: process.uptime() });
});

// GMod server pushes its ULX usergroup list here so the dev console can show them
app.post('/api/ranks', authMiddleware, (req, res) => {
  const { ranks } = req.body || {};
  if (!Array.isArray(ranks)) return res.status(400).json({ error: 'ranks must be an array' });
  const clean = ranks.filter(r => typeof r === 'string' && /^[a-zA-Z0-9_-]{1,32}$/.test(r));
  app._knownRanks = clean;
  res.json({ ok: true, stored: clean.length });
});

// cached mp3 endpoint, range works
app.get('/stream/:filename', (req, res) => {
  const match = req.params.filename.match(/^([a-zA-Z0-9_-]{11})\.mp3$/);
  if (!match) {
    return res.status(400).json({ error: 'Invalid stream URL' });
  }

  const videoId = match[1];
  const filePath = path.join(config.cacheDir, `${videoId}.mp3`);

  if (!fs.existsSync(filePath)) {
    return res.status(404).json({ error: 'File not found or expired' });
  }

  const stat = fs.statSync(filePath);

  res.set({
    'Content-Type': 'audio/mpeg',
    'Content-Length': stat.size,
    'Accept-Ranges': 'bytes',
    'Cache-Control': 'public, max-age=86400',
  });

  metrics.incrementStream(videoId);
  res.on('close', () => metrics.decrementStream(videoId));

  const range = req.headers.range;
  if (range) {
    const parts = range.replace(/bytes=/, '').split('-');
    const start = parseInt(parts[0], 10);
    const end = parts[1] ? parseInt(parts[1], 10) : stat.size - 1;

    if (start >= stat.size || end >= stat.size) {
      res.status(416).set('Content-Range', `bytes */${stat.size}`).end();
      return;
    }

    res.status(206).set({
      'Content-Range': `bytes ${start}-${end}/${stat.size}`,
      'Content-Length': end - start + 1,
    });
    fs.createReadStream(filePath, { start, end }).pipe(res);
  } else {
    fs.createReadStream(filePath).pipe(res);
  }
});

registerDevRoutes(app);

startCleanupInterval();

app.listen(config.port, '0.0.0.0', () => {
  console.log(`YT Converter Server running on port ${config.port}`);
  if (spotify.isConfigured()) {
    console.log('Spotify support: enabled');
  } else {
    console.log('Spotify support: disabled (set SPOTIFY_CLIENT_ID and SPOTIFY_CLIENT_SECRET)');
  }
  if (config.apiSecret === 'dev-secret-change-in-production') {
    console.log('⚠ WARNING: Using default dev secret. Set API_SECRET in .env for production!');
  }
});
