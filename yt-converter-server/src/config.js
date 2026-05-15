const path = require('path');
const fs = require('fs');

const envPath = path.join(__dirname, '..', '.env');
require('./env')(envPath);

const rankLimitsPath = path.join(__dirname, '..', 'data', 'rank-limits.json');

function _loadRankLimitsFile() {
  try {
    if (fs.existsSync(rankLimitsPath)) {
      return JSON.parse(fs.readFileSync(rankLimitsPath, 'utf8'));
    }
  } catch (e) {
    console.warn('Failed to load rank-limits.json:', e.message);
  }
  return null;
}

const config = {
  port: parseInt(process.env.PORT, 10) || 9999,
  baseUrl: process.env.BASE_URL || '',
  apiSecret: process.env.API_SECRET || '',
  cacheDays: parseInt(process.env.CACHE_DAYS, 10) || 30,
  maxDurationSeconds: parseInt(process.env.MAX_DURATION_SECONDS, 10) || 600,
  rateLimitWindowMs: parseInt(process.env.RATE_LIMIT_WINDOW_MS, 10) || 60000,
  rateLimitMax: parseInt(process.env.RATE_LIMIT_MAX, 10) || 10,
  cookiesFile: process.env.COOKIES_FILE || './cookies/cookies.txt',
  audioQuality: parseInt(process.env.AUDIO_QUALITY, 10) || 5,
  devConsoleEnabled: process.env.DEV_CONSOLE === '1',
  devConsoleToken: process.env.DEV_CONSOLE_TOKEN || '',
  // rank limits — empty means everything is unlimited by default
  // loaded from data/rank-limits.json; if file missing, no restrictions
  rankLimits: _loadRankLimitsFile() || {},
  rankLimitsPath,
  spotifyClientId: process.env.SPOTIFY_CLIENT_ID || '',
  spotifyClientSecret: process.env.SPOTIFY_CLIENT_SECRET || '',
  cacheDir: path.join(__dirname, '..', 'cache'),
  dbPath: path.join(__dirname, '..', 'data', 'history.db'),
};

config.reloadRankLimits = function () {
  config.rankLimits = _loadRankLimitsFile() || {};
};

module.exports = config;
