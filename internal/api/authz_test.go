// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/policy"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// mount builds a tiny router that injects principal p, gates GET pattern on
// perm, and records whether the wrapped handler ran.
func mountAuthz(t *testing.T, st store.Store, p auth.Principal, perm policy.Permission, pattern string) (*httptest.Server, *bool) {
	t.Helper()
	ran := new(bool)
	az := newAuthz(st, newTestLogger())
	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(auth.NewContext(req.Context(), p)))
			})
		})
		g.With(az.require(perm)).Get(pattern, func(w http.ResponseWriter, _ *http.Request) {
			*ran = true
			w.WriteHeader(http.StatusOK)
		})
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, ran
}

func TestRequirePermission_DeniesAndAudits(t *testing.T) {
	st := fake.New()
	p := auth.Principal{Subject: "u1", Roles: []string{"read-only"}, Kind: auth.KindUser}
	// A parameterized pattern ("/x/{id}") requested with a concrete,
	// attacker-choosable value ("/x/zzz") lets this test tell the audited
	// EntityID (the route pattern) apart from the raw request path.
	srv, ran := mountAuthz(t, st, p, policy.JobsWrite, "/x/{id}")

	resp, err := getURL(t, srv.URL+"/x/zzz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if *ran {
		t.Fatal("wrapped handler ran despite 403")
	}
	entries, err := st.ListAuditEntries(t.Context(), "authz", "")
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != "denied" || entries[0].Actor != "u1" {
		t.Fatalf("audit = %+v, want one authz/denied entry for u1", entries)
	}
	entry := entries[0]
	if entry.EntityID != "/x/{id}" {
		t.Errorf("EntityID = %q, want the route pattern %q (not the raw path, which is attacker-controlled and unbounded)", entry.EntityID, "/x/{id}")
	}
	if got := entry.Details["path"]; got != "/x/zzz" {
		t.Errorf("Details[\"path\"] = %v, want %q (raw path must still be recoverable)", got, "/x/zzz")
	}
}

func TestRequirePermission_AllowsGranted(t *testing.T) {
	st := fake.New()
	p := auth.Principal{Subject: "u2", Roles: []string{"operator"}, Kind: auth.KindUser}
	srv, ran := mountAuthz(t, st, p, policy.JobsWrite, "/x")

	resp, err := getURL(t, srv.URL+"/x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !*ran {
		t.Fatalf("status = %d ran = %v, want 200 true", resp.StatusCode, *ran)
	}
}

func TestRequirePermission_SuperuserBypasses(t *testing.T) {
	st := fake.New()
	p := auth.Principal{Superuser: true, Kind: auth.KindAnonymous}
	srv, ran := mountAuthz(t, st, p, policy.UsersManage, "/x")

	resp, err := getURL(t, srv.URL+"/x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !*ran {
		t.Fatalf("status = %d ran = %v, want 200 true (auth-off)", resp.StatusCode, *ran)
	}
}

// getURL issues a GET request bound to the test's context, avoiding the
// noctx lint restriction on http.Get.
func getURL(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}
