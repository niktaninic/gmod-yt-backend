const crypto = require('crypto');
const config = require('./config');

// auth: X-SR-Key (main), X-SR-Test (dev only), HMAC sig (legacy)
function authMiddleware(req, res, next) {
  const apiKey = req.headers['x-sr-key'];
  if (apiKey && config.apiSecret && apiKey === config.apiSecret) {
    return next();
  }

  // dev test mode, only works with default secret
  const isTestMode = req.headers['x-sr-test'] === '1';
  if (isTestMode && config.apiSecret === 'dev-secret-change-in-production') {
    req.isTestMode = true;
    return next();
  }

  // legacy hmac, kept for backwards compat
  const signature = req.headers['x-sr-signature'];
  const timestamp = req.headers['x-sr-timestamp'];

  if (signature && timestamp) {
    const now = Math.floor(Date.now() / 1000);
    const ts = parseInt(timestamp, 10);
    if (isNaN(ts) || Math.abs(now - ts) > 300) {
      return res.status(401).json({ error: 'Request timestamp expired or invalid' });
    }

    const body = JSON.stringify(req.body);
    const payload = `${timestamp}.${body}`;
    const expected = crypto
      .createHmac('sha256', config.apiSecret)
      .update(payload)
      .digest('hex');

    try {
      if (crypto.timingSafeEqual(Buffer.from(signature, 'hex'), Buffer.from(expected, 'hex'))) {
        return next();
      }
    } catch (_) {}

    return res.status(401).json({ error: 'Invalid signature' });
  }

  return res.status(401).json({ error: 'Missing authentication. Send X-SR-Key header.' });
}

module.exports = authMiddleware;
