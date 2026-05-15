const https = require('https');
const config = require('./config');
const converter = require('./converter');

// spotify ids are 22-char base62 blobs
function extractTrackId(url) {
  const patterns = [
    /open\.spotify\.com\/(?:intl-[a-z]{2}\/)?track\/([a-zA-Z0-9]{22})/,
    /spotify:track:([a-zA-Z0-9]{22})/,
  ];
  for (const p of patterns) {
    const m = url.match(p);
    if (m) return m[1];
  }
  return null;
}

function extractPlaylistId(url) {
  const patterns = [
    /open\.spotify\.com\/(?:intl-[a-z]{2}\/)?playlist\/([a-zA-Z0-9]+)/,
    /spotify:playlist:([a-zA-Z0-9]+)/,
  ];
  for (const p of patterns) {
    const m = url.match(p);
    if (m) return m[1];
  }
  return null;
}

function isConfigured() {
  return !!(config.spotifyClientId && config.spotifyClientSecret);
}

let accessToken = null;
let tokenExpiry = 0;

function requestToken() {
  return new Promise((resolve, reject) => {
    const creds = Buffer.from(`${config.spotifyClientId}:${config.spotifyClientSecret}`).toString('base64');
    const postData = 'grant_type=client_credentials';

    const req = https.request({
      hostname: 'accounts.spotify.com',
      path: '/api/token',
      method: 'POST',
      headers: {
        'Authorization': `Basic ${creds}`,
        'Content-Type': 'application/x-www-form-urlencoded',
        'Content-Length': Buffer.byteLength(postData),
      },
    }, (res) => {
      let body = '';
      res.on('data', chunk => body += chunk);
      res.on('end', () => {
        if (res.statusCode !== 200) {
          return reject(new Error(`Spotify auth failed (${res.statusCode})`));
        }
        try {
          const data = JSON.parse(body);
          accessToken = data.access_token;
          tokenExpiry = Date.now() + (data.expires_in - 60) * 1000;
          resolve(accessToken);
        } catch (e) {
          reject(new Error('Failed to parse Spotify token response'));
        }
      });
    });
    req.on('error', reject);
    req.write(postData);
    req.end();
  });
}

async function getToken() {
  if (accessToken && Date.now() < tokenExpiry) return accessToken;
  return requestToken();
}

function fetchSpotifyApi(token, apiPath) {
  return new Promise((resolve, reject) => {
    const req = https.request({
      hostname: 'api.spotify.com',
      path: apiPath,
      method: 'GET',
      headers: { 'Authorization': `Bearer ${token}` },
    }, (res) => {
      let body = '';
      res.on('data', chunk => body += chunk);
      res.on('end', () => {
        if (res.statusCode !== 200) {
          return reject(new Error(`Spotify API failed (${res.statusCode})`));
        }
        try {
          resolve(JSON.parse(body));
        } catch (e) {
          reject(new Error('Failed to parse Spotify API response'));
        }
      });
    });
    req.on('error', reject);
    req.end();
  });
}

async function getTrackInfo(trackId) {
  const token = await getToken();
  const data = await fetchSpotifyApi(token, `/v1/tracks/${encodeURIComponent(trackId)}`);
  return {
    name: data.name,
    artists: (data.artists || []).map(a => a.name).join(', '),
    duration: Math.floor((data.duration_ms || 0) / 1000),
  };
}

async function getPlaylistTracks(playlistId) {
  const token = await getToken();
  const tracks = [];
  let offset = 0;
  const limit = 100;
  const MAX_PAGES = 200;
  let pages = 0;

  while (pages < MAX_PAGES) {
    pages++;
    const data = await fetchSpotifyApi(token, `/v1/playlists/${encodeURIComponent(playlistId)}/tracks?limit=${limit}&offset=${offset}`);
    for (const item of (data.items || [])) {
      const track = item.track;
      if (!track || !track.id) continue;
      tracks.push({
        trackId: track.id,
        name: track.name,
        artists: (track.artists || []).map(a => a.name).join(', '),
        duration: Math.floor((track.duration_ms || 0) / 1000),
      });
    }
    if (!data.next) break;
    offset += limit;
  }

  return tracks;
}

async function resolveToYouTube(trackId) {
  const track = await getTrackInfo(trackId);
  const query = `${track.artists} - ${track.name}`;
  console.log(`Spotify search: "${query}"`);

  const yt = await converter.searchYouTube(query);
  console.log(`Spotify -> YouTube: ${trackId} -> ${yt.videoId} (${yt.title})`);

  return {
    trackId,
    videoId: yt.videoId,
    title: `${track.artists} - ${track.name}`,
    trackName: track.name,
    artist: track.artists,
    spotifyDuration: track.duration,
    youtubeDuration: yt.duration,
  };
}

module.exports = {
  extractTrackId,
  extractPlaylistId,
  isConfigured,
  getTrackInfo,
  getPlaylistTracks,
  resolveToYouTube,
};
