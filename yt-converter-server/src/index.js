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

const app = express();
app.use(express.json());

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
app.post('/api/convert', apiLimiter, authMiddleware, async (req, res) => {
  try {
    const { url, nick, steamid, server_ip } = req.body || {};

    if (!url) {
      return res.status(400).json({ error: 'Missing "url" parameter' });
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
          steamid: steamid || '',
          serverIp: server_ip || '',
          cached: true,
          source,
        });

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
            steamid: steamid || '',
            serverIp: server_ip || '',
            cached: true,
            source,
          });
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

    const conversionPromise = (async () => {
      const info = await converter.getVideoInfo(videoId);
      const duration = Math.floor(info.duration || 0);
      const title = info.title || videoId;

      if (duration > config.maxDurationSeconds) {
        const maxMin = Math.floor(config.maxDurationSeconds / 60);
        const vidMin = Math.floor(duration / 60);
        const vidSec = duration % 60;
        throw {
          status: 400,
          message: `Video too long (${vidMin}:${String(vidSec).padStart(2, '0')}). Maximum allowed: ${maxMin} minutes.`,
        };
      }

      const filePath = await converter.downloadAudio(videoId);
      const stats = fs.statSync(filePath);

      database.setCacheEntry({
        videoId,
        title,
        duration,
        filePath,
        fileSize: stats.size,
        expiresDays: config.cacheDays,
      });

      return { title, duration };
    })();

    conversionLocks.set(videoId, conversionPromise);

    let result;
    try {
      result = await conversionPromise;
    } catch (err) {
      conversionLocks.delete(videoId);
      if (err.status) {
        return res.status(err.status).json({ error: err.message });
      }
      console.error(`Conversion error for ${videoId}:`, err.message || err);
      return res.status(500).json({ error: 'Conversion failed' });
    } finally {
      setTimeout(() => conversionLocks.delete(videoId), 1000);
    }

    database.addHistory({
      videoId,
      title: resolvedTitle || result.title,
      duration: result.duration,
      nick: nick || '',
      steamid: steamid || '',
      serverIp: server_ip || '',
      cached: false,
      source,
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

app.get('/api/history', (req, res) => {
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
