# gmod-yt-backend

YouTube → MP3 for GMod 3D Stream Radio. yt-dlp does the heavy lifting, express serves the files.

## run it

### docker (recommended)

```bash
cp .env.example .env   # set API_SECRET
mkdir -p cookies       # optional: drop cookies.txt here (netscape format)
docker compose up -d --build
```

### bare metal

need: node 18+, python3, yt-dlp, ffmpeg, curl_cffi

```bash
npm install
pip install "yt-dlp[default]" curl_cffi
cp .env.example .env
npm run dev
```

## auth

protected endpoints need `X-SR-Key` header matching `API_SECRET` from `.env`.

also supports `X-SR-Test: 1` (dev only, default secret) and legacy HMAC (`X-SR-Signature` + `X-SR-Timestamp`).

## endpoints

### `POST /api/convert` (auth + rate limit)

body:
```json
{
  "url": "https://youtube.com/watch?v=dQw4w9WgXcQ",
  "nick": "PlayerName",
  "steamid": "STEAM_0:1:12345",
  "server_ip": "192.168.21.37:27015"
}
```

only `url` is required. rest is for history.

response:
```json
{
  "success": true,
  "stream_url": "/stream/dQw4w9WgXcQ.mp3",
  "video_id": "dQw4w9WgXcQ",
  "title": "Rick Astley - Never Gonna Give You Up",
  "duration": 212,
  "cached": true
}
```

`stream_url` is relative — prepend the server base url.

errors: 400 (bad url, too long), 401, 429, 500

### `POST /api/info` (auth + rate limit)

video metadata without converting. body: `{ "url": "..." }`

### `GET /api/history?page=1&limit=50` (public)

conversion history, paginated.

### `GET /api/health` (public)

`{ "status": "ok", "uptime": 3621.5 }`

### `GET /stream/:videoId.mp3` (public)

serves cached mp3s. supports range requests (seeking). 404 if not converted yet.

## gmod setup

only one file is changed in the 3D Stream Radio addon: `lua/streamradio_core/interfaces/youtube.lua`. just replace the original with the one from this repo.

```
sv_streamradio_yt_converter_url "http://example.com:9999"
sv_streamradio_yt_converter_secret "your_api_secret"
sv_streamradio_yt_debug 1  -- optional
```

## cookies

yt-dlp `--impersonate` handles most blocks but real cookies are more reliable. export from browser in netscape format → `cookies/cookies.txt`

## what can break

- yt-dlp gets outdated → youtube changes their api → `pip install -U yt-dlp` inside the container
- cookies expire → re-export from browser
- curl_cffi breaks on update → rebuild docker image
- video over 10min (configurable via `MAX_DURATION_SECONDS` in `.env`)

## .env

| var | default | what |
|-----|---------|------|
| `PORT` | `9999` | server port |
| `API_SECRET` | — | shared key, same as gmod convar |
| `CACHE_DAYS` | `30` | mp3 cache lifetime |
| `MAX_DURATION_SECONDS` | `600` | reject longer videos |
| `RATE_LIMIT_WINDOW_MS` | `60000` | rate limit window |
| `RATE_LIMIT_MAX` | `10` | max requests per window |
| `COOKIES_FILE` | `./cookies/cookies.txt` | yt cookies path |
