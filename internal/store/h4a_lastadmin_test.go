// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

func TestLastAdminGuard(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			a, err := st.CreateUser(ctx, mkUser("admin"))
			if err != nil {
				t.Fatal(err)
			}
			b, err := st.CreateUser(ctx, mkUser("admin"))
			if err != nil {
				t.Fatal(err)
			}
			if err := st.DeleteUser(ctx, b.ID); err != nil {
				t.Fatalf("deleting one of two admins: %v", err)
			}
			if err := st.DeleteUser(ctx, a.ID); !errors.Is(err, store.ErrLastAdmin) {
				t.Fatalf("deleting the last admin = %v, want ErrLastAdmin", err)
			}
			if _, err := st.GetUser(ctx, a.ID); err != nil {
				t.Fatalf("the refused delete removed the last admin: %v", err)
			}
			demoted := a
			demoted.Role = "viewer"
			if _, err := st.UpdateUserKeepingAdmin(ctx, demoted); !errors.Is(err, store.ErrLastAdmin) {
				t.Fatalf("demoting the last admin = %v, want ErrLastAdmin", err)
			}
			disabled := a
			disabled.Disabled = true
			if _, err := st.UpdateUserKeepingAdmin(ctx, disabled); !errors.Is(err, store.ErrLastAdmin) {
				t.Fatalf("disabling the last admin = %v, want ErrLastAdmin", err)
			}
			got, err := st.GetUser(ctx, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Role != "admin" || got.Disabled {
				t.Fatalf("a refused update wrote anyway: role=%q disabled=%v", got.Role, got.Disabled)
			}
			renamed := a
			renamed.DisplayName = "still admin"
			if _, err := st.UpdateUserKeepingAdmin(ctx, renamed); err != nil {
				t.Fatalf("a harmless edit of the last admin = %v, want nil", err)
			}
			// Directory sync keeps its authority.
			if _, err := st.UpdateUser(ctx, demoted); err != nil {
				t.Fatalf("plain UpdateUser demoting the last admin = %v, want nil (directory sync)", err)
			}
		})
	}
}

// TestLastAdminGuard_Edges pins the guard's boundaries: only an ENABLED admin
// counts, only a write that removes the sole one is refused, and a missing
// user is NotFound rather than LastAdmin.
func TestLastAdminGuard_Edges(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			live, err := st.CreateUser(ctx, mkUser("admin"))
			if err != nil {
				t.Fatal(err)
			}
			dormant := mkUser("admin")
			dormant.Disabled = true
			dormant, err = st.CreateUser(ctx, dormant)
			if err != nil {
				t.Fatal(err)
			}
			viewer, err := st.CreateUser(ctx, mkUser("viewer"))
			if err != nil {
				t.Fatal(err)
			}

			// A disabled admin does not count: the one enabled admin is still the last.
			demoted := live
			demoted.Role = "viewer"
			if _, err := st.UpdateUserKeepingAdmin(ctx, demoted); !errors.Is(err, store.ErrLastAdmin) {
				t.Fatalf("demoting the only ENABLED admin = %v, want ErrLastAdmin", err)
			}
			if err := st.DeleteUser(ctx, live.ID); !errors.Is(err, store.ErrLastAdmin) {
				t.Fatalf("deleting the only ENABLED admin = %v, want ErrLastAdmin", err)
			}

			// A disabled admin is not a live admin, so removing or editing it is free.
			dormant.Role = "viewer"
			if _, err := st.UpdateUserKeepingAdmin(ctx, dormant); err != nil {
				t.Fatalf("demoting a disabled admin = %v, want nil", err)
			}
			if err := st.DeleteUser(ctx, dormant.ID); err != nil {
				t.Fatalf("deleting a disabled admin = %v, want nil", err)
			}

			// Non-admins are never guarded, and promoting one is allowed.
			viewer.Role = "admin"
			if _, err := st.UpdateUserKeepingAdmin(ctx, viewer); err != nil {
				t.Fatalf("promoting a viewer = %v, want nil", err)
			}
			// Two live admins again: either may now go.
			demoted.Disabled = true
			if _, err := st.UpdateUserKeepingAdmin(ctx, demoted); err != nil {
				t.Fatalf("disabling one of two admins = %v, want nil", err)
			}
			if err := st.DeleteUser(ctx, viewer.ID); !errors.Is(err, store.ErrLastAdmin) {
				t.Fatalf("deleting the admin that remains = %v, want ErrLastAdmin", err)
			}

			// Unknown ids are NotFound, never LastAdmin.
			ghost := mkUser("admin")
			if _, err := st.UpdateUserKeepingAdmin(ctx, ghost); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("UpdateUserKeepingAdmin of an unknown id = %v, want ErrNotFound", err)
			}
			if err := st.DeleteUser(ctx, ghost.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("DeleteUser of an unknown id = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestLastAdminGuard_Concurrent removes every admin at once, half by deletion
// and half by demotion. However the writes interleave, exactly one admin may
// survive: a guard checked before the write would let several pass.
func TestLastAdminGuard_Concurrent(t *testing.T) {
	const admins = 8
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			created := make([]store.User, admins)
			for i := range created {
				u, err := st.CreateUser(ctx, mkUser("admin"))
				if err != nil {
					t.Fatal(err)
				}
				created[i] = u
			}

			start := make(chan struct{})
			errs := make(chan error, admins)
			var wg sync.WaitGroup
			for i, u := range created {
				wg.Go(func() {
					<-start
					if i%2 == 0 {
						errs <- st.DeleteUser(ctx, u.ID)
						return
					}
					u.Role = "viewer"
					_, err := st.UpdateUserKeepingAdmin(ctx, u)
					errs <- err
				})
			}
			close(start)
			wg.Wait()
			close(errs)

			var refused, removed int
			for err := range errs {
				switch {
				case err == nil:
					removed++
				case errors.Is(err, store.ErrLastAdmin):
					refused++
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}
			if refused != 1 || removed != admins-1 {
				t.Fatalf("removed %d, refused %d; want %d removed and exactly 1 refused", removed, refused, admins-1)
			}
			if n, err := st.CountAdmins(ctx); err != nil || n != 1 {
				t.Fatalf("enabled admins = %d, %v; want 1, nil", n, err)
			}
		})
	}
}
