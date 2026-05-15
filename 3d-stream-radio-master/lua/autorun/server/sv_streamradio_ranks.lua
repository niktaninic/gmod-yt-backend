-- sv_streamradio_ranks.lua
-- Auto-loaded by GMod on server start.
-- Pushes the ULX/ULib usergroup list to the Stream Radio converter backend
-- so the dev panel can show real rank names for the rank-limit editor.

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

local function CollectRanks()
	local ranks = {}
	local seen = {}

	if ULib and ULib.ucl and istable(ULib.ucl.groups) then
		for name, _ in pairs(ULib.ucl.groups) do
			if isstring(name) and not seen[name] then
				seen[name] = true
				table.insert(ranks, name)
			end
		end
	end

	-- fill in from live players (catches groups not yet in the table)
	for _, ply in ipairs(player.GetAll()) do
		local g = ply:GetUserGroup()
		if isstring(g) and g ~= "" and not seen[g] then
			seen[g] = true
			table.insert(ranks, g)
		end
	end

	-- "user" is the default GMod group; always include it
	if not seen["user"] then
		table.insert(ranks, "user")
	end

	table.sort(ranks)
	return ranks
end

local _lastSentKey = ""
local DEBOUNCE_TIMER = "streamradio_rank_sync_debounce"

local function PushRanks()
	local url = GetConverterUrl()
	if url == "" then return end

	local secret = GetConverterSecret()
	local ranks = CollectRanks()
	local key = table.concat(ranks, ",")

	if key == _lastSentKey then return end
	_lastSentKey = key

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
				MsgN("[StreamRadio Ranks] push failed: HTTP ", code, " — ", respBody or "")
			end
		end,
		failed = function(reason)
			MsgN("[StreamRadio Ranks] push failed: ", reason or "unknown")
		end,
	})
end

local function SchedulePush(delay)
	timer.Create(DEBOUNCE_TIMER, delay or 2, 1, PushRanks)
end

-- push after everything has loaded
timer.Simple(5, PushRanks)

-- ULib hook: fires when ULib finishes loading its group database
hook.Add("ULibLoaded", "StreamRadio_SyncRanks", function()
	SchedulePush(3)
end)

-- ULX hook: fires when a player's group is changed
hook.Add("ULibUserGroupChanged", "StreamRadio_SyncRanks", function()
	SchedulePush(2)
end)

-- also sync when a player joins (may introduce a new group)
hook.Add("PlayerInitialSpawn", "StreamRadio_SyncRanks_Join", function()
	SchedulePush(3)
end)

-- periodic safety net: re-sync every 5 minutes
timer.Create("streamradio_rank_periodic", 300, 0, PushRanks)
