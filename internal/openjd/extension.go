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

// registry is the set of OpenJD extension names sqi fully implements, keyed by
// name (case-sensitive). validateExtensions rejects any declared extension not
// present here.
//
//   - TASK_CHUNKING: chunked integer task parameters (CHUNK[INT]); implemented
//     in expand.go.
//   - REDACTED_ENV_VARS: the openjd_redacted_env stdout directive; implemented
//     in internal/worker/openjd/envdirective.go and internal/worker/session.
//
// Read-only after initialisation; never modified at runtime.
var registry = map[string]Extension{
	"TASK_CHUNKING": {
		Name:    "TASK_CHUNKING",
		Origin:  OriginOfficial,
		Status:  "supported",
		Summary: "Chunked integer task parameters (CHUNK[INT]).",
		DocPath: "docs/openjd-extensions/task-chunking.md",
	},
	"REDACTED_ENV_VARS": {
		Name:    "REDACTED_ENV_VARS",
		Origin:  OriginOfficial,
		Status:  "supported",
		Summary: "openjd_redacted_env stdout directive; redacts a var's value from logs.",
		DocPath: "docs/openjd-extensions/redacted-env-vars.md",
	},
}

// LookupExtension returns the registry entry for name and whether it exists.
func LookupExtension(name string) (Extension, bool) {
	ext, ok := registry[name]
	return ext, ok
}
