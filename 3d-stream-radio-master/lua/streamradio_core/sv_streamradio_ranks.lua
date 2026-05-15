-- sv_streamradio_ranks.lua
-- Server-side only. Pushes ULX/ULib usergroup list to the converter backend
-- so the dev console can show available ranks when adding limits.
-- Runs automatically; does nothing if converter URL is not configured.

if not SERVER then return end

local function GetConverterUrl()
	local cv = GetConVar("sv_streamradio_converter_url")
	if not cv then return "" end
	return string.Trim(cv:GetString())
end

local function GetConverterSecret()
	local cv = GetConVar("sv_streamradio_converter_secret")
	if not cv then return "" end
	return string.Trim(cv:GetString())
end

-- collect all ULib/ULX usergroups currently registered
local function CollectRanks()
	local ranks = {}
	local seen = {}

	-- ULib stores groups in ULib.ucl.groups
	if ULib and ULib.ucl and istable(ULib.ucl.groups) then
		for name, _ in pairs(ULib.ucl.groups) do
			if isstring(name) and not seen[name] then
				seen[name] = true
				table.insert(ranks, name)
			end
		end
	end

	-- also grab from live players (catches edge cases where groups file lags)
	for _, ply in ipairs(player.GetAll()) do
		local g = ply:GetUserGroup()
		if isstring(g) and g ~= "" and not seen[g] then
			seen[g] = true
			table.insert(ranks, g)
		end
	end

	-- always include "user" (default GMod group) even if no players online
	if not seen["user"] then
		table.insert(ranks, "user")
	end

	table.sort(ranks)
	return ranks
end

local _lastSent = {}
local _sendTimer = "streamradio_rank_sync"

local function PushRanks()
	local url = GetConverterUrl()
	if url == "" then return end

	local secret = GetConverterSecret()
	local ranks = CollectRanks()

	-- skip if nothing changed
	if table.concat(ranks, ",") == table.concat(_lastSent, ",") then return end
	_lastSent = table.Copy(ranks)

	local body = util.TableToJSON({ ranks = ranks })
	local headers = { ["Content-Type"] = "application/json" }
	if secret ~= "" then headers["X-SR-Key"] = secret end

	local apiUrl = string.TrimRight(url, "/") .. "/api/ranks"

	HTTP({
		url = apiUrl,
		method = "POST",
		body = body,
		type = "application/json",
		headers = headers,
		success = function(code, respBody)
			if code ~= 200 then
				MsgN("[StreamRadio Ranks] Failed to push ranks: HTTP ", code, " — ", respBody or "")
			end
		end,
		failed = function(reason)
			MsgN("[StreamRadio Ranks] Failed to push ranks: ", reason or "unknown error")
		end,
	})
end

-- push on server start, wait a bit for ULib to finish loading groups
timer.Simple(5, function()
	PushRanks()
end)

-- push whenever a player's group changes (covers ULX promotions/demotions)
hook.Add("ULibUserGroupChanged", "StreamRadio_RankSync", function()
	-- debounce: multiple changes at once (e.g. ULX reload) → single push
	timer.Create(_sendTimer, 2, 1, PushRanks)
end)

-- also push on player join (new group may appear for the first time)
hook.Add("PlayerInitialSpawn", "StreamRadio_RankSync_Join", function(ply)
	timer.Create(_sendTimer, 3, 1, PushRanks)
end)

-- periodic re-sync every 5 minutes as a safety net
timer.Create("streamradio_rank_periodic", 300, 0, PushRanks)
