// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

// lastAdminRaceStore lets a second request win the race at the decision point
// of the request under test: right before the guarded write (DeleteUser,
// UpdateUserKeepingAdmin). hook runs once, and only once armed, so the auth
// middleware's own store calls during login cannot trigger it early.
type lastAdminRaceStore struct {
	store.Store

	hook  func()
	armed atomic.Bool
	once  sync.Once
}

func (s *lastAdminRaceStore) fire() {
	if !s.armed.Load() {
		return
	}
	s.once.Do(s.hook)
}

func (s *lastAdminRaceStore) DeleteUser(ctx context.Context, id string) error {
	s.fire()
	return s.Store.DeleteUser(ctx, id)
}

func (s *lastAdminRaceStore) UpdateUserKeepingAdmin(ctx context.Context, u store.User) (store.User, error) {
	s.fire()
	return s.Store.UpdateUserKeepingAdmin(ctx, u)
}

// TestConcurrentRemovalsKeepOneAdmin pins that two requests that each remove
// one of the last two admins cannot both pass the count check and lock the
// farm out of administration. The request under test (root acting on
// root2) is overtaken at its decision point by the other request (acting on
// root); the guard must then see a single admin left and refuse with 409.
func TestConcurrentRemovalsKeepOneAdmin(t *testing.T) {
	cases := []struct {
		name string
		// request acts on the second admin; other is the overtaking request,
		// run against the store directly, acting on the first.
		request func(t *testing.T, srvURL string, cookie *http.Cookie, second store.User) *http.Response
		other   func(t *testing.T, st store.Store, first store.User)
	}{
		{
			name: "delete",
			request: func(t *testing.T, srvURL string, cookie *http.Cookie, second store.User) *http.Response {
				return doRequest(t, http.MethodDelete, srvURL+"/api/v1/users/"+second.ID, nil, cookie)
			},
			other: func(t *testing.T, st store.Store, first store.User) {
				if err := st.DeleteUser(t.Context(), first.ID); err != nil {
					t.Errorf("overtaking DeleteUser: %v", err)
				}
			},
		},
		{
			name: "demote",
			request: func(t *testing.T, srvURL string, cookie *http.Cookie, second store.User) *http.Response {
				return doRequest(t, http.MethodPatch, srvURL+"/api/v1/users/"+second.ID,
					map[string]any{"role": "operator"}, cookie)
			},
			other: func(t *testing.T, st store.Store, first store.User) {
				first.Role = "operator"
				if _, err := st.UpdateUserKeepingAdmin(t.Context(), first); err != nil {
					t.Errorf("overtaking demotion: %v", err)
				}
			},
		},
		{
			name: "disable",
			request: func(t *testing.T, srvURL string, cookie *http.Cookie, second store.User) *http.Response {
				return doRequest(t, http.MethodPatch, srvURL+"/api/v1/users/"+second.ID,
					map[string]any{"disabled": true}, cookie)
			},
			other: func(t *testing.T, st store.Store, first store.User) {
				first.Disabled = true
				if _, err := st.UpdateUserKeepingAdmin(t.Context(), first); err != nil {
					t.Errorf("overtaking disable: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// seedBackends is called per case: each case needs a farm with exactly
			// two admins, and an earlier case leaves one behind.
			for backend, st := range seedBackends(t) {
				t.Run(backend, func(t *testing.T) {
					first := seedAuthUser(t, st, "root", "hunter2!", "admin")
					second := seedAuthUser(t, st, "root2", "hunter2!", "admin")
					wrapped := &lastAdminRaceStore{Store: st, hook: func() { tc.other(t, st, first) }}
					srv := newAuthTestServer(t, wrapped)
					cookie := loginCookie(t, srv, "root", "hunter2!")
					wrapped.armed.Store(true)

					resp := tc.request(t, srv.URL, cookie, second)
					defer resp.Body.Close()
					if resp.StatusCode != http.StatusConflict {
						t.Fatalf("%s of the now-last admin = %d, want 409", tc.name, resp.StatusCode)
					}
					if detail := decodeProblem(t, resp); detail != "cannot remove the last admin" {
						t.Fatalf("detail = %q, want %q", detail, "cannot remove the last admin")
					}
					assertLiveAdmins(t, st, 1)
				})
			}
		})
	}
}

// assertLiveAdmins fails unless exactly want enabled admins exist.
func assertLiveAdmins(t *testing.T, st store.Store, want int) {
	t.Helper()
	users, err := st.ListUsers(t.Context())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	n := 0
	for _, u := range users {
		if u.Role == "admin" && !u.Disabled {
			n++
		}
	}
	if n != want {
		t.Fatalf("enabled admins = %d, want %d (last-admin lockout)", n, want)
	}
}
