local RADIOIFACE = RADIOIFACE
if not istable(RADIOIFACE) then
	StreamRadioLib.ReloadAddon()
	return
end

RADIOIFACE.name = "Spotify"
RADIOIFACE.priority = -10001
RADIOIFACE.online = true
RADIOIFACE.cache = false

RADIOIFACE.downloadTimeout = 0
RADIOIFACE.downloadFirst = false
RADIOIFACE.allowCaching = true

local g_cvDebug = CreateConVar(
	"sv_streamradio_spotify_debug",
	"0",
	bit.bor(FCVAR_ARCHIVE, FCVAR_GAMEDLL, FCVAR_REPLICATED),
	"Enable Spotify converter debug logging (0 = off, 1 = on)"
)

local function DebugLog(...)
	if not g_cvDebug:GetBool() then return end
	MsgN("[StreamRadio Spotify] ", ...)
end

-- survives lua hot reload
StreamRadioLib._spotifyResultCache = StreamRadioLib._spotifyResultCache or {}
-- don't preserve pending callbacks across reloads: old closures capture dead upvalues
StreamRadioLib._spotifyPendingCallbacks = {}

local g_resultCache = StreamRadioLib._spotifyResultCache
local g_pendingCallbacks = StreamRadioLib._spotifyPendingCallbacks
local RESULT_CACHE_TTL = 300

local function GetCachedResult(trackId)
	local entry = g_resultCache[trackId]
	if not entry then return nil end
	if CurTime() - entry.time > RESULT_CACHE_TTL then
		g_resultCache[trackId] = nil
		return nil
	end
	return entry.streamUrl
end

local function SetCachedResult(trackId, streamUrl)
	g_resultCache[trackId] = {
		streamUrl = streamUrl,
		time = CurTime(),
	}
end

local function ResolvePending(trackId, success, streamUrl, errorCode)
	local pending = g_pendingCallbacks[trackId]
	g_pendingCallbacks[trackId] = nil
	if not pending then return end
	for _, entry in ipairs(pending) do
		entry.callback(entry.selfRef, success, streamUrl, errorCode)
	end
end

local ERROR_CONVERTER_UNAVAILABLE = 120000
local ERROR_CONVERTER_FAILED = 120001
local ERROR_CONVERTER_TOO_LONG = 120002
local ERROR_CONVERTER_INVALID_URL = 120003
local ERROR_CONVERTER_RATE_LIMITED = 120004

StreamRadioLib.Error.AddStreamErrorCode({
	id = ERROR_CONVERTER_UNAVAILABLE,
	name = "STREAM_ERROR_SPOTIFY_CONVERTER_UNAVAILABLE",
	description = "[Spotify] Converter server is not configured or unreachable",
	helptext = [[
The converter server is not configured or cannot be reached.

Server admin needs to:
	1. Set up the yt-converter-server with Spotify credentials
	2. Set sv_streamradio_converter_url to the server URL
	3. Set sv_streamradio_converter_secret to the shared API secret
]],
})

StreamRadioLib.Error.AddStreamErrorCode({
	id = ERROR_CONVERTER_FAILED,
	name = "STREAM_ERROR_SPOTIFY_CONVERTER_FAILED",
	description = "[Spotify] Conversion failed",
	helptext = [[
The converter server failed to convert the Spotify track.
This could be a temporary issue. Try again later.
]],
})

StreamRadioLib.Error.AddStreamErrorCode({
	id = ERROR_CONVERTER_TOO_LONG,
	name = "STREAM_ERROR_SPOTIFY_CONVERTER_TOO_LONG",
	description = "[Spotify] Track is too long",
	helptext = [[
The track exceeds the maximum allowed duration (default: 10 minutes).
Try a shorter track.
]],
})

StreamRadioLib.Error.AddStreamErrorCode({
	id = ERROR_CONVERTER_INVALID_URL,
	name = "STREAM_ERROR_SPOTIFY_CONVERTER_INVALID_URL",
	description = "[Spotify] Invalid Spotify URL",
	helptext = [[
The provided URL is not a valid Spotify track URL.
]],
})

StreamRadioLib.Error.AddStreamErrorCode({
	id = ERROR_CONVERTER_RATE_LIMITED,
	name = "STREAM_ERROR_SPOTIFY_CONVERTER_RATE_LIMITED",
	description = "[Spotify] Rate limited",
	helptext = [[
Too many conversion requests. Please wait a moment and try again.
]],
})

local g_cvConverterUrl = CreateConVar(
	"sv_streamradio_converter_url",
	"",
	bit.bor(FCVAR_ARCHIVE, FCVAR_GAMEDLL, FCVAR_REPLICATED),
	"URL of the converter server, e.g. http://your-server:9999"
)

local g_cvConverterSecret = CreateConVar(
	"sv_streamradio_converter_secret",
	"",
	bit.bor(FCVAR_ARCHIVE, FCVAR_GAMEDLL, FCVAR_REPLICATED),
	"Shared API key for converter authentication"
)

function RADIOIFACE:CheckURL(url)
	url = string.lower(url)

	if string.find(url, "spotify.com/track/", 1, true) then return true end
	if string.find(url, "spotify:track:", 1, true) then return true end

	if string.find(url, "spotify.com/intl-", 1, true) and string.find(url, "/track/", 1, true) then
		return true
	end

	if string.find(url, "spotify.com/playlist/", 1, true) then return true end
	if string.find(url, "spotify:playlist:", 1, true) then return true end

	return false
end

local function ExtractTrackId(url)
	local id = string.match(url, "/track/(%w+)")
	if id and #id == 22 then return id end

	id = string.match(url, "spotify:track:(%w+)")
	if id and #id == 22 then return id end

	return nil
end

local function IsPlaylistURL(url)
	url = string.lower(url)
	if string.find(url, "spotify.com/playlist/", 1, true) then return true end
	if string.find(url, "spotify:playlist:", 1, true) then return true end
	return false
end

local function GetConverterConfig()
	return string.Trim(g_cvConverterUrl:GetString()), string.Trim(g_cvConverterSecret:GetString())
end

-- returns {ip, rank} for the given player
-- rank = ULX/ULib usergroup ("user", "admin", "superadmin", custom, ...)
local function GetPlayerInfo(ply)
	if not IsValid(ply) or not ply:IsPlayer() then return nil end

	local raw = ply:IPAddress() or ""
	local ip = string.match(raw, "^(.+):%d+$") or raw

	local rank = ply:GetUserGroup() or "user"

	return { ip = ip, rank = rank }
end

local function BuildHeaders(secret)
	local h = { ["Content-Type"] = "application/json" }
	if secret ~= "" then h["X-SR-Key"] = secret end
	return h
end

-- raw spotify: URIs are flaky here, stick to normal track urls
local function BuildTrackPlaybackUrl(trackData)
	local trackId = string.Trim(tostring(trackData.track_id or trackData.trackId or ""))
	if trackId ~= "" then
		return "https://open.spotify.com/track/" .. trackId
	end

	return string.Trim(tostring(trackData.url or ""))
end

-- same flow as yt: first track now, rest stay playlist data

function RADIOIFACE:ConvertPlaylist(url, callback, context)
	local realm = SERVER and "SERVER" or "CLIENT"
	DebugLog(realm, " Playlist convert called for: ", url)

	local converterUrl, converterSecret = GetConverterConfig()
	if converterUrl == "" then
		DebugLog(realm, " ERROR: sv_streamradio_converter_url is not set!")
		callback(self, false, nil, ERROR_CONVERTER_UNAVAILABLE)
		return
	end

	local body = { url = url }
	local headers = BuildHeaders(converterSecret)
	local apiUrl = string.TrimRight(converterUrl, "/") .. "/api/playlist"
	DebugLog(realm, " Fetching playlist: ", apiUrl)

	StreamRadioLib.Http.RequestRaw(apiUrl, function(success, data)
		if not success then
			DebugLog(realm, " Playlist fetch failed")
			callback(self, false, nil, ERROR_CONVERTER_FAILED)
			return
		end

		local rd = util.JSONToTable(data.body or "")
		if not rd or not rd.tracks or #rd.tracks == 0 then
			DebugLog(realm, " Empty or invalid playlist")
			callback(self, false, nil, ERROR_CONVERTER_INVALID_URL)
			return
		end

		DebugLog(realm, " Playlist loaded: ", #rd.tracks, " tracks")

		local playlistItems = {}
		for i, t in ipairs(rd.tracks) do
			local trackUrl = BuildTrackPlaybackUrl(t)
			if trackUrl == "" then
				trackUrl = string.Trim(tostring(t.url or ""))
			end

			playlistItems[i] = {
				url = trackUrl,
				name = t.title or trackUrl,
			}
		end

		local firstTrack = playlistItems[1]
		self:ConvertSingleTrack(firstTrack.url, converterUrl, converterSecret, function(streamUrl, errCode)
			if not streamUrl then
				callback(self, false, nil, errCode or ERROR_CONVERTER_FAILED)
				return
			end

			-- stream.lua reads arg #5 and seeds the entity playlist
			callback(self, true, streamUrl, nil, {
				tracks = playlistItems,
				currentIndex = 1,
			})
		end, context)
	end, util.TableToJSON(body), "POST", headers, "application/json")
end

function RADIOIFACE:ConvertSingleTrack(trackUrl, converterUrl, converterSecret, resultCallback, context)
	local realm = SERVER and "SERVER" or "CLIENT"

	local bodyTable = {
		url = trackUrl,
		nick = "",
		steamid = "",
		server_ip = "",
		player_ip = "",
		rank = "default",
	}

	if CLIENT then
		local ply = LocalPlayer()
		if IsValid(ply) then
			bodyTable.nick = ply:Nick() or ""
			bodyTable.steamid = ply:SteamID() or ""
			-- GetUserGroup() is networked by ULib to clients
			bodyTable.rank = ply:GetUserGroup() or "user"
		end
	end

	if SERVER then
		bodyTable.server_ip = game.GetIPAddress() or ""
		local ctxPly = context and context.ply or nil
		local info = GetPlayerInfo(ctxPly)
		if info then
			bodyTable.player_ip = info.ip
			bodyTable.rank = info.rank
		end
	end

	local headers = BuildHeaders(converterSecret)
	local apiUrl = string.TrimRight(converterUrl, "/") .. "/api/convert"

	StreamRadioLib.Http.RequestRaw(apiUrl, function(success, data)
		if not success then
			local code = data and data.code or -1
			DebugLog(realm, " Track convert failed, code=", tostring(code))
			local errCode = ERROR_CONVERTER_FAILED
			if code == 401 then errCode = ERROR_CONVERTER_UNAVAILABLE end
			if code == 429 then errCode = ERROR_CONVERTER_RATE_LIMITED end
			resultCallback(nil, errCode)
			return
		end

		local rd = util.JSONToTable(data.body or "")
		if not rd or not rd.success or not rd.stream_url then
			resultCallback(nil, ERROR_CONVERTER_FAILED)
			return
		end

		local streamUrl = string.TrimRight(converterUrl, "/") .. rd.stream_url
		local tid = rd.track_id
		if tid then
			SetCachedResult(tid, streamUrl)
		end

		DebugLog(realm, " Track ready: ", rd.title or "", " -> ", streamUrl)
		resultCallback(streamUrl)
	end, util.TableToJSON(bodyTable), "POST", headers, "application/json")
end

function RADIOIFACE:Convert(url, callback, context)
	local realm = SERVER and "SERVER" or "CLIENT"
	DebugLog(realm, " Convert called for: ", url)

	if IsPlaylistURL(url) then
		self:ConvertPlaylist(url, callback, context)
		return
	end

	local converterUrl = string.Trim(g_cvConverterUrl:GetString())
	local converterSecret = string.Trim(g_cvConverterSecret:GetString())

	if converterUrl == "" then
		DebugLog(realm, " ERROR: sv_streamradio_converter_url is not set!")
		callback(self, false, nil, ERROR_CONVERTER_UNAVAILABLE)
		return
	end

	local trackId = ExtractTrackId(url)

	if trackId then
		local cachedUrl = GetCachedResult(trackId)
		if cachedUrl then
			DebugLog(realm, " Using cached result for ", trackId, ": ", cachedUrl)
			callback(self, true, cachedUrl)
			return
		end

		-- same id already in flight, piggyback
		if g_pendingCallbacks[trackId] then
			DebugLog(realm, " Conversion already in-flight for ", trackId, ", queuing callback")
			table.insert(g_pendingCallbacks[trackId], { selfRef = self, callback = callback })
			return
		end

		g_pendingCallbacks[trackId] = { { selfRef = self, callback = callback } }
	end

	DebugLog(realm, " Converter URL: ", converterUrl)

	local bodyTable = {
		url = url,
		nick = "",
		steamid = "",
		server_ip = "",
		player_ip = "",
		rank = "default",
	}

	if CLIENT then
		local ply = LocalPlayer()
		if IsValid(ply) then
			bodyTable.nick = ply:Nick() or ""
			bodyTable.steamid = ply:SteamID() or ""
			bodyTable.rank = ply:GetUserGroup() or "user"
		end
	end

	if SERVER then
		bodyTable.server_ip = game.GetIPAddress() or ""
		local ctxPly = context and context.ply or nil
		local info = GetPlayerInfo(ctxPly)
		if info then
			bodyTable.player_ip = info.ip
			bodyTable.rank = info.rank
		end
	end

	local bodyJson = util.TableToJSON(bodyTable)

	local headers = {
		["Content-Type"] = "application/json",
	}

	if converterSecret ~= "" then
		headers["X-SR-Key"] = converterSecret
	end

	local apiUrl = string.TrimRight(converterUrl, "/") .. "/api/convert"
	DebugLog(realm, " POST -> ", apiUrl)

	StreamRadioLib.Http.RequestRaw(apiUrl, function(success, data)
		local code = data and data.code or -1
		DebugLog(realm, " Response: success=", tostring(success), " code=", tostring(code))

		if not success then
			if code == 401 then
				DebugLog(realm, " ERROR: Authentication failed (401). Check sv_streamradio_converter_secret")
			elseif code == 429 then
				DebugLog(realm, " ERROR: Rate limited (429)")
			else
				DebugLog(realm, " ERROR: HTTP request failed. Code: ", tostring(code))
				if data and data.body then
					DebugLog(realm, " Response body: ", string.sub(data.body, 1, 500))
				end
			end

			local errCode = ERROR_CONVERTER_FAILED
			if code == 401 then errCode = ERROR_CONVERTER_UNAVAILABLE end
			if code == 429 then errCode = ERROR_CONVERTER_RATE_LIMITED end

			if trackId then
				ResolvePending(trackId, false, nil, errCode)
			else
				callback(self, false, nil, errCode)
			end
			return
		end

		local responseBody = data.body or ""
		DebugLog(realm, " Response body: ", string.sub(responseBody, 1, 500))

		local responseData = util.JSONToTable(responseBody)

		if not responseData then
			DebugLog(realm, " ERROR: Failed to parse JSON response")
			if trackId then
				ResolvePending(trackId, false, nil, ERROR_CONVERTER_FAILED)
			else
				callback(self, false, nil, ERROR_CONVERTER_FAILED)
			end
			return
		end

		if not responseData.success then
			local errorMsg = responseData.error or "Unknown error"
			DebugLog(realm, " ERROR: Converter returned error: ", errorMsg)

			local errCode = ERROR_CONVERTER_FAILED
			if string.find(errorMsg, "too long", 1, true) or string.find(errorMsg, "Too long", 1, true) then
				errCode = ERROR_CONVERTER_TOO_LONG
			elseif string.find(errorMsg, "Invalid", 1, true) then
				errCode = ERROR_CONVERTER_INVALID_URL
			elseif string.find(errorMsg, "not configured", 1, true) then
				errCode = ERROR_CONVERTER_UNAVAILABLE
			end

			if trackId then
				ResolvePending(trackId, false, nil, errCode)
			else
				callback(self, false, nil, errCode)
			end
			return
		end

		local streamPath = responseData.stream_url
		if not streamPath or streamPath == "" then
			DebugLog(realm, " ERROR: No stream_url in response")
			if trackId then
				ResolvePending(trackId, false, nil, ERROR_CONVERTER_FAILED)
			else
				callback(self, false, nil, ERROR_CONVERTER_FAILED)
			end
			return
		end

		local streamUrl = string.TrimRight(converterUrl, "/") .. streamPath

		local tid = responseData.track_id or trackId
		if tid then
			SetCachedResult(tid, streamUrl)
		end

		DebugLog(realm, " SUCCESS: ", responseData.title or "", " -> ", streamUrl)
		DebugLog(realm, " Duration: ", tostring(responseData.duration or 0), "s, Cached: ", tostring(responseData.cached))

		if trackId then
			ResolvePending(trackId, true, streamUrl)
		else
			callback(self, true, streamUrl)
		end
	end, bodyJson, "POST", headers, "application/json")
end

return true
