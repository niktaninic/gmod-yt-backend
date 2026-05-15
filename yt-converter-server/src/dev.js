const crypto = require('crypto');
const path = require('path');
const fs = require('fs');
const http = require('http');
const config = require('./config');
const metrics = require('./metrics');
const database = require('./database');
const converter = require('./converter');

// in-memory sessions: sessionToken -> expiry ms
const _sessions = new Map();
const _SESSION_TTL_MS = 8 * 60 * 60 * 1000;

function _createSession() {
  const token = crypto.randomBytes(32).toString('hex');
  _sessions.set(token, Date.now() + _SESSION_TTL_MS);
  return token;
}

function _isValidSession(token) {
  if (!token || !_sessions.has(token)) return false;
  const exp = _sessions.get(token);
  if (Date.now() > exp) { _sessions.delete(token); return false; }
  return true;
}

function _parseCookies(req) {
  const out = {};
  for (const part of (req.headers.cookie || '').split(';')) {
    const eq = part.indexOf('=');
    if (eq < 1) continue;
    try { out[decodeURIComponent(part.slice(0, eq).trim())] = decodeURIComponent(part.slice(eq + 1).trim()); } catch (_) {}
  }
  return out;
}

function _sessionFromReq(req) {
  return _parseCookies(req)['_dcs'] || null;
}

function devAuthMiddleware(req, res, next) {
  if (!config.devConsoleEnabled) return res.status(404).send('Not found');
  if (_isValidSession(_sessionFromReq(req))) return next();
  return res.status(401).json({ error: 'Unauthorized' });
}

const _LOGIN_HTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
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
</html>`;

function registerDevRoutes(app) {
  if (!config.devConsoleEnabled) return;

  // login / logout
  app.get('/dev/login', (req, res) => {
    if (!config.devConsoleEnabled) return res.status(404).send('Not found');
    if (_isValidSession(_sessionFromReq(req))) return res.redirect('/dev');
    res.send(_LOGIN_HTML.replace('{{ERR}}', ''));
  });

  app.post('/dev/login', (req, res) => {
    if (!config.devConsoleEnabled) return res.status(404).send('Not found');
    let body = '';
    req.on('data', chunk => { body += chunk; if (body.length > 2048) req.destroy(); });
    req.on('end', () => {
      let token;
      try { token = new URLSearchParams(body).get('token') || ''; } catch (_) { token = ''; }
      if (token && config.devConsoleToken) {
        try {
          const a = Buffer.from(token);
          const b = Buffer.from(config.devConsoleToken);
          if (a.length === b.length && crypto.timingSafeEqual(a, b)) {
            const sess = _createSession();
            res.setHeader('Set-Cookie', `_dcs=${sess}; HttpOnly; SameSite=Strict; Path=/dev; Max-Age=${_SESSION_TTL_MS / 1000}`);
            return res.redirect('/dev');
          }
        } catch (_) {}
      }
      res.send(_LOGIN_HTML.replace('{{ERR}}', '<div class="err">invalid token</div>'));
    });
  });

  app.get('/dev/logout', (req, res) => {
    const sess = _sessionFromReq(req);
    if (sess) _sessions.delete(sess);
    res.setHeader('Set-Cookie', '_dcs=; HttpOnly; SameSite=Strict; Path=/dev; Max-Age=0');
    res.redirect('/dev/login');
  });

  // serve the console UI
  app.get('/dev', (req, res) => {
    if (!config.devConsoleEnabled) return res.status(404).send('Not found');
    if (!_isValidSession(_sessionFromReq(req))) return res.redirect('/dev/login');
    res.sendFile(path.join(__dirname, 'public', 'dev.html'));
  });

  // system stats
  app.get('/dev/api/stats', devAuthMiddleware, (req, res) => {
    const active = metrics.getActiveStreams();
    res.json({
      system: metrics.getSystemStats(),
      totals: metrics.getTotals(),
      avgConversionMs: metrics.getAvgConversionTime(),
      activeStreams: active.total,
      activeConversions: active.activeConversions,
      rankLimits: config.rankLimits,
      // merge GMod-pushed ranks with any ranks already in rank-limits.json
      // so the panel is useful even before GMod connects
      availableRanks: (() => {
        const known = new Set(app._knownRanks || []);
        for (const k of Object.keys(config.rankLimits)) known.add(k);
        return [...known].sort();
      })(),
    });
  });

  // recent event log
  app.get('/dev/api/events', devAuthMiddleware, (req, res) => {
    const limit = Math.min(500, parseInt(req.query.limit, 10) || 100);
    res.json(metrics.getRecentEvents(limit));
  });

  // player leaderboard
  app.get('/dev/api/players', devAuthMiddleware, (req, res) => {
    res.json(metrics.getPlayerStats(database.db));
  });

  // delete all history rows for a specific player
  app.delete('/dev/api/players/:steamid', devAuthMiddleware, (req, res) => {
    const steamid = decodeURIComponent(req.params.steamid);
    if (!steamid) return res.status(400).json({ error: 'steamid required' });
    database.db.prepare(`DELETE FROM history WHERE steamid = ?`).run(steamid);
    res.json({ ok: true });
  });

  // cache listing
  app.get('/dev/api/cache', devAuthMiddleware, (req, res) => {
    const rows = database.db.prepare(
      `SELECT video_id, title, duration, file_size, created_at, expires_at FROM cache_entries ORDER BY created_at DESC LIMIT 200`
    ).all();
    const cacheDir = config.cacheDir;
    let totalBytes = 0;
    try {
      for (const f of fs.readdirSync(cacheDir)) {
        if (f.endsWith('.mp3')) {
          try { totalBytes += fs.statSync(path.join(cacheDir, f)).size; } catch (_) {}
        }
      }
    } catch (_) {}
    res.json({ entries: rows, totalSizeMB: (totalBytes / 1024 / 1024).toFixed(2) });
  });

  // delete single cache entry
  app.delete('/dev/api/cache/:videoId', devAuthMiddleware, (req, res) => {
    const { videoId } = req.params;
    if (!/^[a-zA-Z0-9_-]{11}$/.test(videoId)) {
      return res.status(400).json({ error: 'invalid video id' });
    }
    const filePath = path.join(config.cacheDir, `${videoId}.mp3`);
    try { fs.unlinkSync(filePath); } catch (_) {}
    database.deleteCacheEntry(videoId);
    res.json({ ok: true });
  });

  // purge all cache
  app.delete('/dev/api/cache', devAuthMiddleware, (req, res) => {
    const rows = database.db.prepare(`SELECT video_id FROM cache_entries`).all();
    let removed = 0;
    for (const { video_id } of rows) {
      try { fs.unlinkSync(path.join(config.cacheDir, `${video_id}.mp3`)); } catch (_) {}
      database.deleteCacheEntry(video_id);
      removed++;
    }
    res.json({ ok: true, removed });
  });

  // rank limits — GET / PUT / DELETE per rank
  app.get('/dev/api/rank-limits', devAuthMiddleware, (req, res) => {
    res.json({ limits: config.rankLimits });
  });

  app.put('/dev/api/rank-limits', devAuthMiddleware, (req, res) => {
    const { limits } = req.body || {};
    if (!limits || typeof limits !== 'object' || Array.isArray(limits)) {
      return res.status(400).json({ error: 'body must be {limits:{...}}' });
    }
    for (const [rank, lim] of Object.entries(limits)) {
      if (!/^[a-zA-Z0-9_-]{1,32}$/.test(rank)) {
        return res.status(400).json({ error: `invalid rank name: ${rank}` });
      }
      const dur = parseInt(lim.maxDurationSeconds, 10);
      const daily = parseInt(lim.dailyConversions, 10);
      if (!Number.isFinite(dur) || dur < 0) {
        return res.status(400).json({ error: `${rank}: maxDurationSeconds must be >= 0 (0 = unlimited)` });
      }
      if (!Number.isFinite(daily) || daily < -1) {
        return res.status(400).json({ error: `${rank}: dailyConversions must be >= -1` });
      }
    }
    const cleaned = {};
    for (const [rank, lim] of Object.entries(limits)) {
      cleaned[rank] = {
        maxDurationSeconds: parseInt(lim.maxDurationSeconds, 10),
        dailyConversions: parseInt(lim.dailyConversions, 10),
      };
    }
    try {
      fs.writeFileSync(config.rankLimitsPath, JSON.stringify(cleaned, null, 2), 'utf8');
    } catch (e) {
      return res.status(500).json({ error: 'failed to write rank-limits.json: ' + e.message });
    }
    config.reloadRankLimits();
    res.json({ ok: true, limits: config.rankLimits });
  });

  app.delete('/dev/api/rank-limits/:rank', devAuthMiddleware, (req, res) => {
    const { rank } = req.params;
    if (!/^[a-zA-Z0-9_-]{1,32}$/.test(rank)) return res.status(400).json({ error: 'invalid rank name' });
    const next = { ...config.rankLimits };
    delete next[rank];
    try {
      fs.writeFileSync(config.rankLimitsPath, JSON.stringify(next, null, 2), 'utf8');
    } catch (e) {
      return res.status(500).json({ error: 'failed to write rank-limits.json: ' + e.message });
    }
    config.reloadRankLimits();
    res.json({ ok: true, limits: config.rankLimits });
  });

  // blocked players
  app.get('/dev/api/blocked', devAuthMiddleware, (_req, res) => {
    res.json(database.getBlockedPlayers());
  });

  app.post('/dev/api/blocked', devAuthMiddleware, (req, res) => {
    const { steamid, reason } = req.body || {};
    if (typeof steamid !== 'string' || steamid.length === 0) {
      return res.status(400).json({ error: 'steamid required' });
    }
    // allow any non-empty string — admin may block non-standard IDs too
    if (steamid.length > 50) return res.status(400).json({ error: 'steamid too long' });
    database.blockPlayer(steamid, reason || '');
    res.json({ ok: true });
  });

  app.delete('/dev/api/blocked/:steamid', devAuthMiddleware, (req, res) => {
    const steamid = decodeURIComponent(req.params.steamid);
    database.unblockPlayer(steamid);
    res.json({ ok: true });
  });

  // test conversion endpoint — streams log lines as SSE
  app.get('/dev/api/convert-test', devAuthMiddleware, async (req, res) => {
    const url = req.query.url;
    if (!url) return res.status(400).send('missing url');

    res.set({
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache',
      'Connection': 'keep-alive',
    });
    res.flushHeaders();

    const send = (type, data) => {
      res.write(`event: ${type}\ndata: ${JSON.stringify(data)}\n\n`);
    };

    send('log', { msg: `Starting conversion: ${url}` });
    send('log', { msg: `Timestamp: ${new Date().toISOString()}` });

    const ytMod = require('./youtube');
    const spotifyMod = require('./spotify');

    let videoId = ytMod.extractVideoId(url);
    let source = 'youtube';

    if (!videoId) {
      const trackId = spotifyMod.extractTrackId(url);
      if (!trackId) {
        send('log', { msg: 'ERROR: Could not extract video/track ID from URL' });
        send('done', { ok: false });
        return res.end();
      }
      source = 'spotify';
      send('log', { msg: `Detected Spotify track: ${trackId}` });

      const t0 = Date.now();
      try {
        send('log', { msg: 'Fetching Spotify track info...' });
        const resolved = await spotifyMod.resolveToYouTube(trackId);
        videoId = resolved.videoId;
        send('log', { msg: `Spotify resolved in ${Date.now() - t0}ms` });
        send('log', { msg: `YouTube ID: ${videoId} | Title: ${resolved.title}` });
      } catch (err) {
        send('log', { msg: `ERROR Spotify resolve: ${err.message}` });
        send('done', { ok: false });
        return res.end();
      }
    } else {
      send('log', { msg: `Detected YouTube video: ${videoId}` });
    }

    // check cache
    const cached = database.getCacheEntry(videoId);
    if (cached) {
      const fp = path.join(config.cacheDir, `${videoId}.mp3`);
      const exists = fs.existsSync(fp);
      send('log', { msg: `Cache hit: ${cached.title} | Exists on disk: ${exists}` });
      send('log', { msg: `Expires: ${cached.expires_at} | Size: ${(cached.file_size / 1024).toFixed(1)} KB` });
      send('done', { ok: true, cached: true, videoId, title: cached.title });
      return res.end();
    }

    send('log', { msg: 'Not cached. Running yt-dlp...' });
    const t1 = Date.now();

    try {
      send('log', { msg: `yt-dlp args: --match-filter duration<=${config.maxDurationSeconds}, audio-quality=${config.audioQuality}, concurrent-fragments=4` });
      const result = await converter.convertAudio(videoId, config.maxDurationSeconds);
      const elapsed = Date.now() - t1;
      const stats = fs.statSync(result.filePath);

      database.setCacheEntry({
        videoId,
        title: result.title,
        duration: result.duration,
        filePath: result.filePath,
        fileSize: stats.size,
        expiresDays: config.cacheDays,
      });

      send('log', { msg: `yt-dlp finished in ${elapsed}ms` });
      send('log', { msg: `Title: ${result.title}` });
      send('log', { msg: `Duration: ${result.duration}s | File size: ${(stats.size / 1024).toFixed(1)} KB` });
      send('log', { msg: `Saved to: ${result.filePath}` });
      send('done', { ok: true, cached: false, videoId, title: result.title, durationMs: elapsed });
    } catch (err) {
      const elapsed = Date.now() - t1;
      send('log', { msg: `ERROR after ${elapsed}ms: ${err.message || String(err)}` });
      send('done', { ok: false, error: err.message || String(err) });
    }

    res.end();
  });

  // simulate a full GMod client request → POST /api/convert
  app.post('/dev/api/simulate', devAuthMiddleware, (req, res) => {
    const { url, nick, steamid, server_ip, rank } = req.body || {};
    if (!url) return res.status(400).json({ error: 'missing url' });

    const body = JSON.stringify({ url, nick: nick || '', steamid: steamid || '', server_ip: server_ip || '', rank: rank || 'default' });

    const options = {
      hostname: '127.0.0.1',
      port: config.port,
      path: '/api/convert',
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Content-Length': Buffer.byteLength(body),
        'X-SR-Key': config.apiSecret,
        'X-Forwarded-For': req.ip,
      },
    };

    const t0 = Date.now();
    const proxyReq = http.request(options, (proxyRes) => {
      let data = '';
      proxyRes.on('data', chunk => { data += chunk; });
      proxyRes.on('end', () => {
        let parsed;
        try { parsed = JSON.parse(data); } catch (_) { parsed = { raw: data }; }
        res.json({ status: proxyRes.statusCode, durationMs: Date.now() - t0, response: parsed });
      });
    });

    proxyReq.on('error', err => res.status(500).json({ error: err.message }));
    proxyReq.write(body);
    proxyReq.end();
  });

  console.log('Dev console enabled at /dev (login at /dev/login)');
}

module.exports = { registerDevRoutes, devAuthMiddleware };
