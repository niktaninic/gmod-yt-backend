const crypto = require('crypto');
const config = require('./config');

function authMiddleware(req, res, next) {
  const apiKey = req.headers['x-sr-key'];
  if (apiKey && config.apiSecret) {
    try {
      const a = Buffer.from(apiKey);
      const b = Buffer.from(config.apiSecret);
      if (a.length === b.length && crypto.timingSafeEqual(a, b)) return next();
    } catch (_) {}
  }

  // dev-only bypass, dead simple on purpose
  const isTestMode = req.headers['x-sr-test'] === '1';
  if (isTestMode && config.apiSecret === 'dev-secret-change-in-production') {
    req.isTestMode = true;
    return next();
  }

  return res.status(401).json({ error: 'Missing authentication. Send X-SR-Key header.' });
}

module.exports = authMiddleware;
