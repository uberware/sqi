// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

// Origin distinguishes an upstream OpenJD extension from one defined by sqi.
type Origin string

const (
	// OriginOfficial marks an extension defined by the upstream OpenJD spec.
	OriginOfficial Origin = "official"
	// OriginVendor marks an extension defined by sqi. Vendor extension names
	// MUST carry the SQI_ prefix so they can never collide with a future
	// official OpenJD name; if upstreamed, the prefix is dropped and the entry
	// flips to OriginOfficial (the "promotion path").
	OriginVendor Origin = "vendor"
)

// Extension status values. Status is the SECOND half of the registration gate:
// validateExtensions accepts an extension only when it is both present in the
// registry AND marked supported.
//
// The split exists so that a partially-implemented extension can be present —
// so the conformance harness can drive it through the real parse and validate
// path — while production still rejects it, for a stated reason rather than
// because a map lookup missed.
const (
	// StatusSupported means the extension is fully implemented and accepted.
	StatusSupported = "supported"
	// StatusInProgress means the extension is registered for conformance
	// scoring but is NOT accepted: some required part of it is unimplemented.
	StatusInProgress = "in-progress"
)

// Extension is one entry in the supported-extension registry.
type Extension struct {
	// Name is the OpenJD extension identifier, matching [A-Z_0-9]{3,128}.
	Name string
	// Origin is official (upstream OpenJD) or vendor (sqi-defined).
	Origin Origin
	// Status is the implementation status, e.g. "supported".
	Status string
	// Summary is a one-line description.
	Summary string
	// DocPath points at the extension's contribution doc, repo-relative.
	DocPath string
}

// registry is the set of OpenJD extension names sqi knows about, keyed by name
// (case-sensitive). Presence alone is not enough for validateExtensions to
// accept a declared extension — the entry's Status must also be
// StatusSupported; see the Status doc comment for why. validateExtensions
// rejects any declared extension not present here at all.
//
//   - TASK_CHUNKING: chunked integer task parameters (CHUNK[INT]); implemented
//     in expand.go.
//   - REDACTED_ENV_VARS: the openjd_redacted_env stdout directive; implemented
//     in internal/worker/openjd/envdirective.go and internal/worker/session.
//   - SQI_CHUNK_BOUNDS: derives Task.Param.<name>.Start/.End for CHUNK[INT]
//     parameters; implemented in expand.go (DeriveChunkBounds) and wired in
//     submit.go.
//   - EXPR: the OpenJD expression language; implemented across internal/openjd
//     (scope model, checker, resolver), internal/openjd/expr and
//     internal/worker/fmtres. Supported since sub-project H2 — see that entry's
//     own comment.
//
// Read-only after initialisation; never modified at runtime.
var registry = map[string]Extension{
	"TASK_CHUNKING": {
		Name:    "TASK_CHUNKING",
		Origin:  OriginOfficial,
		Status:  StatusSupported,
		Summary: "Chunked integer task parameters (CHUNK[INT]).",
		DocPath: "docs/openjd-extensions/task-chunking.md",
	},
	"REDACTED_ENV_VARS": {
		Name:    "REDACTED_ENV_VARS",
		Origin:  OriginOfficial,
		Status:  StatusSupported,
		Summary: "openjd_redacted_env stdout directive; redacts a var's value from logs.",
		DocPath: "docs/openjd-extensions/redacted-env-vars.md",
	},
	"SQI_PATH_TRANSLATION": {
		Name:    "SQI_PATH_TRANSLATION",
		Origin:  OriginVendor,
		Status:  StatusSupported,
		Summary: "Per-product path-delivery checklist (swap/file/flags/env/stage).",
		DocPath: "docs/openjd-extensions/path-translation.md",
	},
	"SQI_CHUNK_BOUNDS": {
		Name:    "SQI_CHUNK_BOUNDS",
		Origin:  OriginVendor,
		Status:  StatusSupported,
		Summary: "Exposes a CHUNK[INT] chunk's first/last integer as Task.Param.<name>.Start/.End.",
		DocPath: "docs/openjd-extensions/sqi-chunk-bounds.md",
	},
	// EXPR is the OpenJD expression language extension, delivered across the
	// EXPR program's sub-projects A0 and A-I. It was registered but NOT
	// supported until sub-project H2 flipped it here: section 1.2.2 makes the
	// BOOL, RANGE_EXPR and LIST[*] job parameter types part of this extension,
	// and those were sub-project F's, so accepting an EXPR template before F
	// landed would have shipped a partial implementation the extension's own
	// contract forbids. F shipped, so the whole extension is now accepted and
	// every EXPR template is submittable.
	ExtensionEXPR: {
		Name:    ExtensionEXPR,
		Origin:  OriginOfficial,
		Status:  StatusSupported,
		Summary: "Python-subset expression language in format strings.",
		DocPath: "docs/openjd-extensions/expr.md",
	},
}

// ExtensionEXPR is the registry name of the OpenJD expression-language
// extension.
//
// Exported for internal/scheduler, which decides per lease request whether a
// job needs a worker capable of phase-3 expression evaluation. It compares this
// name against the extension list [Submitter.Submit] persisted on the job row,
// so the test is EXACT for every job submitted since migration 00027. Rows
// older than that column record no list, and for those alone the scheduler
// falls back to scanning the raw template for these bytes -- a heuristic whose
// two error directions are written out in that package's exprcaps.go.
const ExtensionEXPR = "EXPR"

// LookupExtension returns the registry entry for name and whether it exists.
func LookupExtension(name string) (Extension, bool) {
	ext, ok := registry[name]
	return ext, ok
}
