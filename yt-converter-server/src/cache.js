const fs = require('fs');
const path = require('path');
const config = require('./config');
const database = require('./database');

function cleanExpired() {
  const expired = database.getExpiredCacheEntries();
  let cleaned = 0;

  for (const entry of expired) {
    const filePath = path.join(config.cacheDir, `${entry.video_id}.mp3`);
    try {
      if (fs.existsSync(filePath)) {
        fs.unlinkSync(filePath);
      }
    } catch (e) {
      console.error(`Failed to delete cache file ${filePath}: ${e.message}`);
    }

    database.deleteCacheEntry(entry.video_id);
    cleaned++;
  }

  if (cleaned > 0) {
    console.log(`Cache cleanup: removed ${cleaned} expired entries`);
  }
}

function startCleanupInterval() {
  cleanExpired();
  setInterval(cleanExpired, 3600000); // hourly
}

module.exports = { cleanExpired, startCleanupInterval };
