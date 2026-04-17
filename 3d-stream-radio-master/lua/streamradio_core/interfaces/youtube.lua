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

-- stored on StreamRadioLib global so it survives lua reload
StreamRadioLib._ytResultCache = StreamRadioLib._ytResultCache or {}
StreamRadioLib._ytPendingCallbacks = StreamRadioLib._ytPendingCallbacks or {}

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
The YouTube converter server is not configured or cannot be reached.

Server admin needs to:
	1. Set up the yt-converter-server
	2. Set sv_streamradio_yt_converter_url to the server URL
	3. Set sv_streamradio_yt_converter_secret to the shared API secret
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

local g_cvConverterUrl = CreateConVar(
	"sv_streamradio_yt_converter_url",
	"",
	bit.bor(FCVAR_ARCHIVE, FCVAR_GAMEDLL, FCVAR_REPLICATED),
	"URL of the YouTube converter server, e.g. http://your-server:9999"
)

local g_cvConverterSecret = CreateConVar(
	"sv_streamradio_yt_converter_secret",
	"",
	bit.bor(FCVAR_ARCHIVE, FCVAR_GAMEDLL, FCVAR_REPLICATED),
	"Shared API key for YouTube converter authentication"
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
	url = string.lower(url)

	local id = string.match(url, "[?&]v=([%w_-]+)")
	if id and #id == 11 then return id end

	id = string.match(url, "youtu%.be/([%w_-]+)")
	if id and #id == 11 then return id end

	id = string.match(url, "youtube%.com/embed/([%w_-]+)")
	if id and #id == 11 then return id end

	id = string.match(url, "youtube%.com/v/([%w_-]+)")
	if id and #id == 11 then return id end

	return nil
end

function RADIOIFACE:Convert(url, callback)
	local realm = SERVER and "SERVER" or "CLIENT"
	DebugLog(realm, " Convert called for: ", url)

	local converterUrl = string.Trim(g_cvConverterUrl:GetString())
	local converterSecret = string.Trim(g_cvConverterSecret:GetString())

	if converterUrl == "" then
		DebugLog(realm, " ERROR: sv_streamradio_yt_converter_url is not set!")
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

		-- already converting, just piggyback
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
	}

	if CLIENT then
		local ply = LocalPlayer()
		if IsValid(ply) then
			bodyTable.nick = ply:Nick() or ""
			bodyTable.steamid = ply:SteamID() or ""
		end
	end

	if SERVER then
		bodyTable.server_ip = game.GetIPAddress() or ""
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
				DebugLog(realm, " ERROR: Authentication failed (401). Check sv_streamradio_yt_converter_secret")
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

