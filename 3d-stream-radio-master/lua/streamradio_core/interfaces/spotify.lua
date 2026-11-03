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

-- shared helper lives in lua/autorun/streamradio_converter.lua
local SRC = StreamRadioConverter
if not istable(SRC) then
	include("autorun/streamradio_converter.lua")
	SRC = StreamRadioConverter
end

local g_cvDebug = GetConVar("sv_streamradio_spotify_debug") or CreateConVar(
	"sv_streamradio_spotify_debug",
	"0",
	bit.bor(FCVAR_ARCHIVE, FCVAR_GAMEDLL, FCVAR_REPLICATED),
	"Enable Spotify converter debug logging (0 = off, 1 = on)"
)

local function DebugLog(...)
	if not g_cvDebug:GetBool() then return end
	MsgN("[StreamRadio Spotify] ", SERVER and "SERVER " or "CLIENT ", ...)
end

local ERR = SRC.RegisterErrors(120000, "SPOTIFY", "Spotify", "track")
local g_resolver = SRC.NewResolver("spotify")

function RADIOIFACE:CheckURL(url)
	url = string.lower(url)

	if string.find(url, "spotify.com/track/", 1, true) then return true end
	if string.find(url, "spotify:track:", 1, true) then return true end

	if string.find(url, "spotify.com/intl-", 1, true) and string.find(url, "/track/", 1, true) then
		return true
	end

	if string.find(url, "spotify.com/playlist/", 1, true) then return true end
	if string.find(url, "spotify.com/intl-", 1, true) and string.find(url, "/playlist/", 1, true) then
		return true
	end
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
	if string.find(url, "spotify:playlist:", 1, true) then return true end
	if string.find(url, "spotify.com/", 1, true) and string.find(url, "/playlist/", 1, true) then return true end
	return false
end

-- raw spotify: URIs are flaky in the url sanitizer, use normal track urls
local function BuildTrackPlaybackUrl(trackData)
	local trackId = string.Trim(tostring(trackData.track_id or ""))
	if trackId ~= "" then
		return "https://open.spotify.com/track/" .. trackId
	end

	return string.Trim(tostring(trackData.url or ""))
end

function RADIOIFACE:ConvertPlaylist(url, callback, context)
	DebugLog("playlist: ", url)

	g_resolver:FetchPlaylist(url, context, ERR, DebugLog, function(ok, tracks, errId, startIndex)
		if not ok then
			callback(self, false, nil, errId)
			return
		end

		local items = {}
		for _, t in ipairs(tracks) do
			local trackUrl = BuildTrackPlaybackUrl(t)
			if trackUrl ~= "" then
				items[#items + 1] = {
					url = trackUrl,
					name = (isstring(t.title) and t.title ~= "") and t.title or trackUrl,
				}
			end
		end

		if #items == 0 then
			callback(self, false, nil, ERR.INVALID_URL)
			return
		end

		-- if the first track fails for a track-specific reason (removed,
		-- region locked, ...) try the next ones instead of failing the playlist
		local function TryAt(index, attempt)
			local item = items[index]
			g_resolver:ConvertTrack(ExtractTrackId(item.url), item.url, context, ERR, DebugLog, function(ok2, streamUrl, errId2)
				if not ok2 then
					if attempt < 3 and #items > attempt and SRC.IsItemError(errId2) then
						DebugLog("playlist item #", index, " failed, trying the next one")
						TryAt(index % #items + 1, attempt + 1)
						return
					end

					callback(self, false, nil, errId2)
					return
				end

				callback(self, true, streamUrl, nil, {
					tracks = items,
					currentIndex = index,
				})
			end)
		end

		TryAt(math.Clamp(startIndex or 1, 1, #items), 1)
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

	local trackId = ExtractTrackId(url)
	if not trackId then
		callback(self, false, nil, ERR.INVALID_URL)
		return
	end

	g_resolver:ConvertTrack(trackId, url, context, ERR, DebugLog, function(ok, streamUrl, errId)
		callback(self, ok, streamUrl, errId)
	end)
end

return true
