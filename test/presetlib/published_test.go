// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build presetlib

// Package presetlib_test validates the PUBLISHED preset library against the
// validator in this working tree.
//
// The copy published at uberware.github.io/sqi-presets is refreshed only on
// release. A validator change that tightens what a preset may contain (for
// example, a PATH parameter may not use LINE_EDIT) can be applied to every
// preset in this repo in the same change, but the published copy stays
// invalid until the next release, and nothing else reports it: the list page
// renders from the index (which needs no validation) and only the detail page
// parses, so the first signal is a user clicking a preset and getting an
// error.
//
// This test runs the published bytes through the same
// path the server uses -- presetlib.FetchDefinition, which pins the sha256 and
// then calls product.ParseDefinition -- so a validator change that invalidates
// published content fails here rather than in someone's browser.
//
// Build tag `presetlib` keeps it out of `make ci`: it needs the network, and a
// unit-test suite that reaches the internet is a flake generator. Run it with
// `make test-preset-library`.
package presetlib_test

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/presetlib"
	"github.com/uberware/sqi/internal/product"
)

// defaultIndexURL mirrors config.Defaults()' preset_library.url. Duplicated
// rather than imported so this test validates what operators actually get by
// default, and fails if that default is repointed.
const defaultIndexURL = "https://uberware.github.io/sqi-presets/index.json"

// indexURL is the library under test. SQI_TEST_PRESET_LIBRARY_URL points it at
// a staging index or a local file server.
func indexURL() string {
	if u := os.Getenv("SQI_TEST_PRESET_LIBRARY_URL"); u != "" {
		return u
	}
	return defaultIndexURL
}

// unreachable reports whether err is a transport failure rather than a verdict
// about the content. An offline runner must SKIP; invalid content must FAIL.
// Collapsing the two would let a real breakage hide behind a network blip.
func unreachable(err error) bool {
	var dnsErr *net.DNSError
	var opErr *net.OpError
	return errors.As(err, &dnsErr) || errors.As(err, &opErr) || errors.Is(err, context.DeadlineExceeded)
}

// validateOptions mirrors what the preset routes pass in production: limits
// enforced, EXPR budget left at its defaults (this test has no operator
// configuration to offer), and a generous deadline since we are validating a
// whole library rather than serving one request.
func validateOptions() product.ValidateOptions {
	return product.ValidateOptions{EnforceLimits: true}
}

func TestPublishedPresets_ValidateAgainstThisTree(t *testing.T) {
	url := indexURL()
	svc := presetlib.New(url, time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	entries, err := svc.FetchIndex(ctx, true)
	if err != nil {
		if unreachable(err) {
			t.Skipf("preset library %s unreachable, skipping: %v", url, err)
		}
		t.Fatalf("fetch index %s: %v", url, err)
	}
	if len(entries) == 0 {
		t.Fatalf("preset library %s lists no presets", url)
	}
	t.Logf("validating %d published presets from %s", len(entries), url)

	for _, entry := range entries {
		t.Run(entry.Name, func(t *testing.T) {
			// FetchDefinition is the production path: it verifies the index's
			// sha256 before parsing, so this also catches a definition that
			// drifted from the fingerprint the index vouches for.
			if _, err := svc.FetchDefinition(ctx, entry, validateOptions()); err != nil {
				if unreachable(err) {
					t.Skipf("definition %s unreachable: %v", entry.Definition, err)
				}
				t.Errorf("published preset %q no longer validates against this tree.\n"+
					"  definition: %s\n"+
					"  error: %v\n"+
					"This means the published library is stale relative to the validator in\n"+
					"this working tree. Publish the corrected presets (the release workflow\n"+
					"regenerates the library from presets/), or, if the validator change was\n"+
					"unintended, revert it.", entry.Name, entry.Definition, err)
			}
		})
	}
}
