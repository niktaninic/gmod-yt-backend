const { execFile, execFileSync } = require('child_process');
const fs = require('fs');
const path = require('path');
const config = require('./config');

if (!fs.existsSync(config.cacheDir)) {
  fs.mkdirSync(config.cacheDir, { recursive: true });
}

// yt-dlp needs write access to cookies, so we work on a copy
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

// pick whatever chrome target is available for --impersonate
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

function extractVideoId(url) {
  const patterns = [
    /(?:youtube\.com\/watch\?.*v=|youtu\.be\/|youtube\.com\/embed\/|youtube\.com\/v\/)([a-zA-Z0-9_-]{11})/,
    /^([a-zA-Z0-9_-]{11})$/,
  ];
  for (const p of patterns) {
    const m = url.match(p);
    if (m) return m[1];
  }
  return null;
}

function getVideoInfo(videoId) {
  return new Promise((resolve, reject) => {
    const args = [
      '--dump-json',
      '--no-download',
      '--no-playlist',
      '--js-runtimes', 'node',
    ];

    const cookiesPath = getWritableCookiesPath();
    if (cookiesPath) {
      args.push('--cookies', cookiesPath);
    }

    if (impersonateTarget) {
      args.push('--impersonate', impersonateTarget);
    }

    args.push(`https://www.youtube.com/watch?v=${videoId}`);

    execFile('yt-dlp', args, { timeout: 30000, maxBuffer: 10 * 1024 * 1024 }, (err, stdout, stderr) => {
      if (err) {
        return reject(new Error(`yt-dlp info failed: ${stderr || err.message}`));
      }
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

    // race condition guard
    if (fs.existsSync(outputPath)) {
      return resolve(outputPath);
    }

    const tmpPath = path.join(config.cacheDir, `${videoId}.tmp.mp3`);

    const args = [
      '-x',
      '--audio-format', 'mp3',
      '--audio-quality', '2',
      '--no-playlist',
      '--js-runtimes', 'node',
      '-o', tmpPath,
    ];

    const cookiesPath = getWritableCookiesPath();
    if (cookiesPath) {
      args.push('--cookies', cookiesPath);
    }

    if (impersonateTarget) {
      args.push('--impersonate', impersonateTarget);
    }

    args.push(`https://www.youtube.com/watch?v=${videoId}`);

    execFile('yt-dlp', args, { timeout: 120000, maxBuffer: 5 * 1024 * 1024 }, (err, stdout, stderr) => {
      if (err) {
          try { fs.unlinkSync(tmpPath); } catch (_) {}
        return reject(new Error(`yt-dlp download failed: ${stderr || err.message}`));
      }

      // yt-dlp sometimes writes with a different extension before converting
      const possibleFiles = [tmpPath];
      const tmpBase = tmpPath.replace(/\.tmp\.mp3$/, '.tmp');
      for (const ext of ['.mp3', '.m4a', '.webm', '.opus']) {
        possibleFiles.push(tmpBase + ext);
      }

      let foundFile = null;
      for (const f of possibleFiles) {
        if (fs.existsSync(f)) {
          foundFile = f;
          break;
        }
      }

      if (!foundFile) {
        return reject(new Error('Downloaded file not found'));
      }

      try {
        fs.renameSync(foundFile, outputPath);
      } catch (e) {
        return reject(new Error(`Failed to rename: ${e.message}`));
      }

      resolve(outputPath);
    });
  });
}

module.exports = {
  extractVideoId,
  getVideoInfo,
  downloadAudio,
};
