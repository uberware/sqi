// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Submit to sqi: After Effects ScriptUI panel.
//
// Install into the Scripts/ScriptUI Panels folder (together with json2.js), restart After
// Effects, then open "Submit to sqi.jsx" from the Window menu. It also runs as a floating
// palette from File > Scripts > Run Script File.
//
// The panel writes a version 1 handoff context file (see docs/dcc-submitters.md) describing
// every QUEUED render-queue item, then launches `sqi-submit --context <file>`, which opens
// the shared submit dialog.
//
// ExtendScript is ES3: no let/const, no arrow functions, no Array.prototype.forEach, no
// native JSON (json2.js supplies it). Keep the scene arithmetic in the small pure functions
// below, which tests run under Node without After Effects.

#include "json2.js"

var SQI_SETTINGS_SECTION = "sqi";
var SQI_SETTINGS_KEY = "SubmitCommand";
var SQI_PREF_KEY = "Pref_SCRIPTING_FILE_NETWORK_SECURITY";
var SQI_PREF_SECTIONS = ["Main Pref Section v2", "Main Pref Section"];

var SQI_MSG = {
    noProject: "Open a project before submitting to sqi.",
    neverSaved:
        "This project has never been saved. Save it first (File > Save As): the farm renders " +
        "the file on disk.",
    savePrompt: "The project has unsaved changes. Save it before submitting to sqi?",
    notSaved: "Save the project before submitting: the farm renders the file on disk.",
    saveFailed: "The project could not be saved: ",
    noFileAccess:
        "Turn on Preferences > Scripting & Expressions > \"Allow Scripts to Write Files and " +
        "Access Network\", then submit again.",
    noQueued:
        "Nothing is queued. Add a composition to the Render Queue and leave its status " +
        "Queued, then submit again.",
    noCommand:
        "sqi-submit was not found. Install it with pip install \"sqi-submitter[qt]\" " +
        "(or pipx install \"sqi-submitter[qt]\"), then click \"Locate sqi-submit\u2026\" " +
        "in this panel and choose the sqi-submit program.",
    commandMissing:
        "The saved sqi-submit location no longer exists: click \"Locate sqi-submit\u2026\" " +
        "and choose it again. Saved location: ",
    badPath:
        "sqi cannot launch a program or temp file whose path contains a quote, $, % or " +
        "backtick character. Move or reinstall it somewhere without one. Path: ",
    writeFailed: "The submit context file could not be written to "
};

// -- pure functions (run under Node by the tests) ---------------------------------------

// The frame range After Effects shows the artist, as "S-E" (inclusive).
function sqiFrameRange(displayStartFrame, timeSpanStart, timeSpanDuration, frameDuration) {
    var start = displayStartFrame + Math.round(timeSpanStart / frameDuration);
    var end = start + Math.round(timeSpanDuration / frameDuration) - 1;
    return start + "-" + end;
}

// An image sequence has a [#####] frame token in its output file name; anything else is a
// movie.
function sqiIsSequence(fileName) {
    if (fileName === null || fileName === undefined) {
        return false;
    }
    return String(fileName).indexOf("[#") !== -1;
}

// The number of frames in an "S-E" range.
function sqiFrameCount(frameRange) {
    var m = /^(-?\d+)-(-?\d+)$/.exec(frameRange);
    if (m === null) {
        return 1;
    }
    return parseInt(m[2], 10) - parseInt(m[1], 10) + 1;
}

// One render target. A movie cannot be split across machines, so it carries its whole frame
// count as FramesPerTask and its own range as Frames, and locks both: an edit to either
// (a wider range, a step, a gap) would split it into several tasks. A sequence splits freely.
function sqiTarget(index, compName, frameRange, outputFsName, isSequence) {
    var extra = { RenderQueueItem: String(index) };
    var locked = [];
    if (!isSequence) {
        extra.FramesPerTask = String(sqiFrameCount(frameRange));
        extra.Frames = frameRange;
        locked.push("FramesPerTask");
        locked.push("Frames");
    }
    return {
        name: compName,
        kind: "render_queue_item",
        frame_range: frameRange,
        output_path: outputFsName === undefined ? null : outputFsName,
        extra: extra,
        locked: locked
    };
}

// The names made unique: the first occurrence of a name is kept, and every later duplicate
// becomes "<name> (#<k>)", k being indexes[i] (default: the 1-based position). A candidate
// that is itself already taken gets " (#<k>)" again until it is free.
function sqiUniqueNames(names, indexes) {
    var taken = {};
    var result = [];
    var i;
    for (i = 0; i < names.length; i += 1) {
        taken["n:" + names[i]] = true;
    }
    var seen = {};
    for (i = 0; i < names.length; i += 1) {
        var name = names[i];
        if (seen["n:" + name] === true) {
            var suffix = " (#" + (indexes === undefined ? i + 1 : indexes[i]) + ")";
            name = name + suffix;
            while (taken["n:" + name] === true) {
                name = name + suffix;
            }
            taken["n:" + name] = true;
        }
        seen["n:" + names[i]] = true;
        result.push(name);
    }
    return result;
}

// The targets with unique names, a duplicate suffixed with its render-queue index, so two
// items for one comp (a ProRes and an H.264 output, say) give two distinct job names.
function sqiUniqueTargetNames(targets) {
    var names = [];
    var indexes = [];
    var i;
    for (i = 0; i < targets.length; i += 1) {
        names.push(targets[i].name);
        indexes.push(targets[i].extra.RenderQueueItem);
    }
    var unique = sqiUniqueNames(names, indexes);
    for (i = 0; i < targets.length; i += 1) {
        targets[i].name = unique[i];
    }
    return targets;
}

// The smallest range covering every "S-E" range given.
function sqiUnionRange(ranges) {
    var lo = null;
    var hi = null;
    var i;
    for (i = 0; i < ranges.length; i += 1) {
        var m = /^(-?\d+)-(-?\d+)$/.exec(ranges[i]);
        if (m !== null) {
            var s = parseInt(m[1], 10);
            var e = parseInt(m[2], 10);
            if (lo === null || s < lo) {
                lo = s;
            }
            if (hi === null || e > hi) {
                hi = e;
            }
        }
    }
    return lo === null ? null : lo + "-" + hi;
}

// The version 1 handoff document for the targets read from the render queue.
function sqiBuildContext(hostVersion, scenePath, targets) {
    var ranges = [];
    var i;
    for (i = 0; i < targets.length; i += 1) {
        ranges.push(targets[i].frame_range);
    }
    return {
        version: 1,
        host: { name: "aftereffects", display_name: "After Effects", version: hostVersion },
        scene: {
            path: scenePath,
            frame_range: sqiUnionRange(ranges),
            output_path: null,
            renderer: null
        },
        targets: targets,
        server_url: null
    };
}

// The system.callSystem string that starts sqi-submit without blocking After Effects, or
// null when a path could not be quoted safely (the caller alerts; nothing is ever escaped).
// --detached makes sqi-submit show a startup failure in a message box: `start` gives it a
// console of its own that closes with it, so stderr alone would reach nobody.
function sqiLaunchCommand(isWindows, exe, contextPath) {
    var bad = isWindows ? /["%]/ : /["$\x60\\]/;
    if (bad.test(exe) || bad.test(contextPath)) {
        return null;
    }
    if (isWindows) {
        return "cmd /c start \"\" \"" + exe + "\" --context \"" + contextPath + "\" --detached";
    }
    return "\"" + exe + "\" --context \"" + contextPath + "\" --detached >/dev/null 2>&1 &";
}

// Whether "Allow Scripts to Write Files and Access Network" is on. readPref(section) returns
// the preference's number or throws when the section is absent; the key lives under one of
// two section names depending on the After Effects version, so either one being on is enough.
// Returns null when neither can be read (the file write then reports the problem).
function sqiFileAccessAllowed(readPref) {
    var readable = false;
    var i;
    for (i = 0; i < SQI_PREF_SECTIONS.length; i += 1) {
        try {
            var value = Number(readPref(SQI_PREF_SECTIONS[i]));
            readable = true;
            if (value === 1) {
                return true;
            }
        } catch (e) {
            // This section does not exist in this After Effects version.
        }
    }
    return readable ? false : null;
}

// -- After Effects glue (needs a real After Effects; the manual checklist covers it) ----

function sqiIsWindows() {
    return $.os.indexOf("Windows") !== -1;
}

function sqiReadFileAccessPref() {
    return sqiFileAccessAllowed(function (section) {
        return app.preferences.getPrefAsLong(section, SQI_PREF_KEY);
    });
}

function sqiSavedCommand() {
    if (app.settings.haveSetting(SQI_SETTINGS_SECTION, SQI_SETTINGS_KEY)) {
        return app.settings.getSetting(SQI_SETTINGS_SECTION, SQI_SETTINGS_KEY);
    }
    return null;
}

// Offers to save a dirty project. Returns true when the project on disk is current.
function sqiEnsureSaved() {
    if (app.project.file === null) {
        alert(SQI_MSG.neverSaved);
        return false;
    }
    if (app.project.dirty) {
        if (!confirm(SQI_MSG.savePrompt)) {
            alert(SQI_MSG.notSaved);
            return false;
        }
        try {
            app.project.save();
        } catch (e) {
            alert(SQI_MSG.saveFailed + e.toString());
            return false;
        }
    }
    return true;
}

function sqiOutputFile(item) {
    try {
        return item.outputModule(1).file;
    } catch (e) {
        return null;
    }
}

// Every QUEUED render-queue item as a target, with unique names.
function sqiReadQueuedTargets() {
    var targets = [];
    var queue = app.project.renderQueue;
    var i;
    for (i = 1; i <= queue.numItems; i += 1) {
        var item = queue.item(i);
        if (item.status !== RQItemStatus.QUEUED) {
            continue;
        }
        var comp = item.comp;
        var range = sqiFrameRange(
            comp.displayStartFrame,
            item.timeSpanStart,
            item.timeSpanDuration,
            comp.frameDuration
        );
        var file = sqiOutputFile(item);
        var fsName = file === null ? null : file.fsName;
        // File.name is URI-encoded ("%5B%23" for "[#"); displayName is decoded.
        var fileName = file === null ? null : file.displayName;
        targets.push(sqiTarget(i, comp.name, range, fsName, sqiIsSequence(fileName)));
    }
    return sqiUniqueTargetNames(targets);
}

// Writes the context to <temp>/sqi-handoff/<timestamp>.json. Returns the path or null.
function sqiWriteContext(context) {
    var folder = new Folder(Folder.temp.fsName + "/sqi-handoff");
    if (!folder.exists && !folder.create()) {
        alert(SQI_MSG.writeFailed + folder.fsName + ".");
        return null;
    }
    var file = new File(folder.fsName + "/" + new Date().getTime() + ".json");
    file.encoding = "UTF-8";
    if (!file.open("w")) {
        alert(SQI_MSG.writeFailed + folder.fsName + ".");
        return null;
    }
    var ok = file.write(JSON.stringify(context));
    file.close();
    if (!ok) {
        alert(SQI_MSG.writeFailed + folder.fsName + ".");
        return null;
    }
    return file.fsName;
}

function sqiSubmit() {
    if (app.project === null || app.project === undefined) {
        alert(SQI_MSG.noProject);
        return;
    }
    if (!sqiEnsureSaved()) {
        return;
    }
    if (sqiReadFileAccessPref() === false) {
        alert(SQI_MSG.noFileAccess);
        return;
    }
    var targets = sqiReadQueuedTargets();
    if (targets.length === 0) {
        alert(SQI_MSG.noQueued);
        return;
    }
    var exe = sqiSavedCommand();
    if (exe === null || exe === "") {
        alert(SQI_MSG.noCommand);
        return;
    }
    if (!new File(exe).exists) {
        alert(SQI_MSG.commandMissing + exe);
        return;
    }
    var context = sqiBuildContext(app.version, app.project.file.fsName, targets);
    var contextPath = sqiWriteContext(context);
    if (contextPath === null) {
        return;
    }
    var command = sqiLaunchCommand(sqiIsWindows(), exe, contextPath);
    if (command === null) {
        alert(SQI_MSG.badPath + exe + " / " + contextPath);
        return;
    }
    system.callSystem(command);
}

function sqiLocate() {
    var prompt = "Locate sqi-submit";
    var chosen = sqiIsWindows()
        ? File.openDialog(prompt, "Programs:*.exe;*.cmd;*.bat,All files:*.*")
        : File.openDialog(prompt);
    if (chosen === null) {
        return null;
    }
    app.settings.saveSetting(SQI_SETTINGS_SECTION, SQI_SETTINGS_KEY, chosen.fsName);
    return chosen.fsName;
}

function sqiCommandSummary() {
    var saved = sqiSavedCommand();
    return saved === null || saved === ""
        ? "sqi-submit: not set. Click Locate sqi-submit\u2026"
        : "sqi-submit: " + saved;
}

function BuildUI(parent) {
    var panel =
        parent instanceof Panel
            ? parent
            : new Window("palette", "Submit to sqi", undefined, { resizeable: true });
    panel.orientation = "column";
    panel.alignChildren = ["fill", "top"];
    panel.margins = 10;
    panel.spacing = 8;

    var summary = panel.add("statictext", undefined, sqiCommandSummary(), { multiline: true });
    summary.minimumSize.width = 220;
    var submit = panel.add("button", undefined, "Submit to sqi");
    var locate = panel.add("button", undefined, "Locate sqi-submit\u2026");

    submit.onClick = function () {
        sqiSubmit();
    };
    locate.onClick = function () {
        var chosen = sqiLocate();
        if (chosen !== null) {
            summary.text = sqiCommandSummary();
        }
    };

    panel.layout.layout(true);
    panel.layout.resize();
    panel.onResizing = panel.onResize = function () {
        this.layout.resize();
    };
    if (panel instanceof Window) {
        panel.center();
        panel.show();
    }
    return panel;
}

// Window exists only inside After Effects; the tests load this file without it.
if (typeof Window !== "undefined") {
    BuildUI(this);
}
