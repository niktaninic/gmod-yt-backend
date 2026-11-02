# Analiza kodu i przepisanie backendu

Backend przepisany z Node.js/Express na **Go** (jeden statyczny plik binarny), całość w Dockerze.
Poniżej lista błędów znalezionych w poprzedniej wersji (backend + pliki Lua) i co z nimi zrobiono.

## Bezpieczeństwo

| # | Problem | Waga | Naprawa |
|---|---|---|---|
| 1 | `sv_streamradio_converter_secret` miał `FCVAR_REPLICATED` – **każdy gracz** mógł wpisać nazwę convara w konsoli, odczytać sekret i używać API (konwersje, historia, `/api/ranks`). | krytyczna | Sekret tylko na serwerze (`FCVAR_PROTECTED`). Klienci dostają od serwera krótkotrwały token HMAC-SHA256 przypisany do SteamID i rangi (`X-SR-Token`). |
| 2 | Stored XSS w konsoli dev: nick gracza (wysyłany przez klienta) i SteamID trafiały do `onclick="quickBlock('…')"`. `esc()` nie chroni kontekstu JS (przeglądarka dekoduje encje przed uruchomieniem), więc nick `x');fetch(...);('` wykonywał kod w sesji admina. | wysoka | `jsArg()` = `esc(JSON.stringify(s))` we wszystkich handlerach inline; `esc()` escapuje też `'`. |
| 3 | Ranga była brana z body żądania klienta – gracz mógł podać `"rank":"superadmin"` i ominąć limity. | wysoka | Ranga i SteamID klienta pochodzą z podpisanego tokenu. |
| 4 | Rate limit kluczowany po `server_ip` z body (kontrolowane przez wysyłającego) → łatwe obejście albo zablokowanie prawdziwego serwera GMod; limiter działał przed autoryzacją. | średnia | Limit per SteamID / IP / serwer, liczony tylko dla prawdziwej pracy (nowa konwersja, lookup). Osobny limiter antyfloodowy per IP. |
| 5 | `express.static(public)` udostępniał `/dev.html` publicznie, nawet przy `DEV_CONSOLE=0`. | niska | Konsola osadzona w binarce, serwowana tylko po zalogowaniu. |
| 6 | Logowanie do konsoli bez ochrony przed brute-force, sesje nigdy nie były czyszczone, brak `Secure` na cookie, brak sprawdzenia Origin. | średnia | Limit prób logowania, GC sesji, `Secure` za HTTPS, sprawdzanie Origin dla metod zmieniających stan, nagłówki `X-Frame-Options` itd. |
| 7 | Serwer startował z pustym albo przykładowym `API_SECRET`; tryb `X-SR-Test`. | średnia | Start odmawiany przy pustym/przykładowym sekrecie; `X-SR-Test` usunięty. |
| 8 | Kontener działał jako root. | niska | Użytkownik `app` (uid 10001), `cap_drop: ALL`, `no-new-privileges`. |

## Błędy logiki

| # | Problem | Naprawa |
|---|---|---|
| 9 | Dla rangi bez skonfigurowanych limitów `maxDuration = null` → **`MAX_DURATION_SECONDS` było ignorowane** i pobierały się nawet 10-godzinne filmy. | Ranga nieskonfigurowana → globalny limit; `null` w pliku → świadomie bez limitu. |
| 10 | Limit długości nie był sprawdzany przy trafieniu w cache – zwykły gracz mógł puścić 2-godzinny utwór, jeśli wcześniej skonwertował go VIP. | Limit sprawdzany także dla cache i dla żądań czekających na tę samą konwersję. |
| 11 | Zakładano, że `--match-filter` kończy się kodem 101. W rzeczywistości kończy się kodem 0 bez pliku → „za długi film” dawał błąd 500 „Downloaded file not found”. | `--break-match-filters` (kod 101), metadane drukowane na etapie `pre_process`, więc długość jest znana bez drugiego wywołania. |
| 12 | Lua: odpowiedzi 4xx trafiały do gałęzi `not success` bez parsowania body → gracz nigdy nie widział „za długi” / „zły link”, zawsze „conversion failed”. | Backend zwraca `code` (`TOO_LONG`, `DAILY_LIMIT`, `BLOCKED`, …), Lua mapuje je na dedykowane komunikaty (dodane nowe: zablokowany, limit dzienny, live). |
| 13 | `/stream`: `Range: bytes=-500` → `NaN` → wyjątek; brak obsługi `error` na `ReadStream` – usunięcie pliku w trakcie odczytu **wywalało cały proces**. | `http.ServeContent` (pełne RFC 7233, 416, If-Range, ETag) + `sendfile`. |
| 14 | `parseInt(x) \|\| default` zamieniał `AUDIO_QUALITY=0` na 5, `MAX_DURATION_SECONDS=0` było niemożliwe. | Poprawne parsowanie, 0 zachowane. |
| 15 | Wykrywanie impersonacji szukało `chrome` małymi literami, a yt-dlp wypisuje `Chrome-133` → `--impersonate` **nigdy nie był włączany**. | Poprawne wykrywanie (`YTDLP_IMPERSONATE=auto`). |
| 16 | Obraz `node:20-slim` – yt-dlp wymaga Node ≥ 22 do wyzwań JS YouTube i oznacza Node 20 jako *unsupported* → brakujące formaty / blokady. | Node 22 w obrazie (sprawdzone: `JS runtimes: node-22`). |
| 17 | Wyścig limitu dziennego: równoległe żądania przechodziły sprawdzenie przed zapisem historii. | Licznik konwersji „w locie” per gracz. |
| 18 | Deduplikacja historii (60 s) była globalna po `video_id` – gdy kilku graczy puszczało ten sam utwór, zapisywany był tylko pierwszy. | Deduplikacja per gracz + film. |
| 19 | README mówi, że `"admin": null` = bez limitów, ale zapis w konsoli dev wywalał się na `null`. | `null` obsługiwane. |
| 20 | Test konwersji w konsoli dev omijał blokadę konwersji → dwa yt-dlp pisały do tego samego pliku tymczasowego. | Konsola używa tej samej ścieżki co API (wspólne blokady i pula). |
| 21 | Playlisty YouTube/Spotify nie wypełniały się: `ENT:SetPlaylist` działa tylko na serwerze, a seeding był **po** sprawdzeniu `HasBass` (serwery zwykle nie mają gm_bass3). | Seeding przeniesiony przed sprawdzenie BASS. |
| 22 | `server_ip` wysyłany jako `ip:port` (`game.GetIPAddress()`). | Port obcinany. |
| 23 | `streamradio_core/sv_streamradio_ranks.lua` – martwy duplikat (nigdy nieładowany). Push rang ustawiał „wysłano” przed odpowiedzią → po błędzie nigdy nie ponawiał. | Duplikat usunięty, zapamiętywanie po sukcesie. |
| 24 | Wspólny plik cookies zapisywany przez równoległe procesy yt-dlp → możliwe uszkodzenie. | Każde wywołanie dostaje własną kopię; mount cookies może być `:ro`. |
| 25 | Timeout zabijał tylko yt-dlp – ffmpeg zostawał jako sierota; śmieci `.tmp.webm`/`.part` zostawały na dysku. | Grupa procesów zabijana w całości, katalog `.tmp` sprzątany. |
| 26 | Spotify: wyścig przy odświeżaniu tokenu; brak deduplikacji; zawsze pierwszy wynik wyszukiwania (często teledysk z intro albo 1-godzinny loop). | Mutex na token, singleflight, wybór wyniku o długości najbliższej oryginałowi (z 5 wyników). |
| 27 | Lua: wpis „konwersja w toku” bez limitu czasu – jeśli żądanie się zawiesiło, dany film był zablokowany do restartu. | Wpis traci ważność po 180 s. |
| 28 | Nowe formaty linków nieobsługiwane: `/shorts/`, `/live/`, `youtube-nocookie`. | Dodane (backend + Lua). Live streamy odrzucane z czytelnym błędem. |

## Wydajność

| # | Było | Jest |
|---|---|---|
| 29 | Brak limitu równoległych procesów – 50 różnych filmów = 50× yt-dlp + ffmpeg naraz. | Pula workerów + kolejka z limitem (`503 BUSY` zamiast zawieszenia hosta). |
| 30 | Film za długi = 2 wywołania yt-dlp. | 1 wywołanie: metadane + filtr + pobranie + mp3. |
| 31 | Brak indeksu na `steamid` → pełny skan tabeli przy każdej konwersji (limit dzienny) i w rankingu graczy (skorelowane podzapytanie na grupę). | Indeks `(steamid, created_at)`, ranking jednym CTE. |
| 32 | Cache wygasał 30 dni od pobrania, nawet dla utworów granych codziennie; brak limitu rozmiaru; sprzątanie max 100 plików/h; brak sprzątania sierot. | Przesuwane wygasanie, LRU z `MAX_CACHE_SIZE_MB`, sprzątanie wsadowe i sierot, przycinanie historii. |
| 33 | Każdy klient GMod pobierał tę samą playlistę osobnym wywołaniem yt-dlp. | Cache playlist (10 min) + singleflight. |
| 34 | Playlisty przez `--dump-json` (megabajty JSON). | `--print` z kilkoma polami. |
| 35 | Synchroniczne `existsSync`/`statSync` na każdy stream, bufor zdarzeń na `Array.shift()` (O(n)). | Strumieniowanie `sendfile`, bufor pierścieniowy. |
| 36 | Logi Dockera bez rotacji, brak healthchecka, `docker stop` bez obsługi. | Rotacja logów, `HEALTHCHECK`, łagodne zamykanie (SIGTERM → zamknięcie HTTP, bazy). |

### Pomiar (ta sama maszyna, te same dane, stary Node vs nowy Go)

| test | Node | Go |
|---|---|---|
| stream pliku 15 MB, 20 równoległych | 919 MB/s | **4910 MB/s** |
| stream Range 64 KB, 50 równoległych | 2 507 req/s | **26 288 req/s** |
| `/api/convert` trafienie w cache, 50 równoległych | 2 506 req/s | **3 914 req/s** (przed dodatkowym ograniczeniem zapisów TTL) |
| RAM | 110–154 MB | **~22 MB** |

Czas samej konwersji zależy głównie od YouTube i ffmpeg – tu zysk to brak podwójnych wywołań,
brak równoległych konwersji tego samego filmu i kontrolowana liczba procesów.
