// SPDX-License-Identifier: AGPL-3.0-or-later

package bus_test

import (
	"encoding/hex"
	"slices"
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/bus"
)

func TestGrantsFingerprint_StableAndSensitive(t *testing.T) {
	t.Parallel()
	fp := bus.GrantsFingerprint()
	if raw, err := hex.DecodeString(fp); err != nil || len(raw) != 32 {
		t.Fatalf("GrantsFingerprint() = %q, want 64 hex digits of SHA-256", fp)
	}
	if again := bus.GrantsFingerprint(); again != fp {
		t.Errorf("GrantsFingerprint() changed between calls: %q then %q", fp, again)
	}
	if got := bus.GrantsFingerprintOf(brokerauth.WorkerPermissions); got != fp {
		t.Errorf("fingerprint of brokerauth.WorkerPermissions = %q, want GrantsFingerprint() %q", got, fp)
	}

	// Any change to what a worker is granted, in either direction, moves it.
	edits := []struct {
		name string
		edit func(p *natsserver.Permissions)
	}{
		{"publish subject added", func(p *natsserver.Permissions) {
			p.Publish.Allow = append(p.Publish.Allow, "task.extra.>")
		}},
		{"publish subject removed", func(p *natsserver.Permissions) {
			p.Publish.Allow = p.Publish.Allow[1:]
		}},
		{"subscribe subject added", func(p *natsserver.Permissions) {
			p.Subscribe.Allow = append(p.Subscribe.Allow, "worker.>")
		}},
		{"publish deny added", func(p *natsserver.Permissions) {
			p.Publish.Deny = []string{"task.cancel.>"}
		}},
		{"response permission added", func(p *natsserver.Permissions) {
			p.Response = &natsserver.ResponsePermission{MaxMsgs: 1}
		}},
	}
	seen := map[string]string{fp: "the shipped grants"}
	for _, e := range edits {
		got := bus.GrantsFingerprintOf(func(id string) *natsserver.Permissions {
			p := brokerauth.WorkerPermissions(id)
			e.edit(p)
			return p
		})
		if prev, dup := seen[got]; dup {
			t.Errorf("%s: fingerprint %q equals that of %s", e.name, got, prev)
		}
		seen[got] = e.name
	}

	// The encoding is canonical: the same grants listed in another order are
	// the same grants.
	reordered := bus.GrantsFingerprintOf(func(id string) *natsserver.Permissions {
		p := brokerauth.WorkerPermissions(id)
		slices.Reverse(p.Publish.Allow)
		slices.Reverse(p.Subscribe.Allow)
		return p
	})
	if reordered != fp {
		t.Errorf("the same grants in another order hash to %q, want %q", reordered, fp)
	}

	// The worker ID the grants are sampled for is part of what is hashed, so
	// grants that embed it differently differ.
	renamed := bus.GrantsFingerprintOf(func(string) *natsserver.Permissions {
		return brokerauth.WorkerPermissions("another-worker")
	})
	if renamed == fp {
		t.Error("grants for a different worker ID hash the same as the sample's")
	}
}
