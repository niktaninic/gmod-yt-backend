const { execFile, execFileSync } = require('child_process');
const fs = require('fs');
const path = require('path');
const config = require('./config');

if (!fs.existsSync(config.cacheDir)) {
  fs.mkdirSync(config.cacheDir, { recursive: true });
}

// yt-dlp wants a writable cookies file, so use a temp copy
const writableCookiesPath = path.join(config.cacheDir, '.cookies_tmp.txt');
let _cookiesMtime = 0;

function getWritableCookiesPath() {
  if (!fs.existsSync(config.cookiesFile)) return null;
  try {
    const mtime = fs.statSync(config.cookiesFile).mtimeMs;
    if (mtime !== _cookiesMtime) {
      fs.copyFileSync(config.cookiesFile, writableCookiesPath);
      _cookiesMtime = mtime;
    }
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
    // --print is much leaner than --dump-json (no multi-MB JSON blob)
    const args = ['--no-download', '--print', '%(title)s\t%(duration)s', ...buildBaseArgs()];
    args.push(`https://www.youtube.com/watch?v=${videoId}`);

    execFile('yt-dlp', args, { timeout: 20000, maxBuffer: 64 * 1024 }, (err, stdout, stderr) => {
      if (err) return reject(new Error(`yt-dlp info failed: ${stderr || err.message}`));
      const line = stdout.trim();
      const tab = line.indexOf('\t');
      if (tab === -1) return reject(new Error('Failed to parse yt-dlp output'));
      resolve({
        title: line.slice(0, tab),
        duration: parseFloat(line.slice(tab + 1)) || 0,
      });
    });
  });
}

// single yt-dlp call: prints info then downloads.
// --match-filter gates the download so we never pull a file that's too long.
// --print fires after info extraction but before download, so stdout has title+duration
// even if the download fails. On exit 101 (match-filter blocked) stdout is empty.
async function convertAudio(videoId, maxDuration) {
  const outputPath = path.join(config.cacheDir, `${videoId}.mp3`);

  // rare: another request finished while we were waiting for the lock
  if (fs.existsSync(outputPath)) {
    const info = await getVideoInfo(videoId);
    return { title: info.title, duration: Math.floor(info.duration || 0), filePath: outputPath };
  }

  const tmpPath = path.join(config.cacheDir, `${videoId}.tmp.mp3`);
  const limited = maxDuration != null && maxDuration > 0;
  const args = [
    '--print', '%(title)s\t%(duration)s',
    '--no-simulate',
    ...(limited ? ['--match-filter', `duration <= ${maxDuration}`] : []),
    '-x', '--audio-format', 'mp3', '--audio-quality', String(config.audioQuality),
    '--concurrent-fragments', '4',
    '--no-mtime',
    '--socket-timeout', '30',
    '--retries', '3',
    '--fragment-retries', '5',
    ...buildBaseArgs(), '-o', tmpPath,
    `https://www.youtube.com/watch?v=${videoId}`,
  ];

  return new Promise((resolve, reject) => {
    execFile('yt-dlp', args, { timeout: 120000, maxBuffer: 256 * 1024 }, async (err, stdout, stderr) => {
      if (err) {
        // 101 = no downloads (match-filter blocked = video too long)
        if (err.code === 101 && limited) {
          // stdout is empty on 101; do a fast info-only call for the exact duration
          try {
            const info = await getVideoInfo(videoId);
            const dur = Math.floor(info.duration || 0);
            const maxMin = Math.floor(maxDuration / 60);
            const vidMin = Math.floor(dur / 60);
            const vidSec = dur % 60;
            return reject({ status: 400, message: `Video too long (${vidMin}:${String(vidSec).padStart(2, '0')}). Maximum allowed: ${maxMin} minutes.` });
          } catch (_) {
            return reject({ status: 400, message: `Video too long. Maximum allowed: ${Math.floor(maxDuration / 60)} minutes.` });
          }
        }
        try { fs.unlinkSync(tmpPath); } catch (_) {}
        return reject(new Error(`yt-dlp download failed: ${err.message}`));
      }

      const line = (stdout || '').trim().split('\n')[0] || '';
      const tab = line.indexOf('\t');
      const title = tab !== -1 ? line.slice(0, tab) : videoId;
      const duration = tab !== -1 ? (parseFloat(line.slice(tab + 1)) || 0) : 0;

      const tmpBase = tmpPath.replace(/\.tmp\.mp3$/, '.tmp');
      let foundFile = [tmpPath, ...(['.mp3', '.m4a', '.webm', '.opus'].map(ext => tmpBase + ext))].find(f => fs.existsSync(f));

      // fallback: glob the cache dir for anything matching videoId.tmp* (handles unexpected extensions)
      if (!foundFile) {
        try {
          const prefix = `${videoId}.tmp`;
          const match = fs.readdirSync(config.cacheDir).find(f => f.startsWith(prefix) && !f.endsWith('.part'));
          if (match) foundFile = path.join(config.cacheDir, match);
        } catch (_) {}
      }

      if (!foundFile) {
        console.error(`yt-dlp no output file for ${videoId}. stderr: ${(stderr || '').slice(0, 500)}`);
        return reject(new Error(`Downloaded file not found. yt-dlp stderr: ${(stderr || '(empty)').slice(0, 300)}`));
      }

      try {
        fs.renameSync(foundFile, outputPath);
      } catch (e) {
        return reject(new Error(`Failed to rename: ${e.message}`));
      }

      resolve({ title, duration: Math.floor(duration), filePath: outputPath });
    });
  });
}

function downloadAudio(videoId) {
  return new Promise((resolve, reject) => {
    const outputPath = path.join(config.cacheDir, `${videoId}.mp3`);

    // someone else might have finished first
    if (fs.existsSync(outputPath)) return resolve(outputPath);

    const tmpPath = path.join(config.cacheDir, `${videoId}.tmp.mp3`);
    const args = [
      '-x', '--audio-format', 'mp3', '--audio-quality', String(config.audioQuality),
      '--concurrent-fragments', '4',
      '--no-mtime',
      '--socket-timeout', '30',
      '--retries', '3',
      '--fragment-retries', '5',
      ...buildBaseArgs(), '-o', tmpPath,
    ];
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
    const args = ['--print', '%(id)s\t%(title)s\t%(duration)s', '--no-download', ...buildBaseArgs()];
    args.push(`ytsearch1:${query}`);

    execFile('yt-dlp', args, { timeout: 30000, maxBuffer: 64 * 1024 }, (err, stdout, stderr) => {
      if (err) return reject(new Error(`YouTube search failed: ${stderr || err.message}`));
      const line = (stdout || '').trim();
      const parts = line.split('\t');
      if (parts.length < 3 || !parts[0]) return reject(new Error('No YouTube results found'));
      resolve({
        videoId: parts[0],
        title: parts[1] || '',
        duration: Math.floor(parseFloat(parts[2]) || 0),
      });
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
  convertAudio,
  downloadAudio,
  searchYouTube,
  getPlaylistItems,
};
