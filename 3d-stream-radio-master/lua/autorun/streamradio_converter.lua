-- streamradio_converter.lua (shared)
-- Common code for the YouTube / Spotify interfaces talking to yt-converter-server.
--
-- Security model:
--   * sv_streamradio_converter_secret lives on the SERVER only (FCVAR_PROTECTED,
--     not replicated). Before, it was FCVAR_REPLICATED, so every player could
--     read it from the console and use the backend directly.
--   * The server signs a short-lived token per player:
--       "v1|<steamid>|<usergroup>|<expires unix>|<hex HMAC-SHA256(secret, first four fields)>"
--     and sends it to that player via net message. Clients send it as X-SR-Token.
--     The backend takes steamid and rank from the token, so they can't be spoofed.

if SERVER then
	AddCSLuaFile()
end

StreamRadioConverter = StreamRadioConverter or {}
local SRC = StreamRadioConverter

local NET_TOKEN = "StreamRadio_ConverterToken"
local NET_TOKEN_REQUEST = "StreamRadio_ConverterTokenReq"

local TOKEN_TTL = 6 * 60 * 60
local TOKEN_REFRESH_INTERVAL = 30 * 60
local RESULT_CACHE_TTL = 300
local PENDING_STALE_AFTER = 180

-- ---------------------------------------------------------------------------
-- convars
-- ---------------------------------------------------------------------------

SRC.cvUrl = GetConVar("sv_streamradio_converter_url") or CreateConVar(
	"sv_streamradio_converter_url",
	"",
	bit.bor(FCVAR_ARCHIVE, FCVAR_GAMEDLL, FCVAR_REPLICATED),
	"URL of the converter server, e.g. http://your-server:9999"
)

if SERVER then
	SRC.cvSecret = GetConVar("sv_streamradio_converter_secret") or CreateConVar(
		"sv_streamradio_converter_secret",
		"",
		bit.bor(FCVAR_ARCHIVE, FCVAR_GAMEDLL, FCVAR_PROTECTED, FCVAR_DONTRECORD),
		"Shared API key for converter authentication (server only, never sent to clients)"
	)
end

function SRC.GetBaseUrl()
	local url = string.Trim(SRC.cvUrl:GetString())
	return (string.TrimRight(url, "/"))
end

local function StripPort(addr)
	addr = tostring(addr or "")
	return string.match(addr, "^(%d+%.%d+%.%d+%.%d+):%d+$") or addr
end

-- ---------------------------------------------------------------------------
-- HMAC-SHA256 on top of util.SHA256 (hex output)
-- ---------------------------------------------------------------------------

local function HexToBin(hex)
	return (string.gsub(hex, "%x%x", function(c)
		return string.char(tonumber(c, 16))
	end))
end

local function HmacSha256(key, msg)
	if #key > 64 then
		key = HexToBin(util.SHA256(key))
	end
	key = key .. string.rep("\0", 64 - #key)

	local ipad, opad = {}, {}
	for i = 1, 64 do
		local b = string.byte(key, i)
		ipad[i] = string.char(bit.bxor(b, 0x36))
		opad[i] = string.char(bit.bxor(b, 0x5c))
	end

	local inner = HexToBin(util.SHA256(table.concat(ipad) .. msg))
	return string.lower(util.SHA256(table.concat(opad) .. inner))
end

SRC.HmacSha256 = HmacSha256

-- RFC 4231-style sanity check, so a broken util.SHA256 shows up in the log
-- instead of as mysterious 401s.
do
	local ok, res = pcall(HmacSha256, "key", "The quick brown fox jumps over the lazy dog")
	SRC.HmacOk = ok and res == "f7bc83f430538424b13298e6aa6fb143ef4d59a14946175997479dbc2d1a3cd8"
	if not SRC.HmacOk then
		MsgN("[StreamRadio Converter] WARNING: HMAC self-test failed, client tokens will not work: ", tostring(res))
	end
end

-- ---------------------------------------------------------------------------
-- client tokens
-- ---------------------------------------------------------------------------

if SERVER then
	util.AddNetworkString(NET_TOKEN)
	util.AddNetworkString(NET_TOKEN_REQUEST)

	local function SafeRank(rank)
		rank = tostring(rank or "user")
		if #rank < 1 or #rank > 32 or not string.match(rank, "^[%w_%-]+$") then
			return "user"
		end
		return rank
	end

	function SRC.MakeToken(ply)
		if not IsValid(ply) or not ply:IsPlayer() or ply:IsBot() then return nil end

		local secret = string.Trim(SRC.cvSecret:GetString())
		if secret == "" then return nil end

		local steamid = ply:SteamID()
		if not isstring(steamid) or not string.match(steamid, "^STEAM_%d:%d:%d+$") then return nil end

		local exp = os.time() + TOKEN_TTL
		local payload = "v1|" .. steamid .. "|" .. SafeRank(ply:GetUserGroup()) .. "|" .. exp
		return payload .. "|" .. HmacSha256(secret, payload), exp
	end

	function SRC.SendToken(ply)
		local token, exp = SRC.MakeToken(ply)
		if not token then return end

		net.Start(NET_TOKEN)
		net.WriteString(token)
		net.WriteUInt(TOKEN_TTL, 32)
		net.Send(ply)
	end

	local function SendToAll()
		for _, ply in ipairs(player.GetHumans()) do
			SRC.SendToken(ply)
		end
	end

	hook.Add("PlayerInitialSpawn", "StreamRadioConverter_Token", function(ply)
		timer.Simple(1, function()
			if IsValid(ply) then SRC.SendToken(ply) end
		end)
	end)

	-- rank changed -> new token so the backend applies the right limits
	local function OnGroupChanged(ply)
		if isentity(ply) and IsValid(ply) then
			timer.Simple(0.5, function()
				if IsValid(ply) then SRC.SendToken(ply) end
			end)
		else
			timer.Create("StreamRadioConverter_TokenGroupResync", 1, 1, SendToAll)
		end
	end

	hook.Add("CAMI.PlayerUsergroupChanged", "StreamRadioConverter_Token", OnGroupChanged)
	hook.Add("ULibUserGroupChanged", "StreamRadioConverter_Token", function() OnGroupChanged(nil) end)

	timer.Create("StreamRadioConverter_TokenRefresh", TOKEN_REFRESH_INTERVAL, 0, SendToAll)

	local lastRequest = {}
	net.Receive(NET_TOKEN_REQUEST, function(_, ply)
		if not IsValid(ply) then return end
		local now = RealTime()
		if lastRequest[ply] and now - lastRequest[ply] < 10 then return end
		lastRequest[ply] = now
		SRC.SendToken(ply)
	end)

	hook.Add("PlayerDisconnected", "StreamRadioConverter_Token", function(ply)
		lastRequest[ply] = nil
	end)

	-- lua refresh: hand out fresh tokens right away
	timer.Simple(0, SendToAll)
end

if CLIENT then
	SRC._token = SRC._token
	SRC._tokenRefreshAt = SRC._tokenRefreshAt or 0

	net.Receive(NET_TOKEN, function()
		SRC._token = net.ReadString()
		local ttl = net.ReadUInt(32)
		-- relative to our own clock, so client/server clock skew doesn't matter
		SRC._tokenRefreshAt = RealTime() + math.max(ttl - 600, 60)
	end)

	local lastAsk = 0
	function SRC.RequestToken()
		local now = RealTime()
		if now - lastAsk < 5 then return end
		lastAsk = now
		net.Start(NET_TOKEN_REQUEST)
		net.SendToServer()
	end

	function SRC.HasValidToken()
		return isstring(SRC._token) and SRC._token ~= "" and RealTime() < SRC._tokenRefreshAt
	end

	function SRC.InvalidateToken()
		SRC._token = nil
		SRC._tokenRefreshAt = 0
	end
end

-- calls back with a header table, or nil if no auth is available (yet)
function SRC.GetAuthHeaders(callback)
	local headers = { ["Content-Type"] = "application/json" }

	if SERVER then
		local secret = string.Trim(SRC.cvSecret:GetString())
		if secret == "" then
			callback(nil)
			return
		end
		headers["X-SR-Key"] = secret
		callback(headers)
		return
	end

	if SRC.HasValidToken() then
		headers["X-SR-Token"] = SRC._token
		callback(headers)
		return
	end

	-- just joined / token expired: ask the server and wait a few seconds
	SRC.RequestToken()
	local timerName = "StreamRadioConverter_WaitToken_" .. tostring(math.random(1, 2 ^ 30))
	local tries = 0
	timer.Create(timerName, 0.25, 40, function()
		tries = tries + 1
		if SRC.HasValidToken() then
			timer.Remove(timerName)
			headers["X-SR-Token"] = SRC._token
			callback(headers)
		elseif tries >= 40 then
			callback(nil)
		elseif tries % 20 == 0 then
			SRC.RequestToken()
		end
	end)
end

-- ---------------------------------------------------------------------------
-- request helpers
-- ---------------------------------------------------------------------------

function SRC.BuildBody(url, context)
	local body = {
		url = url,
		nick = "",
		steamid = "",
		rank = "",
		server_ip = "",
		player_ip = "",
	}

	if SERVER then
		body.server_ip = StripPort(game.GetIPAddress())

		local ply = context and context.ply or nil
		if IsValid(ply) and ply:IsPlayer() then
			body.nick = ply:Nick() or ""
			body.steamid = ply:SteamID() or ""
			body.rank = ply:GetUserGroup() or "user"
			body.player_ip = StripPort(ply:IPAddress())
		end
	else
		-- steamid + rank come from the signed token, nick is informational
		local ply = LocalPlayer()
		if IsValid(ply) then
			body.nick = ply:Nick() or ""
		end
	end

	return body
end

-- maps backend error codes (JSON "code") to the interface's error ids
function SRC.MapError(errs, httpCode, rd)
	local code = istable(rd) and rd.code or nil

	if code == "TOO_LONG" then return errs.TOO_LONG end
	if code == "LIVE_UNSUPPORTED" then return errs.LIVE or errs.FAILED end
	if code == "INVALID_URL" then return errs.INVALID_URL end
	if code == "BLOCKED" then return errs.BLOCKED or errs.FAILED end
	if code == "DAILY_LIMIT" then return errs.DAILY_LIMIT or errs.RATE_LIMITED end
	if code == "RATE_LIMITED" or code == "BUSY" then return errs.RATE_LIMITED end
	if code == "UNAUTHORIZED" or code == "SPOTIFY_DISABLED" then return errs.UNAVAILABLE end

	if httpCode == 401 or httpCode == 403 then return errs.UNAVAILABLE end
	if httpCode == 429 or httpCode == 503 then return errs.RATE_LIMITED end
	if not httpCode or httpCode < 0 then return errs.UNAVAILABLE end

	return errs.FAILED
end

-- POST <base>/<path> with auth. callback(ok, responseTable, httpCode)
function SRC.Post(path, body, callback, debugLog, isRetry)
	local base = SRC.GetBaseUrl()
	if base == "" then
		debugLog("sv_streamradio_converter_url is not set")
		callback(false, nil, -1)
		return
	end

	SRC.GetAuthHeaders(function(headers)
		if not headers then
			debugLog(SERVER and "sv_streamradio_converter_secret is not set" or "no converter token received from server")
			callback(false, { code = "UNAUTHORIZED" }, 401)
			return
		end

		local apiUrl = base .. path
		debugLog("POST ", apiUrl)

		StreamRadioLib.Http.RequestRaw(apiUrl, function(success, data)
			local httpCode = data and data.code or -1
			local rd = util.JSONToTable(data and data.body or "") or nil

			-- token rejected (e.g. secret rotated): fetch a new one, retry once
			if CLIENT and httpCode == 401 and not isRetry then
				SRC.InvalidateToken()
				SRC.Post(path, body, callback, debugLog, true)
				return
			end

			if not success or not rd or rd.success == false then
				debugLog("request failed: http=", tostring(httpCode), " code=", tostring(rd and rd.code), " error=", tostring(rd and rd.error))
				callback(false, rd, httpCode)
				return
			end

			callback(true, rd, httpCode)
		end, util.TableToJSON(body), "POST", headers, "application/json")
	end)
end

-- ---------------------------------------------------------------------------
-- per-interface resolver with result cache + in-flight de-duplication
-- ---------------------------------------------------------------------------

function SRC.NewResolver(name)
	SRC._resolvers = SRC._resolvers or {}
	local r = SRC._resolvers[name] or {}
	SRC._resolvers[name] = r

	r.cache = r.cache or {}
	-- pending callbacks are never kept across lua refresh (stale closures)
	r.pending = {}

	function r:GetCached(key)
		local e = self.cache[key]
		if not e then return nil end
		if SysTime() - e.time > RESULT_CACHE_TTL or e.base ~= SRC.GetBaseUrl() then
			self.cache[key] = nil
			return nil
		end
		return e.streamUrl
	end

	function r:SetCached(key, streamUrl)
		self.cache[key] = { streamUrl = streamUrl, time = SysTime(), base = SRC.GetBaseUrl() }
	end

	-- callback(ok, streamUrl, errId, response)
	function r:ConvertTrack(key, trackUrl, context, errs, debugLog, callback)
		if key then
			local cached = self:GetCached(key)
			if cached then
				debugLog("using cached result for ", key)
				callback(true, cached, nil, nil)
				return
			end

			local p = self.pending[key]
			if p and SysTime() - p.started < PENDING_STALE_AFTER then
				debugLog("conversion already in flight for ", key)
				table.insert(p.callbacks, callback)
				return
			end

			self.pending[key] = { started = SysTime(), callbacks = { callback } }
		end

		local function finish(ok, streamUrl, errId, rd)
			if not key then
				callback(ok, streamUrl, errId, rd)
				return
			end

			local p = self.pending[key]
			self.pending[key] = nil
			if not p then return end

			for _, cb in ipairs(p.callbacks) do
				cb(ok, streamUrl, errId, rd)
			end
		end

		SRC.Post("/api/convert", SRC.BuildBody(trackUrl, context), function(ok, rd, httpCode)
			if not ok then
				finish(false, nil, SRC.MapError(errs, httpCode, rd), rd)
				return
			end

			local streamPath = rd.stream_url
			if not isstring(streamPath) or streamPath == "" then
				finish(false, nil, errs.FAILED, rd)
				return
			end

			local streamUrl = SRC.GetBaseUrl() .. streamPath
			if key then
				self:SetCached(key, streamUrl)
			end

			debugLog("ready: ", tostring(rd.title), " (", tostring(rd.duration), "s, cached=", tostring(rd.cached), ") -> ", streamUrl)
			finish(true, streamUrl, nil, rd)
		end, debugLog)
	end

	-- callback(ok, tracks, errId, startIndex)
	-- the backend already drops entries the player can't play (too long for
	-- their rank, deleted, private) and tells us where watch?v=X&list=Y starts
	function r:FetchPlaylist(url, context, errs, debugLog, callback)
		SRC.Post("/api/playlist", SRC.BuildBody(url, context), function(ok, rd, httpCode)
			if not ok then
				callback(false, nil, SRC.MapError(errs, httpCode, rd))
				return
			end

			if not istable(rd.tracks) or #rd.tracks == 0 then
				callback(false, nil, errs.INVALID_URL)
				return
			end

			local start = math.Clamp(tonumber(rd.start_index) or 1, 1, #rd.tracks)
			debugLog("playlist loaded: ", #rd.tracks, " tracks, ", tonumber(rd.skipped) or 0, " skipped, start at #", start)
			callback(true, rd.tracks, nil, start)
		end, debugLog)
	end

	return r
end

-- ---------------------------------------------------------------------------
-- playlist helpers (server)
-- ---------------------------------------------------------------------------

-- errors that are specific to one track: a playlist should skip it.
-- (unavailable / rate limited / blocked / daily limit would fail the next
-- track too, so those stop the playlist instead of cascading through it)
SRC._itemErrors = SRC._itemErrors or {}

function SRC.IsItemError(errId)
	return SRC._itemErrors[tonumber(errId) or -1] == true
end

function SRC.IsConverterURL(url)
	url = string.lower(tostring(url or ""))
	return string.find(url, "youtu", 1, true) ~= nil or string.find(url, "spotify", 1, true) ~= nil
end

-- asks the backend to convert a track in the background (next playlist
-- item), so the switch is instant. Fire and forget, server only.
function SRC.Prefetch(url, context)
	if not SERVER or not SRC.IsConverterURL(url) then return end
	SRC.Post("/api/prefetch", SRC.BuildBody(url, context), function() end, function() end)
end

-- registers the stream error codes shared by both interfaces
function SRC.RegisterErrors(baseId, prefix, label, thing)
	local LIBError = StreamRadioLib.Error
	local errs = {
		UNAVAILABLE = baseId,
		FAILED = baseId + 1,
		TOO_LONG = baseId + 2,
		INVALID_URL = baseId + 3,
		RATE_LIMITED = baseId + 4,
		BLOCKED = baseId + 5,
		DAILY_LIMIT = baseId + 6,
		LIVE = baseId + 7,
	}

	local defs = {
		{ errs.UNAVAILABLE, "CONVERTER_UNAVAILABLE", "Converter server is not configured or unreachable", [[
The converter server is not configured or cannot be reached.

Server admin needs to:
	1. Run yt-converter-server (docker compose up -d)
	2. Set sv_streamradio_converter_url to the server URL
	3. Set sv_streamradio_converter_secret to the API_SECRET of the server
]] },
		{ errs.FAILED, "CONVERTER_FAILED", "Conversion failed", "The converter server failed to convert the " .. thing .. ".\nThis could be a temporary issue. Try again later." },
		{ errs.TOO_LONG, "CONVERTER_TOO_LONG", thing .. " is too long", "The " .. thing .. " exceeds the maximum duration allowed for your rank.\nTry a shorter one." },
		{ errs.INVALID_URL, "CONVERTER_INVALID_URL", "Invalid " .. label .. " URL", "The provided URL is not a valid " .. label .. " URL." },
		{ errs.RATE_LIMITED, "CONVERTER_RATE_LIMITED", "Rate limited", "Too many conversion requests or the converter is busy.\nPlease wait a moment and try again." },
		{ errs.BLOCKED, "CONVERTER_BLOCKED", "You are blocked", "Your SteamID has been blocked from using the converter by the server admin." },
		{ errs.DAILY_LIMIT, "CONVERTER_DAILY_LIMIT", "Daily limit reached", "You reached the daily conversion limit of your rank.\nAlready converted tracks still play." },
		{ errs.LIVE, "CONVERTER_LIVE", "Live streams are not supported", "Live streams can't be converted. Try a normal video." },
	}

	for _, id in ipairs({ errs.FAILED, errs.TOO_LONG, errs.INVALID_URL, errs.LIVE }) do
		SRC._itemErrors[id] = true
	end

	for _, d in ipairs(defs) do
		LIBError.AddStreamErrorCode({
			id = d[1],
			name = "STREAM_ERROR_" .. prefix .. "_" .. d[2],
			description = "[" .. label .. "] " .. d[3],
			helptext = d[4],
		})
	end

	return errs
end

return SRC
