// SPDX-License-Identifier: AGPL-3.0-or-later

package brokerauth_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
)

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestWorkerUserPermissions_EquivalentToWorkerPermissions(t *testing.T) {
	for _, id := range []string{"w1", "7b1f6c52-3f0e-4c2a-9d57-0a1b2c3d4e5f"} {
		t.Run(id, func(t *testing.T) {
			legacy := brokerauth.WorkerPermissions(id)
			jwtForm := brokerauth.WorkerUserPermissions(id)

			gotPub := sortedCopy(jwtForm.Pub.Allow)
			wantPub := sortedCopy(legacy.Publish.Allow)
			if strings.Join(gotPub, "|") != strings.Join(wantPub, "|") {
				t.Errorf("pub allow = %v, want %v", gotPub, wantPub)
			}
			gotSub := sortedCopy(jwtForm.Sub.Allow)
			wantSub := sortedCopy(legacy.Subscribe.Allow)
			if strings.Join(gotSub, "|") != strings.Join(wantSub, "|") {
				t.Errorf("sub allow = %v, want %v", gotSub, wantSub)
			}
			if len(jwtForm.Pub.Deny) != 0 || len(jwtForm.Sub.Deny) != 0 {
				t.Errorf("deny lists must be empty, got pub=%v sub=%v", jwtForm.Pub.Deny, jwtForm.Sub.Deny)
			}
			if jwtForm.Resp != nil {
				t.Errorf("Resp must be nil, got %+v", jwtForm.Resp)
			}
		})
	}
}

func TestGenerateAccountSeed(t *testing.T) {
	seed, pub, err := brokerauth.GenerateAccountSeed()
	if err != nil {
		t.Fatalf("GenerateAccountSeed: %v", err)
	}
	kp, err := nkeys.FromSeed(seed)
	if err != nil {
		t.Fatalf("FromSeed: %v", err)
	}
	prefix, _, err := nkeys.DecodeSeed(seed)
	if err != nil {
		t.Fatalf("DecodeSeed: %v", err)
	}
	if prefix != nkeys.PrefixByteAccount {
		t.Errorf("seed prefix = %v, want account", prefix)
	}
	gotPub, err := kp.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	if gotPub != pub {
		t.Errorf("returned pub %q != derived %q", pub, gotPub)
	}
	if !strings.HasPrefix(pub, "A") {
		t.Errorf("public key %q must start with A", pub)
	}
	if !nkeys.IsValidPublicAccountKey(pub) {
		t.Errorf("public key %q is not a valid account key", pub)
	}
}
