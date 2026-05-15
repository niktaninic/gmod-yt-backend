# gmod-yt-backend

backend + addon patch files for 3D Stream Radio.
server lives in `yt-converter-server/`.
changed addon files live in `3d-stream-radio-master/lua/`.

## run

docker:

```bash
cd yt-converter-server
cp .env.example .env
mkdir -p cookies
docker compose up -d --build
```

bare metal:

```bash
cd yt-converter-server
npm install
pip install "yt-dlp[default]" curl_cffi
cp .env.example .env
npm run dev
```

needs: node 18+, python3, yt-dlp, ffmpeg, curl_cffi

fill `.env` before you start it if you want anything except the most basic youtube path.

## auth

send `X-SR-Key` with the same secret as `API_SECRET`.

`X-SR-Test: 1` still works, but only with the default dev secret. dont leave that in prod.

## api

all requests that mutate or fetch data need `X-SR-Key: <API_SECRET>` header.

---

### `POST /api/convert`

converts a track and returns a streamable URL. downloads if not cached.

**request body (JSON)**
```json
{
  "url": "https://www.youtube.com/watch?v=...",
  "nick": "PlayerName",
  "steamid": "STEAM_0:0:12345678",
  "rank": "user",
  "server_ip": "192.168.1.1",
  "player_ip": "10.0.0.1"
}
```
only `url` is required. others are used for logging and rank limit checks.

**response**
```json
{
  "success": true,
  "stream_url": "/stream/dQw4w9WgXcQ.mp3",
  "video_id": "dQw4w9WgXcQ",
  "title": "Rick Astley - Never Gonna Give You Up",
  "duration": 213,
  "cached": false,
  "track_id": "spotify:track:...",
  "source": "spotify"
}
```
`track_id` and `source: 'spotify'` only appear for Spotify URLs. `cached: true` means file was already on disk, no conversion was done.

**errors:** `400` invalid/missing URL, `403` steamid is blocked, `429` daily limit hit, `500` conversion failed.

---

### `POST /api/info`

metadata only, no download.

**request body**
```json
{ "url": "https://..." }
```

**response (YouTube)**
```json
{
  "source": "youtube",
  "video_id": "dQw4w9WgXcQ",
  "title": "Rick Astley - Never Gonna Give You Up",
  "duration": 213,
  "duration_formatted": "3:33",
  "allowed": true,
  "max_duration": 600
}
```

**response (Spotify)**
```json
{
  "source": "spotify",
  "track_id": "4uLU6hMCjMI75M1A2tKUQC",
  "title": "Rick Astley - Never Gonna Give You Up",
  "artist": "Rick Astley",
  "track_name": "Never Gonna Give You Up",
  "duration": 213,
  "duration_formatted": "3:33",
  "allowed": true,
  "max_duration": 600
}
```
`allowed` is false if the track exceeds `MAX_DURATION_SECONDS`.

---

### `POST /api/playlist`

playlist metadata only, no conversion.

**request body**
```json
{ "url": "https://www.youtube.com/playlist?list=..." }
```

**response (YouTube)**
```json
{
  "source": "youtube",
  "playlist_id": "PLxxxxxx",
  "tracks": [
    {
      "url": "https://www.youtube.com/watch?v=...",
      "video_id": "dQw4w9WgXcQ",
      "title": "Rick Astley - Never Gonna Give You Up",
      "duration": 213
    }
  ]
}
```

**response (Spotify)**
```json
{
  "source": "spotify",
  "playlist_id": "37i9dQZF...",
  "tracks": [
    {
      "url": "spotify:track:4uLU6hMCjMI75M1A2tKUQC",
      "track_id": "4uLU6hMCjMI75M1A2tKUQC",
      "title": "Rick Astley - Never Gonna Give You Up",
      "duration": 213
    }
  ]
}
```

---

### `GET /stream/:videoId.mp3`

streams cached mp3. supports `Range` header for seeking.

no auth required. respond with `audio/mpeg`. returns `404` if expired/not cached.

---

### `GET /api/history`

recent conversions, paginated.

**query params:** `page` (default 1), `limit` (default 50, max 100)

**response**
```json
{
  "page": 1,
  "limit": 50,
  "total": 312,
  "totalPages": 7,
  "entries": [
    {
      "video_id": "dQw4w9WgXcQ",
      "title": "...",
      "duration": 213,
      "nick": "PlayerName",
      "steamid": "STEAM_0:0:...",
      "server_ip": "...",
      "cached": 0,
      "source": "youtube",
      "rank": "user",
      "created_at": "2026-05-15 14:00:00"
    }
  ]
}
```

---

### `GET /api/health`
basic alive check.

**response:** `{ "status": "ok", "uptime": 3600 }`

---

### `POST /api/ranks`
GMod server pushes its ULX usergroup list here. used by the dev console to show available ranks. body: `{ ranks: ["user", "vip", "admin"] }`.

## gmod side

copy files from `3d-stream-radio-master/lua/` into the addon.
that folder only keeps changed files now.

use these convars:

```cfg
sv_streamradio_converter_url "http://example.com:9999"
sv_streamradio_converter_secret "your_api_secret"
sv_streamradio_yt_debug 1
sv_streamradio_spotify_debug 1
```

spotify playlist/track stuff also needs `SPOTIFY_CLIENT_ID` and `SPOTIFY_CLIENT_SECRET` in `.env`.

## cookies

drop netscape-format cookies into `yt-converter-server/cookies/cookies.txt` if youtube starts acting up.

## rank limits

create `yt-converter-server/data/rank-limits.json` to cap daily conversions and max track length per ULX rank:

```json
{
  "user": { "dailyConversions": 20, "maxDurationSeconds": 600 },
  "vip": { "dailyConversions": 100, "maxDurationSeconds": 1200 },
  "admin": null
}
```

`null` or missing = no limits for that rank. `dailyConversions` is per steamid per UTC day. the file hot-reloads via the dev console — no restart needed.

## dev console

enabled with `DEV_CONSOLE=1` and `DEV_CONSOLE_TOKEN=<something>` in `.env`.

go to `/dev` — it redirects to a login form. token is submitted via POST and stored as an httpOnly session cookie (`Path=/dev`, `SameSite=Strict`, 8h TTL). 

shows live stats, conversion history, rank limit editor, and blocked player management.

blocking a steamid via the console prevents them from converting anything. unblock the same way.

`/dev/logout` to kill the session.

## what breaks

- yt-dlp gets old, youtube changes something, everything goes sideways
- cookies expire
- curl_cffi or yt-dlp updates can break impersonate stuff
- long tracks get rejected if they pass `MAX_DURATION_SECONDS`
- spotify needs valid client creds or that path is dead

## env

`PORT`
default `9999`

`API_SECRET`
shared key for the addon

`BASE_URL`
optional. backend mostly works without it right now

`CACHE_DAYS`
how long mp3s stay around

`MAX_DURATION_SECONDS`
hard cutoff for track length

`RATE_LIMIT_WINDOW_MS`
rate limit window

`RATE_LIMIT_MAX`
max hits per window

`SPOTIFY_CLIENT_ID`
needed for spotify track + playlist support

`SPOTIFY_CLIENT_SECRET`
same deal

`COOKIES_FILE`
path to cookies file

`AUDIO_QUALITY`
yt-dlp VBR quality, 0 (best) to 9 (worst). default `5`

`DEV_CONSOLE`
set to `1` to enable the dev console at `/dev`

`DEV_CONSOLE_TOKEN`
token for dev console auth. required when `DEV_CONSOLE=1`
