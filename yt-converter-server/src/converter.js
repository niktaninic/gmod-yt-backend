const { execFile, execFileSync } = require('child_process');
const fs = require('fs');
const path = require('path');
const config = require('./config');

if (!fs.existsSync(config.cacheDir)) {
  fs.mkdirSync(config.cacheDir, { recursive: true });
}

// yt-dlp wants a writable cookies file, so use a temp copy
const writableCookiesPath = path.join(config.cacheDir, '.cookies_tmp.txt');

function getWritableCookiesPath() {
  if (!fs.existsSync(config.cookiesFile)) return null;
  try {
    fs.copyFileSync(config.cookiesFile, writableCookiesPath);
    return writableCookiesPath;
  } catch (e) {
    console.warn('Could not copy cookies to writable path:', e.message);
    return config.cookiesFile;
  }
}

// grab any chrome-ish target yt-dlp exposes
let impersonateTarget = null;
try {
  const targets = execFileSync('yt-dlp', ['--list-impersonate-targets'], { timeout: 10000 }).toString();
  const match = targets.match(/^\s*(chrome[\w:-]*)/m);
  if (match) {
    impersonateTarget = match[1].trim();
    console.log(`yt-dlp impersonate target: ${impersonateTarget}`);
  } else {
    console.warn('No chrome impersonate targets available. Running without --impersonate.');
  }
} catch (e) {
  console.warn('Could not detect impersonate targets:', e.message);
}

try {
  const nodeVersion = execFileSync('node', ['--version'], { timeout: 5000 }).toString().trim();
  console.log(`node available for yt-dlp js solving: ${nodeVersion}`);
} catch (_) {}

function buildBaseArgs(opts) {
  const args = [];
  if (!opts || !opts.allowPlaylist) args.push('--no-playlist');
  args.push('--js-runtimes', 'node');

  const cookiesPath = getWritableCookiesPath();
  if (cookiesPath) args.push('--cookies', cookiesPath);
  if (impersonateTarget) args.push('--impersonate', impersonateTarget);

  return args;
}

function getVideoInfo(videoId) {
  return new Promise((resolve, reject) => {
    const args = ['--dump-json', '--no-download', ...buildBaseArgs()];
    args.push(`https://www.youtube.com/watch?v=${videoId}`);

    execFile('yt-dlp', args, { timeout: 30000, maxBuffer: 10 * 1024 * 1024 }, (err, stdout, stderr) => {
      if (err) return reject(new Error(`yt-dlp info failed: ${stderr || err.message}`));
      try {
        resolve(JSON.parse(stdout));
      } catch (e) {
        reject(new Error('Failed to parse yt-dlp output'));
      }
    });
  });
}

function downloadAudio(videoId) {
  return new Promise((resolve, reject) => {
    const outputPath = path.join(config.cacheDir, `${videoId}.mp3`);

    // someone else might have finished first
    if (fs.existsSync(outputPath)) return resolve(outputPath);

    const tmpPath = path.join(config.cacheDir, `${videoId}.tmp.mp3`);
    const args = ['-x', '--audio-format', 'mp3', '--audio-quality', '2', ...buildBaseArgs(), '-o', tmpPath];
    args.push(`https://www.youtube.com/watch?v=${videoId}`);

    execFile('yt-dlp', args, { timeout: 120000, maxBuffer: 5 * 1024 * 1024 }, (err, stdout, stderr) => {
      if (err) {
        try { fs.unlinkSync(tmpPath); } catch (_) {}
        return reject(new Error(`yt-dlp download failed: ${stderr || err.message}`));
      }

      // yt-dlp can leave weird temp ext names behind
      const tmpBase = tmpPath.replace(/\.tmp\.mp3$/, '.tmp');
      const possibleFiles = [tmpPath, ...(['.mp3', '.m4a', '.webm', '.opus'].map(ext => tmpBase + ext))];

      const foundFile = possibleFiles.find(f => fs.existsSync(f));
      if (!foundFile) return reject(new Error('Downloaded file not found'));

      try {
        fs.renameSync(foundFile, outputPath);
      } catch (e) {
        return reject(new Error(`Failed to rename: ${e.message}`));
      }

      resolve(outputPath);
    });
  });
}

function searchYouTube(query) {
  return new Promise((resolve, reject) => {
    const args = ['--dump-json', '--no-download', ...buildBaseArgs()];
    args.push(`ytsearch1:${query}`);

    execFile('yt-dlp', args, { timeout: 30000, maxBuffer: 10 * 1024 * 1024 }, (err, stdout, stderr) => {
      if (err) return reject(new Error(`YouTube search failed: ${stderr || err.message}`));
      try {
        const data = JSON.parse(stdout);
        if (!data.id) return reject(new Error('No YouTube results found'));
        resolve({
          videoId: data.id,
          title: data.title || '',
          duration: Math.floor(data.duration || 0),
        });
      } catch (e) {
        reject(new Error('Failed to parse YouTube search results'));
      }
    });
  });
}

function getPlaylistItems(url) {
  return new Promise((resolve, reject) => {
    const args = ['--flat-playlist', '--dump-json', '--no-download', ...buildBaseArgs({ allowPlaylist: true })];
    args.push(url);

    execFile('yt-dlp', args, { timeout: 60000, maxBuffer: 50 * 1024 * 1024 }, (err, stdout, stderr) => {
      if (err) return reject(new Error(`yt-dlp playlist failed: ${stderr || err.message}`));
      try {
        const items = stdout.trim().split('\n')
          .filter(line => line.trim())
          .map(line => {
            const entry = JSON.parse(line);
            return {
              videoId: entry.id,
              title: entry.title || '',
              duration: Math.floor(entry.duration || 0),
            };
          })
          .filter(item => item.videoId);
        if (items.length === 0) return reject(new Error('Empty playlist'));
        resolve(items);
      } catch (e) {
        reject(new Error('Failed to parse yt-dlp playlist output'));
      }
    });
  });
}

module.exports = {
  getVideoInfo,
  downloadAudio,
  searchYouTube,
  getPlaylistItems,
};
