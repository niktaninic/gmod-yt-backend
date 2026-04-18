local RADIOIFACE = RADIOIFACE
if not istable(RADIOIFACE) then
	StreamRadioLib.ReloadAddon()
	return
end

RADIOIFACE.name = "File"
RADIOIFACE.priority = 100000
RADIOIFACE.download = false
RADIOIFACE.online = false
RADIOIFACE.cache = false

RADIOIFACE.downloadTimeout = 0
RADIOIFACE.downloadFirst = false
RADIOIFACE.allowCaching = false

local LIBUrl = StreamRadioLib.Url
local LIBString = StreamRadioLib.String
local LIBError = StreamRadioLib.Error

local function IsHandledServiceUri(url)
	url = string.lower(tostring(url or ""))

	if string.find(url, "spotify:track:", 1, true) then return true end
	if string.find(url, "spotify:playlist:", 1, true) then return true end

	return false
end

function RADIOIFACE:CheckURL(url)
	if IsHandledServiceUri(url) then
		return false
	end

	if not LIBUrl.IsOfflineURL(url) then
		return false
	end

	return true
end

function RADIOIFACE:ParseURL(url)
	local _, filepath = LIBUrl.SplittProtocolAndPath(url)

	local urlResult = "sound/" .. filepath
	urlResult = LIBString.NormalizeSlashes(urlResult)

	return urlResult
end

function RADIOIFACE:Convert(url, callback)
	if LIBUrl.IsDriveLetterOfflineURL(url) then
		callback(self, true, "", LIBError.STREAM_ERROR_BAD_DRIVE_LETTER_PATH)
		return
	end

	local path = self:ParseURL(url)

	callback(self, true, path)
end

return true

