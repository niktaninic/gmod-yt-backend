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

`POST /api/convert`
convert one track and return relative `stream_url`.

`POST /api/info`
metadata only, no download.

`POST /api/playlist`
playlist metadata only. first track still gets converted on addon side when playback starts.

`GET /stream/:filename`
serves cached mp3, range requests work.

`GET /api/history`
recent conversions.

`GET /api/health`
basic alive check.

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
