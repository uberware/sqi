// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Tests for the shared external-login resolution used by both LDAP (C1) and
// OIDC (C2). The property under test throughout is that an account is found by
// its provider-assigned identifier, never by its name.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

func TestResolveExternalUser(t *testing.T) {
	tests := []struct {
		name       string
		seed       []store.User
		authSource string
		id         externalIdentity
		role       string
		ownsRole   bool
		wantErr    bool
		wantUser   string // username after resolution
		wantRole   string
	}{
		{
			name:       "first login provisions",
			authSource: store.AuthSourceOIDC,
			id:         externalIdentity{ExternalID: "sub-1", Username: "alice", DisplayName: "Alice"},
			role:       "user",
			wantUser:   "alice",
			wantRole:   "user",
		},
		{
			name: "rename at the provider keeps the same account",
			seed: []store.User{{
				ID: "u1", Username: "alice", PasswordHash: externalPlaceholderHash, Role: "operator",
				AuthSource: store.AuthSourceOIDC, ExternalID: "sub-1",
			}},
			authSource: store.AuthSourceOIDC,
			id:         externalIdentity{ExternalID: "sub-1", Username: "alice.smith", DisplayName: "Alice Smith"},
			role:       "operator",
			wantUser:   "alice", // username is not re-synced; the account is the same row
			wantRole:   "operator",
		},
		{
			name: "recycled username cannot inherit another identity",
			seed: []store.User{{
				ID: "u1", Username: "dana", PasswordHash: externalPlaceholderHash, Role: "admin",
				AuthSource: store.AuthSourceOIDC, ExternalID: "sub-departed",
			}},
			authSource: store.AuthSourceOIDC,
			id:         externalIdentity{ExternalID: "sub-newhire", Username: "dana", DisplayName: "Dana"},
			role:       "user",
			wantErr:    true,
		},
		{
			name: "local account is never adopted",
			seed: []store.User{{
				ID: "u1", Username: "admin", PasswordHash: "$argon2id$real", Role: "admin",
				AuthSource: store.AuthSourceLocal,
			}},
			authSource: store.AuthSourceOIDC,
			id:         externalIdentity{ExternalID: "sub-1", Username: "admin", DisplayName: "Attacker"},
			role:       "admin",
			wantErr:    true,
		},
		{
			name: "an identity from another provider is not adopted",
			seed: []store.User{{
				ID: "u1", Username: "alice", PasswordHash: externalPlaceholderHash, Role: "admin",
				AuthSource: store.AuthSourceLDAP, ExternalID: "sub-1",
			}},
			authSource: store.AuthSourceOIDC,
			id:         externalIdentity{ExternalID: "sub-1", Username: "alice", DisplayName: "Alice"},
			role:       "user",
			wantErr:    true,
		},
		{
			name:       "an identity with no stable identifier is refused",
			authSource: store.AuthSourceOIDC,
			id:         externalIdentity{Username: "alice", DisplayName: "Alice"},
			role:       "user",
			wantErr:    true,
		},
		{
			name: "provider owns role: resync on login",
			seed: []store.User{{
				ID: "u1", Username: "alice", PasswordHash: externalPlaceholderHash, Role: "read-only",
				AuthSource: store.AuthSourceOIDC, ExternalID: "sub-1",
			}},
			authSource: store.AuthSourceOIDC,
			id:         externalIdentity{ExternalID: "sub-1", Username: "alice"},
			role:       "admin",
			ownsRole:   true,
			wantUser:   "alice",
			wantRole:   "admin",
		},
		{
			name: "local owns role: no resync",
			seed: []store.User{{
				ID: "u1", Username: "alice", PasswordHash: externalPlaceholderHash, Role: "read-only",
				AuthSource: store.AuthSourceOIDC, ExternalID: "sub-1",
			}},
			authSource: store.AuthSourceOIDC,
			id:         externalIdentity{ExternalID: "sub-1", Username: "alice"},
			role:       "admin",
			ownsRole:   false,
			wantUser:   "alice",
			wantRole:   "read-only",
		},
		{
			name: "disabled account is refused",
			seed: []store.User{{
				ID: "u1", Username: "alice", PasswordHash: externalPlaceholderHash, Role: "user",
				AuthSource: store.AuthSourceOIDC, ExternalID: "sub-1", Disabled: true,
			}},
			authSource: store.AuthSourceOIDC,
			id:         externalIdentity{ExternalID: "sub-1", Username: "alice"},
			role:       "user",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := fake.New()
			for _, u := range tt.seed {
				if _, err := st.CreateUser(t.Context(), u); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			h := newTestAuthHandler(t, st)
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth/login", nil)

			got, err := h.resolveExternalUser(r, tt.authSource, tt.id, tt.role, tt.ownsRole)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got user %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.Username != tt.wantUser {
				t.Fatalf("Username = %q, want %q", got.Username, tt.wantUser)
			}
			if got.Role != tt.wantRole {
				t.Fatalf("Role = %q, want %q", got.Role, tt.wantRole)
			}
			if got.AuthSource != tt.authSource {
				t.Fatalf("AuthSource = %q, want %q", got.AuthSource, tt.authSource)
			}
			if got.ExternalID != tt.id.ExternalID {
				t.Fatalf("ExternalID = %q, want %q", got.ExternalID, tt.id.ExternalID)
			}
		})
	}
}
