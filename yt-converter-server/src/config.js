const path = require('path');

const envPath = path.join(__dirname, '..', '.env');
require('./env')(envPath);

const config = {
  port: parseInt(process.env.PORT, 10) || 9999,
  baseUrl: process.env.BASE_URL || '',
  apiSecret: process.env.API_SECRET || '',
  cacheDays: parseInt(process.env.CACHE_DAYS, 10) || 30,
  maxDurationSeconds: parseInt(process.env.MAX_DURATION_SECONDS, 10) || 600,
  rateLimitWindowMs: parseInt(process.env.RATE_LIMIT_WINDOW_MS, 10) || 60000,
  rateLimitMax: parseInt(process.env.RATE_LIMIT_MAX, 10) || 10,
  cookiesFile: process.env.COOKIES_FILE || './cookies/cookies.txt',
  spotifyClientId: process.env.SPOTIFY_CLIENT_ID || '',
  spotifyClientSecret: process.env.SPOTIFY_CLIENT_SECRET || '',
  cacheDir: path.join(__dirname, '..', 'cache'),
  dbPath: path.join(__dirname, '..', 'data', 'history.db'),
};

module.exports = config;
