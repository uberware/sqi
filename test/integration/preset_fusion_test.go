// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// preset_fusion_test.go checks what the fusion-saver-render golden files cannot
// show on their own: that params.json is JSON a script can decode, that its cost
// stays inside the worker's EXPR limits at the largest chunk the preset allows,
// that the -execute argument names the script and carries no job value, and
// what the embedded render.lua asks Fusion to do.
//
// Untagged, like the rest of the preset harness, so `make test-cover` runs it.
// Every test is named TestPresetFusion_* so the preset harness's -run regex
// (make test-preset-harness, and CI's preset-harness-windows job) selects it:
// params.json's escaping differs by host, and only a Windows run can show a
// backslash it failed to escape.
//
// TestPresetFusion_ScriptDrivesFusion runs render.lua under a stock Lua 5.1
// (lua5.1, luajit or lua on PATH). It skips without one; SQI_REQUIRE_LUA=1,
// which CI's integration-tests job sets, turns that skip into a failure.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
)

// fusionSaverPreset is the one Fusion host-script preset.
const fusionSaverPreset = "fusion-saver-render"

// fusionMaxFramesPerTask is the FramesPerTask maxValue the preset declares and
// its readme states.
const fusionMaxFramesPerTask = 1000

// fusionParams is params.json as the script reads it.
type fusionParams struct {
	Comp  string `json:"comp"`
	Saver string `json:"saver"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// fusionExecute matches the one -execute argument: Lua that runs render.lua,
// its path in a long bracket so no character of it is an escape.
var fusionExecute = regexp.MustCompile(`^dofile\(\[==\[(.+)\]==\]\)$`)

// TestPresetFusion_ParamsCostAtMaxChunk pins params.json's cost where it is
// highest. It reads Task.Param.Frame, so it is evaluated at phase 3 on the
// worker, against the worker's EXPR limits: the largest chunk the preset can
// produce (FramesPerTask = maxValue over a range of that many frames, 1000
// frames) must resolve under the worker defaults, with the longest Saver name,
// and carry the chunk's first and last frame. One past the maximum must be
// refused at submission by the maxValue bound itself, or the maximum bounds
// nothing.
func TestPresetFusion_ParamsCostAtMaxChunk(t *testing.T) {
	params := map[string]string{
		"SceneFile":     "/mnt/show/seq010/shot0010/comp/shot0010_comp_v007.comp",
		"Frames":        fmt.Sprintf("1-%d", fusionMaxFramesPerTask),
		"FramesPerTask": strconv.Itoa(fusionMaxFramesPerTask),
		"Saver":         strings.Repeat("S", 256),
	}
	snap, err := capturePreset(t, fusionSaverPreset, params)
	if err != nil {
		t.Fatalf("a %d-frame chunk must resolve under the worker's default EXPR limits: %v", fusionMaxFramesPerTask, err)
	}
	if len(snap.Steps) != 1 || len(snap.Steps[0].Tasks) != 1 {
		t.Fatalf("1-%d at %d frames per task must be one task, got %+v", fusionMaxFramesPerTask, fusionMaxFramesPerTask, snap.Steps)
	}
	p := decodeStrict[fusionParams](t, taskFile(t, snap.Steps[0].Tasks[0], "params"))
	if p.Start != 1 || p.End != fusionMaxFramesPerTask || p.Saver != params["Saver"] {
		t.Errorf("params.json = start %d, end %d, saver %q; want 1, %d and the 256-character name", p.Start, p.End, p.Saver, fusionMaxFramesPerTask)
	}

	params["FramesPerTask"] = strconv.Itoa(fusionMaxFramesPerTask + 1)
	_, err = capturePreset(t, fusionSaverPreset, params)
	wantRefusal := fmt.Sprintf("/parameterValues/FramesPerTask: value %d is above maximum %d", fusionMaxFramesPerTask+1, fusionMaxFramesPerTask)
	if err == nil || !strings.Contains(err.Error(), wantRefusal) {
		t.Errorf("FramesPerTask %d: err = %v, want the parameter binding refusal %q", fusionMaxFramesPerTask+1, err, wantRefusal)
	}
}

// TestPresetFusion_ParamsJSONRoundTrips runs the `quoting` fixture case (a
// double quote, a backslash, spaces and a non-ASCII letter in the Saver's
// name, both quotes and spaces in the comp's path) and decodes params.json with
// encoding/json: every value must come back exactly as given, and the frames
// must be the task's own chunk. The comp path comes back in the worker's own
// form, since under EXPR phase 3 renders PATH values host-native, so on
// Windows the JSON also carries escaped backslashes.
func TestPresetFusion_ParamsJSONRoundTrips(t *testing.T) {
	params := presetCase(t, fusionSaverPreset, "quoting").Params
	if !strings.Contains(params["Saver"], `"`) || !strings.Contains(params["Saver"], `\`) || !strings.Contains(params["Saver"], " ") || !strings.ContainsFunc(params["Saver"], func(r rune) bool { return r > 127 }) {
		t.Fatalf("quoting case's Saver %q needs a quote, a backslash, a space and a non-ASCII letter", params["Saver"])
	}
	snap, err := capturePreset(t, fusionSaverPreset, params)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	for _, task := range snap.Steps[0].Tasks {
		p := decodeStrict[fusionParams](t, taskFile(t, task, "params"))
		if want := filepath.FromSlash(params["SceneFile"]); p.Comp != want {
			t.Errorf("comp = %q, want %q", p.Comp, want)
		}
		if p.Saver != params["Saver"] {
			t.Errorf("saver = %q, want %q", p.Saver, params["Saver"])
		}
		if got, want := fmt.Sprintf("%d-%d", p.Start, p.End), task.Params["Frame"]; got != want {
			t.Errorf("start-end = %s, want the task's chunk %s", got, want)
		}
	}
}

// TestPresetFusion_CommandNamesTheScript pins what the goldens hide (they fold
// the session path to <WORKDIR>): every task of every fixture case runs
// FusionRenderNode -quiet -verbose -execute with Lua that only runs the
// embedded render.lua, by an absolute path, and no job value (the comp, the
// Saver) is part of the command line. Fusion never sees job text as code.
func TestPresetFusion_CommandNamesTheScript(t *testing.T) {
	cases, err := presettest.LoadCases(filepath.Join(presetCaseDir, fusionSaverPreset+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			snap, err := capturePreset(t, fusionSaverPreset, c.Params)
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			for _, task := range snap.Steps[0].Tasks {
				if task.Command != "FusionRenderNode" || len(task.Args) != 4 || !slices.Equal(task.Args[:3], []string{"-quiet", "-verbose", "-execute"}) {
					t.Fatalf("command = %q %q, want FusionRenderNode -quiet -verbose -execute <code>", task.Command, task.Args)
				}
				m := fusionExecute.FindStringSubmatch(task.Args[3])
				if m == nil {
					t.Fatalf("-execute code %q is not dofile([==[<script>]==])", task.Args[3])
				}
				if !filepath.IsAbs(m[1]) || filepath.Base(m[1]) != "render.lua" {
					t.Errorf("script %q must be an absolute path to render.lua", m[1])
				}
				for _, key := range []string{"SceneFile", "Saver"} {
					if value := c.Params[key]; value != "" && strings.Contains(strings.Join(task.Args, " "), value) {
						t.Errorf("job value %s=%q appears in the command line %q", key, value, task.Args)
					}
				}
			}
		})
	}
}

// TestPresetTier3_FoldsALuaLongBracketScriptPath pins the Tier 3 cross-check's
// handling of the Fusion preset's -execute argument: the session directory
// inside the long-bracketed script path differs between the computed and the
// observed run, so it must fold to one placeholder in both POSIX and Windows
// spellings, while an unrelated path is left alone.
func TestPresetTier3_FoldsALuaLongBracketScriptPath(t *testing.T) {
	files := map[string]bool{"render.lua": true}
	const want = "dofile([==[<FILE:render.lua>]==])"
	for _, in := range []string{
		"dofile([==[/tmp/presettest-sessions-1/sessions/abc/render.lua]==])",
		`dofile([==[C:\Users\robin\AppData\Local\Temp\sessions\abc\render.lua]==])`,
	} {
		if got := foldFilePaths([]string{in}, files)[0]; got != want {
			t.Errorf("fold(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"dofile([==[/mnt/show/render.luac]==])", "dofile([==[relative/render.lua]==])"} {
		if got := foldFilePaths([]string{in}, files)[0]; got != in {
			t.Errorf("fold(%q) = %q, want it unchanged", in, got)
		}
	}
}

// findLua returns a Lua interpreter on PATH, preferring 5.1, the version
// Fusion's Lua follows. Without one the test skips, unless SQI_REQUIRE_LUA=1
// makes that a failure.
func findLua(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"lua5.1", "luajit", "lua"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	if os.Getenv("SQI_REQUIRE_LUA") == "1" {
		t.Fatal("SQI_REQUIRE_LUA=1 but none of lua5.1, luajit or lua is on PATH")
	}
	t.Skip("no Lua interpreter (lua5.1, luajit or lua) on PATH")
	return ""
}

// luaLiteral is a Lua string literal for s with every byte a decimal escape,
// so the runner's source stays ASCII whatever the name holds.
func luaLiteral(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range []byte(s) {
		fmt.Fprintf(&b, "\\%03d", c)
	}
	b.WriteByte('"')
	return b.String()
}

// fusionTool is one tool of the fake comp.
type fusionTool struct{ name, id string }

// fusionQuotedSaver is the quoting case's Saver name, written with escapes so
// this file stays ASCII.
const fusionQuotedSaver = "Out \"A\" \\ \u00e9"

// fusionTools are the fake comp's tools: three Savers, one of them with the
// quoting case's name, and a Merge.
var fusionTools = []fusionTool{
	{"Saver1", "Saver"},
	{fusionQuotedSaver, "Saver"},
	{"Merge1", "Merge"},
	{"MainOut", "Saver"},
}

// fusionRunner stands in for the Fusion Render Node. It defines a recording
// fake of the global fusion object and of the comp LoadComp returns, then runs
// the task's own -execute code (rewritten to name the copy of render.lua it is
// given) as Fusion would, and records every call in a JSON file, rewritten
// after each one. Tool lookups are not recorded; LoadComp, SetAttrs, Render,
// Save and Quit are.
//
// argv: the file holding the -execute code, the record file, and the mode:
// "ok", "render-nil" (Render returns nil) or "load-nil" (LoadComp returns
// nil). The script's lines go to this process's stdout.
//
//nolint:dupword // Lua source: nested blocks close with consecutive "end"s.
const fusionRunner = `local code_path, record_path, mode = arg[1], arg[2], arg[3]
local TOOLS = { %s }
local calls = {}

local function quote(s)
    local parts = { '"' }
    for i = 1, #s do
        local b = s:byte(i)
        if b == 34 or b == 92 then
            parts[#parts + 1] = "\\" .. string.char(b)
        elseif b < 32 then
            parts[#parts + 1] = string.format("\\u%%04x", b)
        else
            parts[#parts + 1] = string.char(b)
        end
    end
    parts[#parts + 1] = '"'
    return table.concat(parts)
end

local function encode(v)
    local t = type(v)
    if t == "string" then
        return quote(v)
    elseif t == "number" or t == "boolean" then
        return tostring(v)
    elseif t == "nil" then
        return "null"
    elseif t == "table" then
        local entries = {}
        for k, item in pairs(v) do
            entries[#entries + 1] = { tostring(k), item }
        end
        table.sort(entries, function(a, b) return a[1] < b[1] end)
        local parts = {}
        for _, e in ipairs(entries) do
            parts[#parts + 1] = quote(e[1]) .. ":" .. encode(e[2])
        end
        return "{" .. table.concat(parts, ",") .. "}"
    end
    return quote("<" .. t .. ">")
end

local function save()
    local f = assert(io.open(record_path, "wb"))
    f:write("[" .. table.concat(calls, ",") .. "]")
    f:close()
end

local function note(...)
    local call = {}
    for i = 1, select("#", ...) do
        call[i] = encode((select(i, ...)))
    end
    calls[#calls + 1] = "[" .. table.concat(call, ",") .. "]"
    save()
end

local tools = {}
for i, spec in ipairs(TOOLS) do
    local tool = { spec = spec }
    function tool:GetAttrs()
        return { TOOLS_Name = self.spec[1], TOOLS_RegID = self.spec[2] }
    end
    function tool:SetAttrs(attrs)
        note("SetAttrs", self.spec[1], attrs)
        return true
    end
    tools[i] = tool
end

local comp = {}
function comp:FindTool(name)
    for _, tool in ipairs(tools) do
        if tool.spec[1] == name then
            return tool
        end
    end
    return nil
end
function comp:GetToolList(selected, id)
    local out = {}
    for _, tool in ipairs(tools) do
        if id == nil or tool.spec[2] == id then
            out[#out + 1] = tool
        end
    end
    return out
end
function comp:Render(opts)
    note("Render", opts)
    if mode == "render-nil" then
        return nil
    end
    return true
end
function comp:Save(...)
    note("Save", ...)
    return true
end

fusion = {}
function fusion:LoadComp(...)
    note("LoadComp", ...)
    if mode == "load-nil" then
        return nil
    end
    return comp
end
function fusion:Quit()
    note("Quit")
end

save()
local f = assert(io.open(code_path, "rb"))
local code = f:read("*a")
f:close()
local chunk = assert((loadstring or load)(code, "=execute"))
chunk()
`

// runFusionScript writes the task's resolved render.lua and params.json into a
// fresh folder, writes -execute code of the task's form naming that render.lua,
// and runs it under fusionRunner from a different, empty folder, so the script
// must find params.json beside itself. It returns the calls the fakes recorded,
// as canonical JSON, and what the script wrote to stdout.
func runFusionScript(t *testing.T, lua string, task presettest.TaskSnapshot, mode string) (calls, stdout string) {
	t.Helper()
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "task")
	elsewhere := filepath.Join(dir, "elsewhere")
	for _, d := range []string{taskDir, elsewhere} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if !fusionExecute.MatchString(task.Args[len(task.Args)-1]) {
		t.Fatalf("-execute code %q is not dofile([==[<script>]==])", task.Args[len(task.Args)-1])
	}
	script := filepath.Join(taskDir, "render.lua")
	code := "dofile([==[" + script + "]==])"
	var specs []string
	for _, tool := range fusionTools {
		specs = append(specs, "{ "+luaLiteral(tool.name)+", "+luaLiteral(tool.id)+" }")
	}
	runner := filepath.Join(dir, "runner.lua")
	codeFile := filepath.Join(dir, "execute.lua")
	record := filepath.Join(dir, "calls.json")
	for path, data := range map[string]string{
		script:                                taskFile(t, task, "render"),
		filepath.Join(taskDir, "params.json"): taskFile(t, task, "params"),
		runner:                                fmt.Sprintf(fusionRunner, strings.Join(specs, ", ")),
		codeFile:                              code,
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(t.Context(), lua, runner, codeFile, record, mode)
	cmd.Dir = elsewhere
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("runner failed: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("runner recorded nothing: %v\nstderr:\n%s", err, errOut.String())
	}
	// A Windows Lua writes stdout in text mode, so each line ends "\r\n".
	return canonicalJSON(t, data), strings.ReplaceAll(out.String(), "\r\n", "\n")
}

// TestPresetFusion_ScriptDrivesFusion runs the shipped render.lua, as resolved
// for a real task, under a stock Lua with Fusion faked, and pins the calls it
// makes: the comp loaded from params.json, every Saver but the chosen one put
// in pass-through and the chosen one taken out of it, one Render of the task's
// first to last frame that waits, the success line, then Quit, and never a
// Save. A Saver the comp lacks, a tool that is not a Saver, a comp that does
// not load and a render that fails each print the failure line, naming the
// comp's Savers where the name was wrong, and quit without rendering anything
// else.
func TestPresetFusion_ScriptDrivesFusion(t *testing.T) {
	lua := findLua(t)
	const scene = "/mnt/show/seq010/shot0010/comp/shot0010_comp_v007.comp"
	const savers = `"MainOut", "Out \"A\" \\ ` + "\u00e9" + `", "Saver1"`
	load := []any{"LoadComp", filepath.FromSlash(scene)}
	quit := []any{"Quit"}
	pass := func(name string, on bool) []any {
		return []any{"SetAttrs", name, map[string]any{"TOOLB_PassThrough": on}}
	}
	render := func(start, end int) []any {
		return []any{"Render", map[string]any{"Start": start, "End": end, "Wait": true}}
	}
	selectQuoted := []any{pass("MainOut", true), pass("Saver1", true), pass(fusionQuotedSaver, false)}

	tests := []struct {
		name     string
		saver    string
		frames   string
		mode     string
		want     []any
		wantFail string
	}{
		{
			name: "renders-one-saver", saver: fusionQuotedSaver, frames: "3-7", mode: "ok",
			want: append(append([]any{load}, selectQuoted...), render(3, 7), quit),
		},
		{
			name: "negative-frames", saver: "MainOut", frames: "-5--1", mode: "ok",
			want: []any{
				load, pass(fusionQuotedSaver, true), pass("Saver1", true), pass("MainOut", false),
				render(-5, -1), quit,
			},
		},
		{
			name: "saver-missing", saver: "Saver9", frames: "1-2", mode: "ok",
			want:     []any{load, quit},
			wantFail: `the comp has no tool named "Saver9"; its Savers are ` + savers,
		},
		{
			name: "not-a-saver", saver: "Merge1", frames: "1-2", mode: "ok",
			want:     []any{load, quit},
			wantFail: `the tool "Merge1" is a Merge, not a Saver; the comp's Savers are ` + savers,
		},
		{
			name: "render-fails", saver: fusionQuotedSaver, frames: "3-4", mode: "render-nil",
			want:     append(append([]any{load}, selectQuoted...), render(3, 4), quit),
			wantFail: "Fusion reported that the render did not succeed (Render returned nil)",
		},
		{
			name: "comp-fails-to-load", saver: "MainOut", frames: "1", mode: "load-nil",
			want:     []any{load, quit},
			wantFail: "Fusion could not load the comp " + filepath.FromSlash(scene),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap, err := capturePreset(t, fusionSaverPreset, map[string]string{
				"SceneFile": scene, "Frames": tc.frames, "Saver": tc.saver, "FramesPerTask": "10",
			})
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			if len(snap.Steps[0].Tasks) != 1 {
				t.Fatalf("frames %s must be one task, got %d", tc.frames, len(snap.Steps[0].Tasks))
			}
			wantJSON := canonicalJSON(t, mustMarshal(t, tc.want))
			calls, stdout := runFusionScript(t, lua, snap.Steps[0].Tasks[0], tc.mode)
			if calls != wantJSON {
				t.Errorf("Fusion calls:\n  got  %s\n  want %s", calls, wantJSON)
			}
			if gotOK, wantOK := strings.Contains(stdout, "SQI_RENDER: OK\n"), tc.wantFail == ""; gotOK != wantOK {
				t.Errorf("success line printed = %v, want %v; stdout:\n%s", gotOK, wantOK, stdout)
			}
			if tc.wantFail == "" {
				if strings.Contains(stdout, "SQI_RENDER: FAILED:") {
					t.Errorf("failure line printed on success; stdout:\n%s", stdout)
				}
				return
			}
			for _, line := range []string{"SQI_RENDER: FAILED: " + tc.wantFail + "\n", "openjd_fail: " + tc.wantFail + "\n"} {
				if !strings.Contains(stdout, line) {
					t.Errorf("stdout lacks %q; stdout:\n%s", line, stdout)
				}
			}
		})
	}
}

// TestPresetFusion_EmitsStatus pins the one directive render.lua writes to
// standard output: a status line naming the task's first and last frame, before
// the Render call. There is no progress line, as Render with Wait blocks and
// reports nothing between its start and end. Only the stub Fusion has been run;
// no Fusion has.
func TestPresetFusion_EmitsStatus(t *testing.T) {
	lua := findLua(t)
	snap, err := capturePreset(t, fusionSaverPreset, map[string]string{
		"SceneFile": "/mnt/show/shot0010.comp", "Frames": "3-7", "Saver": "MainOut", "FramesPerTask": "10",
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	_, stdout := runFusionScript(t, lua, snap.Steps[0].Tasks[0], "ok")
	want := "openjd_status: rendering frames 3–7\nSQI_RENDER: OK\n"
	if stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", stdout, want)
	}
	if strings.Contains(stdout, "openjd_progress") {
		t.Errorf("stdout has a progress line:\n%s", stdout)
	}
}
