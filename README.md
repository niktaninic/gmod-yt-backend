# gmod-yt-backend

YouTube / Spotify → MP3 converter backend for the GMod addon
[3D Stream Radio](https://github.com/Grocel/3D-Stream-Radio), plus the patched addon files.

- `yt-converter-server/` – the backend (Go, runs in Docker)
- `3d-stream-radio-master/lua/` – changed/new addon files only, copy them over the addon

The full list of bugs found during the rewrite is in [`docs/ANALIZA.md`](docs/ANALIZA.md) (Polish).

## architecture

```
GMod server ──X-SR-Key──┐
                        ├─► Go HTTP server ──► worker pool ──► yt-dlp + ffmpeg ──► cache/<id>.mp3
GMod clients ─X-SR-Token┘        │                                   (one run per video, shared)
                                 ├─► SQLite (history, cache index, spotify map, blocks)
GMod clients ── GET /stream/<id>.mp3 ──► sendfile(2) straight from disk (Range supported)
```

- the server is a single static Go binary (~12 MB, ~20 MB RAM)
- yt-dlp is still used as the extractor (nothing else keeps up with YouTube), but only through a
  bounded worker pool: `MAX_CONCURRENT_CONVERSIONS` jobs, `MAX_QUEUED_CONVERSIONS` waiting, the rest
  get `503 BUSY` instead of fork-bombing the host
- the same video requested by 30 players at once = **one** yt-dlp run
- metadata + download + mp3 transcode in **one** yt-dlp call, too-long videos are rejected before
  anything is downloaded
- playlists and Spotify lookups are cached and de-duplicated
- mp3 files are served with `sendfile`, Range/HEAD/ETag/If-Range handled by Go's `http.ServeContent`

## run (docker)

```bash
cd yt-converter-server
cp .env.example .env
# put a long random value into API_SECRET, e.g. the output of: openssl rand -hex 32
mkdir -p data cache cookies
docker compose up -d --build
docker compose logs -f
```

The server refuses to start with an empty or placeholder `API_SECRET`.

Health: `curl http://localhost:9999/api/health` (also used by the Docker `HEALTHCHECK`).

Update yt-dlp when YouTube breaks something:

```bash
docker compose build --pull --no-cache && docker compose up -d
```

or set `YTDLP_AUTO_UPDATE=1` in `.env` to `pip install -U yt-dlp` on every container start.

### upgrading from the Node.js version

- `data/` (history.db, rank-limits.json) and `cache/` are reused as-is, the schema is migrated
  automatically. Ownership of the volumes is fixed on first start (the old container ran as root).
- `API_SECRET` is now mandatory and must not be the example value.
- the cookies mount is read-only now (each yt-dlp run gets its own temp copy).
- copy **all** files from `3d-stream-radio-master/lua/` again, including the new
  `lua/autorun/streamradio_converter.lua`, and delete `lua/streamradio_core/sv_streamradio_ranks.lua`
  from the addon if you copied it before (it was a dead duplicate).

## gmod side

```cfg
sv_streamradio_converter_url "http://example.com:9999"
sv_streamradio_converter_secret "same value as API_SECRET"
sv_streamradio_yt_debug 1
sv_streamradio_spotify_debug 1
```

`sv_streamradio_converter_secret` is **server-only** now (`FCVAR_PROTECTED`, not replicated).
Before, it was `FCVAR_REPLICATED`, so every player could read it in the console and use the API.
Clients get a per-player token instead (see auth below).

## auth

| caller | header | notes |
|---|---|---|
| GMod server | `X-SR-Key: <API_SECRET>` | player fields in the body (`steamid`, `rank`, …) are trusted |
| GMod client | `X-SR-Token: <token>` | token = `v1\|<steamid>\|<usergroup>\|<expires>\|<hex HMAC-SHA256(API_SECRET, first 4 fields)>`, signed by the GMod server and sent to the player via net message. SteamID and rank come from the token, so players can't spoof a higher rank. |

`/api/history` and `/api/ranks` need the server key. `/stream/*` and `/api/health` are public.

## api

Errors always look like `{"success": false, "error": "human text", "code": "MACHINE_CODE"}`.

| code | http | meaning |
|---|---|---|
| `INVALID_URL` | 400 | not a supported YouTube/Spotify URL |
| `TOO_LONG` | 400 | longer than the rank's / global limit (also enforced for cached tracks) |
| `LIVE_UNSUPPORTED` | 400 | live streams can't be converted |
| `SPOTIFY_DISABLED` | 400 | no Spotify credentials configured |
| `UNAUTHORIZED` | 401 | missing/invalid key or token |
| `BLOCKED` | 403 | steamid is blocked |
| `RATE_LIMITED` | 429 | too many fresh conversions (has `Retry-After`) |
| `DAILY_LIMIT` | 429 | rank's daily conversion limit reached |
| `BUSY` | 503 | worker queue full (has `Retry-After`) |
| `CONVERSION_FAILED` / `TIMEOUT` | 500 / 504 | yt-dlp failed |

### `POST /api/convert`

```json
{ "url": "https://www.youtube.com/watch?v=...", "nick": "PlayerName", "steamid": "STEAM_0:0:1", "rank": "user", "server_ip": "1.2.3.4", "player_ip": "5.6.7.8" }
```

only `url` is required. Supported: `watch?v=`, `youtu.be/`, `/shorts/`, `/live/`, `/embed/`,
`music.youtube.com`, `open.spotify.com/track/…`, `spotify:track:…`.

```json
{ "success": true, "stream_url": "/stream/dQw4w9WgXcQ.mp3", "video_id": "dQw4w9WgXcQ", "source": "youtube",
  "title": "Rick Astley - Never Gonna Give You Up", "duration": 213, "cached": false }
```

Spotify responses add `"track_id"` and `"source": "spotify"`. Rate limits only apply to work that
actually starts a conversion; cache hits are always cheap.

### `POST /api/info`

metadata only: `source, video_id|track_id, title, duration, duration_formatted, is_live, allowed, max_duration`
(`allowed` / `max_duration` use the caller's rank limits).

### `POST /api/playlist`

```json
{ "source": "youtube", "playlist_id": "PL…", "tracks": [ { "url": "…", "video_id": "…", "title": "…", "duration": 213 } ] }
```

capped at `MAX_PLAYLIST_ITEMS`, cached for 10 minutes.

### `GET|HEAD /stream/:videoId.mp3`

no auth, `Range` (incl. `bytes=-500`), `If-Range`, `ETag`, `Last-Modified`, 416 handled.

### `GET /api/history?page=1&limit=50` (server key)

### `POST /api/ranks` (server key) – `{ "ranks": ["user", "vip"] }`

### `GET /api/health`

`{ "status": "ok", "uptime": 3600, "ytdlp": "2026.10.26", "activeConversions": 0, "queuedConversions": 0, "activeStreams": 3, ... }`

## rank limits

`data/rank-limits.json` (or edit it in the dev console):

```json
{
  "user":  { "dailyConversions": 20, "maxDurationSeconds": 600 },
  "vip":   { "dailyConversions": 100, "maxDurationSeconds": 1200 },
  "admin": null
}
```

- rank listed → its own values (`0` = unlimited for that field)
- rank listed as `null` → unlimited
- rank not listed → global `MAX_DURATION_SECONDS`, no daily cap
  (the old version applied **no** length limit at all here)

`dailyConversions` counts fresh conversions per steamid in the last 24 h. Cached tracks don't count.

## dev console

`DEV_CONSOLE=1` and `DEV_CONSOLE_TOKEN=<at least 12 chars>`, then open `/dev`.
Session cookie (`HttpOnly`, `SameSite=Strict`, `Secure` behind https), login is rate limited,
state-changing requests are origin checked.

## env

| var | default | |
|---|---|---|
| `API_SECRET` | – | **required** |
| `HOST_PORT` | `9999` | published port (compose) |
| `TRUST_PROXY` | `0` | use `X-Real-IP` / `X-Forwarded-For` (only behind a reverse proxy) |
| `CACHE_DAYS` | `30` | sliding: counted from the last play |
| `MAX_CACHE_SIZE_MB` | `0` | LRU eviction above this size, 0 = off |
| `HISTORY_DAYS` | `90` | prune history, 0 = keep forever |
| `MAX_DURATION_SECONDS` | `600` | default length cap, 0 = unlimited |
| `MAX_PLAYLIST_ITEMS` | `500` | |
| `RATE_LIMIT_WINDOW_MS` / `RATE_LIMIT_MAX` | `60000` / `10` | fresh conversions per player (or IP) per window |
| `RATE_LIMIT_SERVER_MAX` | `120` | same for server requests without a player |
| `MAX_CONCURRENT_CONVERSIONS` | `3` | parallel yt-dlp+ffmpeg jobs |
| `MAX_QUEUED_CONVERSIONS` | `50` | waiting jobs before `503 BUSY` |
| `MAX_CONCURRENT_META` | `6` | parallel info/search/playlist lookups |
| `CONVERSION_TIMEOUT_SECONDS` | `300` | kills yt-dlp **and** its ffmpeg children |
| `AUDIO_QUALITY` | `5` | LAME VBR 0 (best) … 9 |
| `AUDIO_FORMAT` | `bestaudio[abr<=192]/bestaudio/best` | yt-dlp format selector |
| `YTDLP_IMPERSONATE` | `auto` | `auto`, `off` or a target like `chrome-133` |
| `YTDLP_JS_RUNTIME` | `node` | Node 22 is bundled (yt-dlp rejects Node 20) |
| `YTDLP_AUTO_UPDATE` | `0` | update yt-dlp on container start |
| `COOKIES_FILE` | `/app/cookies/cookies.txt` | netscape format |
| `CLIENT_TOKENS` | `1` | accept `X-SR-Token` |
| `CLIENT_TOKEN_MAX_TTL_SECONDS` | `86400` | |
| `SPOTIFY_CLIENT_ID` / `SPOTIFY_CLIENT_SECRET` | – | Spotify support |
| `DEV_CONSOLE` / `DEV_CONSOLE_TOKEN` | `0` / – | |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `text` | `json` for log shippers |

## development

```bash
cd yt-converter-server
go test ./...
go run ./cmd/server          # needs yt-dlp, ffmpeg and node >= 22 in PATH
```

## what still breaks

- YouTube changes something → update yt-dlp (see above)
- datacenter IPs get "Sign in to confirm you're not a bot" → put fresh cookies into `cookies/cookies.txt`
- Spotify needs valid client credentials
