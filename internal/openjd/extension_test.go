// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"regexp"
	"strings"
	"testing"
)

func TestLookupExtension(t *testing.T) {
	tests := []struct {
		name       string
		lookup     string
		wantFound  bool
		wantOrigin Origin
	}{
		{name: "task chunking is official", lookup: "TASK_CHUNKING", wantFound: true, wantOrigin: OriginOfficial},
		{name: "redacted env vars is official", lookup: "REDACTED_ENV_VARS", wantFound: true, wantOrigin: OriginOfficial},
		{name: "unknown name not found", lookup: "NOPE", wantFound: false},
		{name: "empty name not found", lookup: "", wantFound: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, found := LookupExtension(tc.lookup)
			if found != tc.wantFound {
				t.Fatalf("LookupExtension(%q) found = %v, want %v", tc.lookup, found, tc.wantFound)
			}
			if found && got.Origin != tc.wantOrigin {
				t.Errorf("LookupExtension(%q) origin = %q, want %q", tc.lookup, got.Origin, tc.wantOrigin)
			}
		})
	}
}

func TestLookupExtension_PathTranslationVendor(t *testing.T) {
	ext, ok := LookupExtension("SQI_PATH_TRANSLATION")
	if !ok {
		t.Fatal("SQI_PATH_TRANSLATION not registered")
	}
	if ext.Origin != OriginVendor {
		t.Errorf("Origin = %q, want %q", ext.Origin, OriginVendor)
	}
	if ext.DocPath != "docs/openjd-extensions/path-translation.md" {
		t.Errorf("DocPath = %q", ext.DocPath)
	}
}

func TestLookupExtension_ChunkBoundsVendor(t *testing.T) {
	ext, ok := LookupExtension("SQI_CHUNK_BOUNDS")
	if !ok {
		t.Fatal("SQI_CHUNK_BOUNDS not registered")
	}
	if ext.Origin != OriginVendor {
		t.Errorf("Origin = %q, want %q", ext.Origin, OriginVendor)
	}
	if ext.DocPath != "docs/openjd-extensions/sqi-chunk-bounds.md" {
		t.Errorf("DocPath = %q", ext.DocPath)
	}
}

// TestRegistryVendorNamespacing is the invariant that guarantees sqi-defined
// (vendor) extension names can never collide with a future official OpenJD name:
// every vendor entry must carry the SQI_ prefix and match the OpenJD name format.
func TestRegistryVendorNamespacing(t *testing.T) {
	nameRE := regexp.MustCompile(`^[A-Z_0-9]{3,128}$`)
	for name, ext := range registry {
		if name != ext.Name {
			t.Errorf("registry key %q != Extension.Name %q", name, ext.Name)
		}
		if !nameRE.MatchString(ext.Name) {
			t.Errorf("extension %q does not match [A-Z_0-9]{3,128}", ext.Name)
		}
		if ext.Origin == OriginVendor && (len(ext.Name) < 4 || ext.Name[:4] != "SQI_") {
			t.Errorf("vendor extension %q must be prefixed SQI_", ext.Name)
		}
	}
}

// TestValidateExtensions_RegisteredButNotSupported pins the two-part gate. An
// extension may be present in the registry while still being rejected, which is
// what lets the conformance harness reach the real validation path for a
// partially-implemented extension without production accepting it.
func TestValidateExtensions_RegisteredButNotSupported(t *testing.T) {
	tmpl := &JobTemplate{
		SpecificationVersion: "jobtemplate-2023-09",
		Name:                 "T",
		Extensions:           []string{"EXPR"},
	}
	errs := validateExtensions(tmpl)
	if len(errs) == 0 {
		t.Fatal("an in-progress extension was accepted; want a rejection")
	}
	msg := errs[0].Message
	if !strings.Contains(msg, "in-progress") {
		t.Errorf("message = %q, want it to name the status", msg)
	}
	if strings.Contains(msg, "unsupported extension") {
		t.Errorf("message = %q, want it NOT to reuse the unknown-name wording: "+
			"EXPR IS known, and saying otherwise misleads", msg)
	}
}

// TestValidateExtensions_SupportedStillPass guards the blast radius: the status
// check runs on the one path every extension flows through.
func TestValidateExtensions_SupportedStillPass(t *testing.T) {
	for _, name := range []string{
		"TASK_CHUNKING", "REDACTED_ENV_VARS", "SQI_PATH_TRANSLATION", "SQI_CHUNK_BOUNDS",
	} {
		t.Run(name, func(t *testing.T) {
			tmpl := &JobTemplate{
				SpecificationVersion: "jobtemplate-2023-09",
				Name:                 "T",
				Extensions:           []string{name},
			}
			if errs := validateExtensions(tmpl); len(errs) != 0 {
				t.Errorf("supported extension %q was rejected: %v", name, errs)
			}
		})
	}
}

// TestRegistry_EverySupportedStatusIsTheConstant stops a typo'd status string
// silently disabling a shipped extension.
func TestRegistry_EverySupportedStatusIsTheConstant(t *testing.T) {
	for name, ext := range registry {
		if name == "EXPR" {
			if ext.Status != StatusInProgress {
				t.Errorf("EXPR status = %q, want %q", ext.Status, StatusInProgress)
			}
			continue
		}
		if ext.Status != StatusSupported {
			t.Errorf("%s status = %q, want %q", name, ext.Status, StatusSupported)
		}
	}
}
