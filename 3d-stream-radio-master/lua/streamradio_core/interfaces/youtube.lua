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

-- shared helper lives in lua/autorun/streamradio_converter.lua
local SRC = StreamRadioConverter
if not istable(SRC) then
	include("autorun/streamradio_converter.lua")
	SRC = StreamRadioConverter
end

local g_cvDebug = GetConVar("sv_streamradio_yt_debug") or CreateConVar(
	"sv_streamradio_yt_debug",
	"0",
	bit.bor(FCVAR_ARCHIVE, FCVAR_GAMEDLL, FCVAR_REPLICATED),
	"Enable YouTube converter debug logging (0 = off, 1 = on)"
)

local function DebugLog(...)
	if not g_cvDebug:GetBool() then return end
	MsgN("[StreamRadio YT] ", SERVER and "SERVER " or "CLIENT ", ...)
end

local ERR = SRC.RegisterErrors(110000, "YT", "YouTube", "video")
local g_resolver = SRC.NewResolver("youtube")

local YoutubeURLs = {
	"youtube://",
	"yt://",
	"://youtube.",
	".youtube.",
	"://youtu.be",
	"://youtube-nocookie.",
	".youtube-nocookie.",
}

function RADIOIFACE:CheckURL(url)
	local low = string.lower(url)

	for _, v in ipairs(YoutubeURLs) do
		if string.find(low, v, 1, true) then
			return true
		end
	end

	return false
end

-- patterns run on the lowercased url, the id is taken from the original
-- (video ids are case-sensitive)
local g_idPatterns = {
	{ "[?&]v=", "[?&]v=([%w_-]+)" },
	{ "youtu%.be/", "youtu%.be/([%w_-]+)" },
	{ "/embed/", "/embed/([%w_-]+)" },
	{ "/shorts/", "/shorts/([%w_-]+)" },
	{ "/live/", "/live/([%w_-]+)" },
	{ "youtube%.com/v/", "youtube%.com/v/([%w_-]+)" },
}

local function ExtractVideoId(url)
	local low = string.lower(url)

	for _, p in ipairs(g_idPatterns) do
		local s = string.find(low, p[1])
		if s then
			local id = string.match(url, p[2], s)
			if id and #id == 11 then
				return id
			end
		end
	end

	return nil
end

local function IsPlaylistURL(url)
	url = string.lower(url)
	if string.find(url, "youtube%.com/playlist") then return true end
	if string.find(url, "[?&]list=") then return true end
	return false
end

-- first track is converted right away, the rest is handed to the entity
-- playlist (stream.lua reads callback arg #5)
function RADIOIFACE:ConvertPlaylist(url, callback, context)
	DebugLog("playlist: ", url)

	g_resolver:FetchPlaylist(url, context, ERR, DebugLog, function(ok, tracks, errId)
		if not ok then
			callback(self, false, nil, errId)
			return
		end

		local items = {}
		for _, t in ipairs(tracks) do
			if isstring(t.url) and t.url ~= "" then
				items[#items + 1] = {
					url = t.url,
					name = (isstring(t.title) and t.title ~= "") and t.title or t.url,
				}
			end
		end

		if #items == 0 then
			callback(self, false, nil, ERR.INVALID_URL)
			return
		end

		local first = items[1]
		g_resolver:ConvertTrack(ExtractVideoId(first.url), first.url, context, ERR, DebugLog, function(ok2, streamUrl, errId2)
			if not ok2 then
				callback(self, false, nil, errId2)
				return
			end

			callback(self, true, streamUrl, nil, {
				tracks = items,
				currentIndex = 1,
			})
		end)
	end)
end

function RADIOIFACE:Convert(url, callback, context)
	DebugLog("convert: ", url)

	if SRC.GetBaseUrl() == "" then
		DebugLog("ERROR: sv_streamradio_converter_url is not set")
		callback(self, false, nil, ERR.UNAVAILABLE)
		return
	end

	if IsPlaylistURL(url) then
		self:ConvertPlaylist(url, callback, context)
		return
	end

	g_resolver:ConvertTrack(ExtractVideoId(url), url, context, ERR, DebugLog, function(ok, streamUrl, errId)
		callback(self, ok, streamUrl, errId)
	end)
end

return true
