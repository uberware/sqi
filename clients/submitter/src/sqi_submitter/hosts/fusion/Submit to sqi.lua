-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Submit to sqi: Fusion comp script (Lua 5.1).
--
-- Install into Fusion's user Scripts/Comp folder (`sqi-submit install-host fusion` copies it
-- there), then choose Script > Submit to sqi in Fusion. It writes a version 1 handoff context
-- file (see docs/dcc-submitters.md) describing the open comp, then launches
-- `sqi-submit --context <file>`, which opens the shared submit dialog. It submits the whole
-- comp: the render command line cannot pick a Saver.
--
-- Fusion bundles Lua 5.1 (LuaJIT in newer releases) but no Python, so this one file is the
-- whole submitter. Keep it to Lua 5.1: no goto, no `//`, no utf8 library. The functions down
-- to "Fusion glue" are pure or take their dependencies as arguments, and the tests run them
-- under a stock Lua without Fusion. The glue needs a real Fusion: the manual checklist covers
-- it. Fusion's own Lua build is what runs this file, so keep to the standard os/io/string
-- libraries only.

SQI_MSG = {
    -- The sentence every sqi host uses (\226\128\148 is an em dash in UTF-8).
    unsaved = "Save your scene before submitting \226\128\148 the farm renders the file on disk.",
    saveFailed = "Fusion could not save the comp, so nothing was submitted.",
    notFound = "sqi-submit was not found. Install it on this machine with: "
        .. "pip install \"sqi-submitter[qt]\" \226\128\148 then set its full path as "
        .. "sqi_submit_command in ~/.sqi/submitter.json.",
    noTemp = "The submit context file could not be written: the TEMP folder is not set.",
    writeFailed = "The submit context file could not be written to ",
    launchFailed = "sqi-submit could not be started: ",
    noExecute = "This Fusion build does not allow scripts to run programs (os.execute), "
        .. "so sqi-submit cannot be started from here.",
    unexpected = "Submit to sqi failed: ",
}

local SQI_TITLE = "Submit to sqi"
local SQI_SETTINGS_KEY = "sqi_submit_command"

-- -- pure functions (run under a stock Lua by the tests) -----------------------------------

-- A JSON string literal for s. Escapes backslash, quote, \n, \r and \t, writes the other
-- bytes below 0x20 as \u00XX, and passes bytes 0x80 and above through unchanged, so UTF-8
-- stays UTF-8. A byte loop rather than a pattern: a pattern cannot name the NUL byte the same
-- way in Lua 5.1 (%z) and later versions.
function sqi_json_string(s)
    local parts = { "\"" }
    for i = 1, #s do
        local b = string.byte(s, i)
        if b == 34 then
            parts[#parts + 1] = "\\\""
        elseif b == 92 then
            parts[#parts + 1] = "\\\\"
        elseif b == 10 then
            parts[#parts + 1] = "\\n"
        elseif b == 13 then
            parts[#parts + 1] = "\\r"
        elseif b == 9 then
            parts[#parts + 1] = "\\t"
        elseif b < 32 then
            parts[#parts + 1] = string.format("\\u%04x", b)
        else
            parts[#parts + 1] = string.char(b)
        end
    end
    parts[#parts + 1] = "\""
    return table.concat(parts)
end

-- "S" or "S-E": the two ends floored and put in order.
function sqi_frame_range(start_, end_)
    local lo = math.floor(start_)
    local hi = math.floor(end_)
    if lo > hi then
        lo, hi = hi, lo
    end
    if lo == hi then
        return string.format("%d", lo)
    end
    return string.format("%d-%d", lo, hi)
end

-- A JSON string literal, or null for nil.
local function json_or_null(value)
    if value == nil then
        return "null"
    end
    return sqi_json_string(tostring(value))
end

-- The version 1 handoff document for the open comp. frame_range and host_version may be nil.
function sqi_context_json(scene_path, frame_range, host_version)
    return "{\"version\":1,"
        .. "\"host\":{\"name\":\"fusion\",\"display_name\":\"Fusion\",\"version\":"
        .. json_or_null(host_version) .. "},"
        .. "\"scene\":{\"path\":" .. sqi_json_string(scene_path)
        .. ",\"frame_range\":" .. json_or_null(frame_range)
        .. ",\"output_path\":null,\"renderer\":null},"
        .. "\"targets\":[],\"server_url\":null}"
end

-- A Unicode code point as UTF-8 bytes (Lua 5.1 has no utf8 library).
local function utf8_encode(cp)
    if cp < 128 then
        return string.char(cp)
    elseif cp < 2048 then
        return string.char(192 + math.floor(cp / 64), 128 + cp % 64)
    elseif cp < 65536 then
        return string.char(
            224 + math.floor(cp / 4096),
            128 + math.floor(cp / 64) % 64,
            128 + cp % 64
        )
    end
    return string.char(
        240 + math.floor(cp / 262144),
        128 + math.floor(cp / 4096) % 64,
        128 + math.floor(cp / 64) % 64,
        128 + cp % 64
    )
end

local JSON_SIMPLE_ESCAPES = {
    ["\""] = "\"", ["\\"] = "\\", ["/"] = "/",
    b = "\b", f = "\f", n = "\n", r = "\r", t = "\t",
}

-- Reads the JSON string whose opening quote is at text[pos]. Returns the decoded string and
-- the position after the closing quote, or nil if the string is malformed or unterminated.
local function read_json_string(text, pos)
    local parts = {}
    local i = pos + 1
    local n = #text
    while i <= n do
        local c = text:sub(i, i)
        if c == "\"" then
            return table.concat(parts), i + 1
        elseif c == "\\" then
            local e = text:sub(i + 1, i + 1)
            if e == "u" then
                local hex = text:match("^%x%x%x%x", i + 2)
                if hex == nil then
                    return nil
                end
                local cp = tonumber(hex, 16)
                i = i + 6
                if cp >= 55296 and cp <= 56319 then
                    -- A high surrogate: combine it with the low one that must follow.
                    local low = text:match("^\\u(%x%x%x%x)", i)
                    local lowcp = low and tonumber(low, 16) or 0
                    if lowcp >= 56320 and lowcp <= 57343 then
                        cp = 65536 + (cp - 55296) * 1024 + (lowcp - 56320)
                        i = i + 6
                    else
                        cp = 65533
                    end
                elseif cp >= 56320 and cp <= 57343 then
                    cp = 65533
                end
                parts[#parts + 1] = utf8_encode(cp)
            else
                local simple = JSON_SIMPLE_ESCAPES[e]
                if simple == nil then
                    return nil
                end
                parts[#parts + 1] = simple
                i = i + 2
            end
        else
            parts[#parts + 1] = c
            i = i + 1
        end
    end
    return nil
end

-- The sqi_submit_command string from the text of ~/.sqi/submitter.json, or nil when the key
-- is absent, is not at the top level, or is not a string. Scans the JSON just far enough to
-- tell a key from a string value; it is not a validator.
function sqi_parse_settings(text)
    if type(text) ~= "string" then
        return nil
    end
    local pos = 1
    if text:sub(1, 3) == "\239\187\191" then
        pos = 4 -- PowerShell 5 writes UTF-8 with a byte-order mark.
    end
    local depth = 0
    local n = #text
    while pos <= n do
        local c = text:sub(pos, pos)
        if c == "\"" then
            local str, after = read_json_string(text, pos)
            if str == nil then
                return nil
            end
            pos = after
            if depth == 1 and str == SQI_SETTINGS_KEY then
                local value_at = text:match("^%s*:%s*()", pos)
                if value_at ~= nil then
                    if text:sub(value_at, value_at) ~= "\"" then
                        return nil
                    end
                    return (read_json_string(text, value_at))
                end
            end
        else
            if c == "{" or c == "[" then
                depth = depth + 1
            elseif c == "}" or c == "]" then
                depth = depth - 1
            end
            pos = pos + 1
        end
    end
    return nil
end

-- A trailing sqi-submit.exe (any case) swapped for the sibling GUI-subsystem sqi-submitw.exe
-- when exists(path) says it is there. sqi-submit.exe is a console program, so launching it
-- gives the dialog a console window for as long as it is open. Anything else is used as it is.
function sqi_windowed_command(command, exists)
    local dir, name = command:match("^(.*[\\/])([^\\/]*)$")
    if dir == nil then
        dir, name = "", command
    end
    if name:lower() ~= "sqi-submit.exe" then
        return command
    end
    local windowed = dir .. "sqi-submitw.exe"
    if exists(windowed) then
        return windowed
    end
    return command
end

-- Why a path cannot be put in the launch command, or nil when it can. A double quote cannot
-- be quoted on either platform; cmd.exe also expands %NAME% inside quotes.
local function unsafe_reason(is_windows, path)
    if path:find("\"", 1, true) then
        return "sqi cannot launch a program or temp file whose path contains a quote (\") "
            .. "character. Move or reinstall it somewhere without one. Path: " .. path
    end
    if is_windows and path:find("%", 1, true) then
        return "sqi cannot launch a program or temp file whose path contains a percent (%) "
            .. "character on Windows. Move or reinstall it somewhere without one. Path: " .. path
    end
    return nil
end

-- The command line that starts sqi-submit without blocking Fusion, or nil plus a message
-- when a path could not be quoted safely (nothing is ever escaped around a quote).
function sqi_launch_command(is_windows, exe, context_path)
    local reason = unsafe_reason(is_windows, exe) or unsafe_reason(is_windows, context_path)
    if reason ~= nil then
        return nil, reason
    end
    if is_windows then
        return "start \"\" \"" .. exe .. "\" --context \"" .. context_path .. "\""
    end
    local function single(path)
        return "'" .. path:gsub("'", function() return "'\\''" end) .. "'"
    end
    return single(exe) .. " --context " .. single(context_path) .. " >/dev/null 2>&1 &"
end

-- -- Fusion glue (needs a real Fusion; the manual checklist covers it) ----------------------

function sqi_is_windows()
    return package.config:sub(1, 1) == "\\"
end

local function file_exists(path)
    local handle = io.open(path, "rb")
    if handle == nil then
        return false
    end
    handle:close()
    return true
end

local function read_file(path)
    local handle = io.open(path, "rb")
    if handle == nil then
        return nil
    end
    local text = handle:read("*a")
    handle:close()
    return text
end

-- Tells the artist something: always on Fusion's Console, then in an AskUser dialog. The
-- Console line comes first and unconditionally because AskUser's control table is unverified:
-- a dialog that is accepted but shows nothing must not leave the artist with no message.
function sqi_message(text)
    print(SQI_TITLE .. ": " .. text)
    pcall(function()
        return comp:AskUser(SQI_TITLE, {
            { "Message", "Text", Name = "", Default = text, Lines = 4, Wrap = true, ReadOnly = true },
        })
    end)
end

-- ~/.sqi/submitter.json, or $SQI_SUBMITTER_SETTINGS when set (as the Python hosts read it).
local function settings_path(is_windows)
    local override = os.getenv("SQI_SUBMITTER_SETTINGS")
    if override ~= nil and override ~= "" then
        return override
    end
    local home = os.getenv("HOME")
    if home == nil or home == "" then
        home = os.getenv("USERPROFILE")
    end
    if home == nil or home == "" then
        return nil
    end
    return home .. (is_windows and "\\" or "/") .. ".sqi" .. (is_windows and "\\" or "/")
        .. "submitter.json"
end

-- The first line `where $PATH:sqi-submit` (Windows; plain `where` searches the current directory
-- before PATH, so a stray sqi-submit.exe there would win) or `command -v sqi-submit` (POSIX) prints.
local function find_on_path(is_windows)
    local command = is_windows and "where $PATH:sqi-submit 2>nul" or "command -v sqi-submit 2>/dev/null"
    local ok, found = pcall(function()
        local pipe = io.popen(command)
        if pipe == nil then
            return nil
        end
        local text = pipe:read("*a")
        pipe:close()
        return text
    end)
    if not ok or type(found) ~= "string" then
        return nil
    end
    local line = found:match("^[^\r\n]+")
    if line == nil then
        return nil
    end
    line = line:gsub("^%s+", ""):gsub("%s+$", "")
    if line == "" then
        return nil
    end
    return line
end

-- Finds sqi-submit as the Python block does: the settings file, then PATH.
local function resolve_command(is_windows)
    local path = settings_path(is_windows)
    local configured = path and sqi_parse_settings(read_file(path))
    if configured ~= nil and configured ~= "" and file_exists(configured) then
        return configured
    end
    return find_on_path(is_windows)
end

-- Opens a new private file for the context and returns its path and handle, or nil. On POSIX
-- os.tmpname creates the file itself (mkstemp: exclusive, mode 0600). On Windows the name is
-- built under %TEMP%, a per-user folder already.
local function open_context_file(is_windows)
    if not is_windows then
        local name = os.tmpname()
        local handle = name and io.open(name, "wb")
        if handle == nil then
            return nil, SQI_MSG.writeFailed .. tostring(name) .. "."
        end
        return name, handle
    end
    local temp = os.getenv("TEMP") or os.getenv("TMP")
    if temp == nil or temp == "" then
        return nil, SQI_MSG.noTemp
    end
    local sep = "\\"
    if temp:find("/", 1, true) and not temp:find("\\", 1, true) then
        sep = "/"
    end
    math.randomseed(os.time())
    local name
    for _ = 1, 5 do
        name = temp .. sep .. "sqi-handoff-" .. os.time() .. "-" .. math.random(1000000000)
            .. ".json"
        if not file_exists(name) then
            break
        end
    end
    local handle = io.open(name, "wb")
    if handle == nil then
        return nil, SQI_MSG.writeFailed .. temp .. "."
    end
    return name, handle
end

local function fusion_version()
    local ok, version = pcall(function()
        return fusion.Version
    end)
    if ok and version ~= nil then
        return tostring(version)
    end
    return nil
end

-- The comp's render range as "S-E", or nil when Fusion does not report one.
local function comp_frame_range(attrs)
    local first, last = attrs.COMPN_RenderStart, attrs.COMPN_RenderEnd
    if type(first) == "number" and type(last) == "number" then
        return sqi_frame_range(first, last)
    end
    return nil
end

-- Saves a modified comp; true when the file on disk is current. COMPB_Modified is read from
-- the comp's attributes; when Fusion does not report it the comp is saved anyway, which only
-- rewrites an unchanged file.
local function ensure_saved(attrs, filename)
    if attrs.COMPB_Modified == false then
        return true
    end
    local ok, result = pcall(function()
        return comp:Save(filename)
    end)
    return ok and result ~= false
end

-- The entry point. Returns true when sqi-submit was started, false after telling the artist
-- why not.
function sqi_submit()
    local is_windows = sqi_is_windows()
    local attrs = comp:GetAttrs()
    local filename = attrs.COMPS_FileName
    if type(filename) ~= "string" or filename == "" then
        sqi_message(SQI_MSG.unsaved)
        return false
    end
    if not ensure_saved(attrs, filename) then
        sqi_message(SQI_MSG.saveFailed)
        return false
    end
    local exe = resolve_command(is_windows)
    if exe == nil then
        sqi_message(SQI_MSG.notFound)
        return false
    end
    if is_windows then
        exe = sqi_windowed_command(exe, file_exists)
    end
    if type(os.execute) ~= "function" then
        sqi_message(SQI_MSG.noExecute)
        return false
    end
    local path, handle = open_context_file(is_windows)
    if path == nil then
        sqi_message(handle)
        return false
    end
    local document = sqi_context_json(filename, comp_frame_range(attrs), fusion_version())
    local written = handle:write(document)
    handle:close()
    if not written then
        os.remove(path)
        sqi_message(SQI_MSG.writeFailed .. path .. ".")
        return false
    end
    local command, reason = sqi_launch_command(is_windows, exe, path)
    if command == nil then
        os.remove(path)
        sqi_message(reason)
        return false
    end
    local status = os.execute(command)
    if status ~= 0 and status ~= true then
        os.remove(path)
        sqi_message(SQI_MSG.launchFailed .. command)
        return false
    end
    return true
end

-- Fusion sets `comp` for a Comp script; the tests load this file without it.
if comp ~= nil then
    local ok, err = pcall(sqi_submit)
    if not ok then
        sqi_message(SQI_MSG.unexpected .. tostring(err))
    end
end
