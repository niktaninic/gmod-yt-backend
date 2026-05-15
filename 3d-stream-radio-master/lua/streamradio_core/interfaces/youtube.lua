local RADIOIFACE = RADIOIFACE
if not istable(RADIOIFACE) then
	StreamRadioLib.ReloadAddon()
	return
end

RADIOIFACE.name = "YouTube"
RADIOIFACE.priority = -10000
RADIOIFACE.online = true
RADIOIFACE.cache = false

RADIOIFACE.downloadTimeout = 0
RADIOIFACE.downloadFirst = false
RADIOIFACE.allowCaching = true

local g_cvDebug = CreateConVar(
	"sv_streamradio_yt_debug",
	"0",
	bit.bor(FCVAR_ARCHIVE, FCVAR_GAMEDLL, FCVAR_REPLICATED),
	"Enable YouTube converter debug logging (0 = off, 1 = on)"
)

local function DebugLog(...)
	if not g_cvDebug:GetBool() then return end
	MsgN("[StreamRadio YT] ", ...)
end

-- survives lua hot reload
StreamRadioLib._ytResultCache = StreamRadioLib._ytResultCache or {}
-- don't preserve pending callbacks across reloads: old closures capture dead upvalues
StreamRadioLib._ytPendingCallbacks = {}

local g_resultCache = StreamRadioLib._ytResultCache
local g_pendingCallbacks = StreamRadioLib._ytPendingCallbacks
local RESULT_CACHE_TTL = 300

local function GetCachedResult(videoId)
	local entry = g_resultCache[videoId]
	if not entry then return nil end
	if CurTime() - entry.time > RESULT_CACHE_TTL then
		g_resultCache[videoId] = nil
		return nil
	end
	return entry.streamUrl
end

local function SetCachedResult(videoId, streamUrl)
	g_resultCache[videoId] = {
		streamUrl = streamUrl,
		time = CurTime(),
	}
end

local function ResolvePending(videoId, success, streamUrl, errorCode)
	local pending = g_pendingCallbacks[videoId]
	g_pendingCallbacks[videoId] = nil
	if not pending then return end
	for _, entry in ipairs(pending) do
		entry.callback(entry.selfRef, success, streamUrl, errorCode)
	end
end

local ERROR_CONVERTER_UNAVAILABLE = 110000
local ERROR_CONVERTER_FAILED = 110001
local ERROR_CONVERTER_TOO_LONG = 110002
local ERROR_CONVERTER_INVALID_URL = 110003
local ERROR_CONVERTER_RATE_LIMITED = 110004

StreamRadioLib.Error.AddStreamErrorCode({
	id = ERROR_CONVERTER_UNAVAILABLE,
	name = "STREAM_ERROR_YT_CONVERTER_UNAVAILABLE",
	description = "[YouTube] Converter server is not configured or unreachable",
	helptext = [[
The converter server is not configured or cannot be reached.

Server admin needs to:
	1. Set up the yt-converter-server
	2. Set sv_streamradio_converter_url to the server URL
	3. Set sv_streamradio_converter_secret to the shared API secret
]],
})

StreamRadioLib.Error.AddStreamErrorCode({
	id = ERROR_CONVERTER_FAILED,
	name = "STREAM_ERROR_YT_CONVERTER_FAILED",
	description = "[YouTube] Conversion failed",
	helptext = [[
The YouTube converter server failed to convert the video.
This could be a temporary issue. Try again later.
]],
})

StreamRadioLib.Error.AddStreamErrorCode({
	id = ERROR_CONVERTER_TOO_LONG,
	name = "STREAM_ERROR_YT_CONVERTER_TOO_LONG",
	description = "[YouTube] Video is too long",
	helptext = [[
The video exceeds the maximum allowed duration (default: 10 minutes).
Try a shorter video.
]],
})

StreamRadioLib.Error.AddStreamErrorCode({
	id = ERROR_CONVERTER_INVALID_URL,
	name = "STREAM_ERROR_YT_CONVERTER_INVALID_URL",
	description = "[YouTube] Invalid YouTube URL",
	helptext = [[
The provided URL is not a valid YouTube video URL.
]],
})

StreamRadioLib.Error.AddStreamErrorCode({
	id = ERROR_CONVERTER_RATE_LIMITED,
	name = "STREAM_ERROR_YT_CONVERTER_RATE_LIMITED",
	description = "[YouTube] Rate limited",
	helptext = [[
Too many conversion requests. Please wait a moment and try again.
]],
})

-- spotify.lua may load first (alphabetical); avoid duplicate convar warnings
local g_cvConverterUrl = GetConVar("sv_streamradio_converter_url") or CreateConVar(
	"sv_streamradio_converter_url",
	"",
	bit.bor(FCVAR_ARCHIVE, FCVAR_GAMEDLL, FCVAR_REPLICATED),
	"URL of the converter server, e.g. http://your-server:9999"
)

local g_cvConverterSecret = GetConVar("sv_streamradio_converter_secret") or CreateConVar(
	"sv_streamradio_converter_secret",
	"",
	bit.bor(FCVAR_ARCHIVE, FCVAR_GAMEDLL, FCVAR_REPLICATED),
	"Shared API key for converter authentication"
)

local YoutubeURLs = {
	"youtube://",
	"yt://",
	"://youtube.",
	".youtube.",
	"://youtu.be",
}

function RADIOIFACE:CheckURL(url)
	for i, v in ipairs(YoutubeURLs) do
		local result = string.find(string.lower(url), v, 1, true)

		if not result then
			continue
		end

		return true
	end

	return false
end

local function ExtractVideoId(url)
	-- patterns match on lowercased url but ID is extracted from original (IDs are case-sensitive)
	local low = string.lower(url)

	local s, e = string.find(low, "[?&]v=")
	if s then
		local id = string.match(url, "[?&]v=([%w_-]+)", s)
		if id and #id == 11 then return id end
	end

	s = string.find(low, "youtu%.be/")
	if s then
		local id = string.match(url, "youtu%.be/([%w_-]+)", s)
		if id and #id == 11 then return id end
	end

	s = string.find(low, "youtube%.com/embed/")
	if s then
		local id = string.match(url, "youtube%.com/embed/([%w_-]+)", s)
		if id and #id == 11 then return id end
	end

	s = string.find(low, "youtube%.com/v/")
	if s then
		local id = string.match(url, "youtube%.com/v/([%w_-]+)", s)
		if id and #id == 11 then return id end
	end

	return nil
end

local function IsPlaylistURL(url)
	url = string.lower(url)
	if string.find(url, "youtube%.com/playlist") then return true end
	if string.find(url, "[?&]list=") then return true end
	return false
end

local function GetConverterConfig()
	return string.Trim(g_cvConverterUrl:GetString()), string.Trim(g_cvConverterSecret:GetString())
end

-- returns {ip, rank} for the given player
-- rank = ULX/ULib usergroup ("user", "admin", "superadmin", custom, ...)
local function GetPlayerInfo(ply)
	if not IsValid(ply) or not ply:IsPlayer() then return nil end

	-- IPAddress() returns "ip:port", strip port
	local raw = ply:IPAddress() or ""
	local ip = string.match(raw, "^(.+):%d+$") or raw

	-- GetUserGroup() is the standard ULib/ULX accessor
	-- returns "user" for regular players, or whatever group the admin set
	local rank = ply:GetUserGroup() or "user"

	return { ip = ip, rank = rank }
end

local function BuildHeaders(secret)
	local h = { ["Content-Type"] = "application/json" }
	if secret ~= "" then h["X-SR-Key"] = secret end
	return h
end

-- convert first track now, hand the rest to entity playlist

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
			playlistItems[i] = {
				url = t.url,
				name = t.title or t.url,
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
		local vid = rd.video_id
		if vid then
			SetCachedResult(vid, streamUrl)
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

	local videoId = ExtractVideoId(url)

	if videoId then
		local cachedUrl = GetCachedResult(videoId)
		if cachedUrl then
			DebugLog(realm, " Using cached result for ", videoId, ": ", cachedUrl)
			callback(self, true, cachedUrl)
			return
		end

		-- same id already in flight, piggyback
		if g_pendingCallbacks[videoId] then
			DebugLog(realm, " Conversion already in-flight for ", videoId, ", queuing callback")
			table.insert(g_pendingCallbacks[videoId], { selfRef = self, callback = callback })
			return
		end

		g_pendingCallbacks[videoId] = { { selfRef = self, callback = callback } }
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

			if videoId then
				ResolvePending(videoId, false, nil, errCode)
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
			if videoId then
				ResolvePending(videoId, false, nil, ERROR_CONVERTER_FAILED)
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
			end

			if videoId then
				ResolvePending(videoId, false, nil, errCode)
			else
				callback(self, false, nil, errCode)
			end
			return
		end

		local streamPath = responseData.stream_url
		if not streamPath or streamPath == "" then
			DebugLog(realm, " ERROR: No stream_url in response")
			if videoId then
				ResolvePending(videoId, false, nil, ERROR_CONVERTER_FAILED)
			else
				callback(self, false, nil, ERROR_CONVERTER_FAILED)
			end
			return
		end

		local streamUrl = string.TrimRight(converterUrl, "/") .. streamPath

		local vid = responseData.video_id or videoId
		if vid then
			SetCachedResult(vid, streamUrl)
		end

		DebugLog(realm, " SUCCESS: ", responseData.title or "", " -> ", streamUrl)
		DebugLog(realm, " Duration: ", tostring(responseData.duration or 0), "s, Cached: ", tostring(responseData.cached))

		if videoId then
			ResolvePending(videoId, true, streamUrl)
		else
			callback(self, true, streamUrl)
		end
	end, bodyJson, "POST", headers, "application/json")
end

return true

