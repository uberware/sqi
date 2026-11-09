// SPDX-License-Identifier: AGPL-3.0-or-later

package brokerauth_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
)

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestUserPermissions_OfWorkerPermissions: a worker's grants in user-JWT form,
// as the auth callout issues them, are its WorkerPermissions exactly.
func TestUserPermissions_OfWorkerPermissions(t *testing.T) {
	for _, id := range []string{"w1", "7b1f6c52-3f0e-4c2a-9d57-0a1b2c3d4e5f"} {
		t.Run(id, func(t *testing.T) {
			legacy := brokerauth.WorkerPermissions(id)
			jwtForm := brokerauth.UserPermissions(legacy)

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
			if jwtForm.Resp == nil || jwtForm.Resp.MaxMsgs != legacy.Response.MaxMsgs || jwtForm.Resp.Expires != legacy.Response.Expires {
				t.Errorf("Resp = %+v, want the response grant %+v", jwtForm.Resp, legacy.Response)
			}
		})
	}
}

func TestUserPermissions_CarriesEveryPart(t *testing.T) {
	tests := []struct {
		name string
		in   natsserver.Permissions
		want jwt.Permissions
	}{
		{"empty", natsserver.Permissions{}, jwt.Permissions{}},
		{
			"allow and deny",
			natsserver.Permissions{
				Publish:   &natsserver.SubjectPermission{Allow: []string{"a.>", "b"}, Deny: []string{"a.secret"}},
				Subscribe: &natsserver.SubjectPermission{Allow: []string{"c.*"}, Deny: []string{"c.x", "c.y"}},
			},
			jwt.Permissions{
				Pub: jwt.Permission{Allow: jwt.StringList{"a.>", "b"}, Deny: jwt.StringList{"a.secret"}},
				Sub: jwt.Permission{Allow: jwt.StringList{"c.*"}, Deny: jwt.StringList{"c.x", "c.y"}},
			},
		},
		{
			"publish only",
			natsserver.Permissions{Publish: &natsserver.SubjectPermission{Allow: []string{"p"}}},
			jwt.Permissions{Pub: jwt.Permission{Allow: jwt.StringList{"p"}}},
		},
		{
			"response",
			natsserver.Permissions{Response: &natsserver.ResponsePermission{MaxMsgs: 3, Expires: 2 * time.Second}},
			jwt.Permissions{Resp: &jwt.ResponsePermission{MaxMsgs: 3, Expires: 2 * time.Second}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := brokerauth.UserPermissions(&tt.in)
			if fmt.Sprintf("%+v", got.Pub) != fmt.Sprintf("%+v", tt.want.Pub) {
				t.Errorf("Pub = %+v, want %+v", got.Pub, tt.want.Pub)
			}
			if fmt.Sprintf("%+v", got.Sub) != fmt.Sprintf("%+v", tt.want.Sub) {
				t.Errorf("Sub = %+v, want %+v", got.Sub, tt.want.Sub)
			}
			switch {
			case (got.Resp == nil) != (tt.want.Resp == nil):
				t.Errorf("Resp = %+v, want %+v", got.Resp, tt.want.Resp)
			case got.Resp != nil && *got.Resp != *tt.want.Resp:
				t.Errorf("Resp = %+v, want %+v", *got.Resp, *tt.want.Resp)
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
