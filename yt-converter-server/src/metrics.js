const os = require('os');

// in-memory ring buffer for recent conversions
const MAX_EVENTS = 500;
const _events = [];

// running totals
const totals = {
  conversions: 0,
  cacheHits: 0,
  errors: 0,
  spotifyResolves: 0,
  apiCalls: 0,
};

const _startTime = Date.now();

function pushEvent(ev) {
  _events.push({ ...ev, ts: Date.now() });
  if (_events.length > MAX_EVENTS) _events.shift();
}

function recordConversion({ videoId, title, durationMs, ytdlpMs, cached, error, source, nick, steamid, serverIp, playerIp }) {
  totals.apiCalls++;

  if (error) {
    totals.errors++;
    pushEvent({ type: 'error', videoId, title, error, source, nick, steamid, serverIp, playerIp, durationMs: durationMs || 0 });
    return;
  }

  if (cached) {
    totals.cacheHits++;
    pushEvent({ type: 'cache_hit', videoId, title, source, nick, steamid, serverIp, playerIp, durationMs: durationMs || 0 });
  } else {
    totals.conversions++;
    if (source === 'spotify') totals.spotifyResolves++;
    pushEvent({ type: 'conversion', videoId, title, source, nick, steamid, serverIp, playerIp, durationMs, ytdlpMs });
  }
}

function recordApiCall(endpoint) {
  totals.apiCalls++;
}

function getSystemStats() {
  const cpus = os.cpus();
  const cpuModel = cpus[0]?.model || 'unknown';
  const cpuCount = cpus.length;

  // rough cpu usage: diff idle/total between two calls is expensive; use load avg instead
  const loadAvg = os.loadavg();

  const totalMem = os.totalmem();
  const freeMem = os.freemem();
  const usedMem = totalMem - freeMem;
  const procMem = process.memoryUsage();

  return {
    uptime: Math.floor((Date.now() - _startTime) / 1000),
    processUptime: Math.floor(process.uptime()),
    cpu: {
      model: cpuModel,
      cores: cpuCount,
      loadAvg1: loadAvg[0].toFixed(2),
      loadAvg5: loadAvg[1].toFixed(2),
      loadAvg15: loadAvg[2].toFixed(2),
    },
    memory: {
      totalMB: Math.round(totalMem / 1024 / 1024),
      usedMB: Math.round(usedMem / 1024 / 1024),
      freeMB: Math.round(freeMem / 1024 / 1024),
      processRssMB: Math.round(procMem.rss / 1024 / 1024),
      processHeapMB: Math.round(procMem.heapUsed / 1024 / 1024),
    },
  };
}

function getPlayerStats(db) {
  try {
    const rows = db.prepare(`
      SELECT nick, steamid,
             COUNT(*) as plays,
             SUM(CASE WHEN cached=0 THEN 1 ELSE 0 END) as new_conversions,
             SUM(CASE WHEN cached=0 AND created_at > datetime('now', '-1 day') THEN 1 ELSE 0 END) as daily_conversions,
             (SELECT rank FROM history h2 WHERE h2.steamid = history.steamid ORDER BY h2.created_at DESC LIMIT 1) as rank,
             MAX(created_at) as last_seen
      FROM history
      WHERE nick != ''
      GROUP BY steamid
      ORDER BY plays DESC
      LIMIT 25
    `).all();
    return rows;
  } catch (_) {
    return [];
  }
}

function getRecentEvents(limit = 100) {
  return _events.slice(-limit).reverse();
}

function getTotals() {
  return { ...totals };
}

// avg ytdlp time from last N conversions in buffer
function getAvgConversionTime() {
  const convs = _events.filter(e => e.type === 'conversion' && e.ytdlpMs);
  if (!convs.length) return null;
  const recent = convs.slice(-50);
  const avg = recent.reduce((a, b) => a + b.ytdlpMs, 0) / recent.length;
  return Math.round(avg);
}

// active streams: people currently pulling /stream/:filename
const _activeStreams = new Map(); // videoId -> count
let _totalActiveStreams = 0;

// active conversions counter (set externally from index.js)
let _activeConversions = 0;

function incrementStream(videoId) {
  _activeStreams.set(videoId, (_activeStreams.get(videoId) || 0) + 1);
  _totalActiveStreams++;
}

function decrementStream(videoId) {
  const cur = _activeStreams.get(videoId) || 0;
  if (cur <= 1) _activeStreams.delete(videoId);
  else _activeStreams.set(videoId, cur - 1);
  if (_totalActiveStreams > 0) _totalActiveStreams--;
}

function setActiveConversions(n) {
  _activeConversions = n;
}

function getActiveStreams() {
  return { total: _totalActiveStreams, activeConversions: _activeConversions };
}

module.exports = {
  recordConversion,
  recordApiCall,
  getSystemStats,
  getPlayerStats,
  getRecentEvents,
  getTotals,
  getAvgConversionTime,
  incrementStream,
  decrementStream,
  setActiveConversions,
  getActiveStreams,
};
