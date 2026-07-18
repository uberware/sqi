// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

func TestBindSubmitIdentity(t *testing.T) {
	tests := []struct {
		name          string
		principal     auth.Principal
		clientOwner   string
		clientSub     string
		wantOwner     string
		wantSubmitter string
		wantStatus    int
	}{
		{
			name:          "submitter is always the principal, client value discarded",
			principal:     auth.Principal{Username: "alice", Roles: []string{"user"}},
			clientSub:     "someone-else",
			wantOwner:     "alice",
			wantSubmitter: "alice",
		},
		{
			name:          "owner defaults to the submitter",
			principal:     auth.Principal{Username: "alice", Roles: []string{"user"}},
			wantOwner:     "alice",
			wantSubmitter: "alice",
		},
		{
			name:          "owner equal to self is accepted",
			principal:     auth.Principal{Username: "alice", Roles: []string{"user"}},
			clientOwner:   "alice",
			wantOwner:     "alice",
			wantSubmitter: "alice",
		},
		{
			name:          "owner equal to self is accepted case-insensitively",
			principal:     auth.Principal{Username: "Alice", Roles: []string{"user"}},
			clientOwner:   "alice",
			wantOwner:     "Alice",
			wantSubmitter: "Alice",
		},
		{
			name:        "owner other than self without jobs.submit_as is refused",
			principal:   auth.Principal{Username: "alice", Roles: []string{"user"}},
			clientOwner: "bob",
			wantStatus:  http.StatusForbidden,
		},
		{
			name:          "operator may submit on behalf of another user",
			principal:     auth.Principal{Username: "proxy", Roles: []string{"operator"}},
			clientOwner:   "bob",
			wantOwner:     "bob",
			wantSubmitter: "proxy",
		},
		{
			name:          "auth off keeps client values verbatim",
			principal:     auth.Principal{Superuser: true, Kind: auth.KindAnonymous},
			clientOwner:   "alice",
			clientSub:     "pipeline",
			wantOwner:     "alice",
			wantSubmitter: "pipeline",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := auth.NewContext(context.Background(), tt.principal)
			owner, submitter, problem, status := bindSubmitIdentity(ctx, tt.clientOwner, tt.clientSub)

			if status != tt.wantStatus {
				t.Fatalf("status = %d (%q), want %d", status, problem, tt.wantStatus)
			}
			if tt.wantStatus != 0 {
				return
			}
			if owner != tt.wantOwner {
				t.Errorf("owner = %q, want %q", owner, tt.wantOwner)
			}
			if submitter != tt.wantSubmitter {
				t.Errorf("submitter = %q, want %q", submitter, tt.wantSubmitter)
			}
		})
	}
}

// Auth-on submit must persist the principal as submitter regardless of what
// the client put in the query string, and must refuse an owner override from
// a principal that lacks jobs.submit_as.
func TestSubmitJobBindsSubmitterFromPrincipal(t *testing.T) {
	st := fake.New()
	ctx := t.Context()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "farm-one"}); err != nil {
		t.Fatalf("create farm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "render"}); err != nil {
		t.Fatalf("create queue: %v", err)
	}
	r := newJobRouter(st, &fakeScheduler{})

	body := strings.NewReader(minimalOpenJDJSON("BindSubmitterTest"))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/jobs?farm_id=farm-1&queue_id=queue-1&owner=bob&submitter=bob", body)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.NewContext(req.Context(), auth.Principal{
		Username: "alice", Roles: []string{"user"},
	}))
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (owner=bob without jobs.submit_as) — body: %s", rec.Code, rec.Body)
	}
}

// The owner/submitter split exists so a privileged proxy (e.g. a pipeline
// gateway) can submit on behalf of an artist: the job belongs to the artist
// (Owner) while the proxy's own identity is recorded as Submitter.
func TestSubmitJobOperatorSubmitsOnBehalfOf(t *testing.T) {
	st := fake.New()
	ctx := t.Context()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "farm-one"}); err != nil {
		t.Fatalf("create farm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "render"}); err != nil {
		t.Fatalf("create queue: %v", err)
	}
	r := newJobRouter(st, &fakeScheduler{})

	body := strings.NewReader(minimalOpenJDJSON("OperatorOnBehalfTest"))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/jobs?farm_id=farm-1&queue_id=queue-1&owner=bob", body)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.NewContext(req.Context(), auth.Principal{
		Username: "proxy", Roles: []string{"operator"},
	}))
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — body: %s", rec.Code, rec.Body)
	}
	var resp jobResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	created, err := st.GetJob(ctx, resp.ID)
	if err != nil {
		t.Fatalf("GetJob(%q): %v", resp.ID, err)
	}
	if created.Owner != "bob" {
		t.Errorf("persisted job Owner = %q, want %q", created.Owner, "bob")
	}
	if created.Submitter != "proxy" {
		t.Errorf("persisted job Submitter = %q, want %q", created.Submitter, "proxy")
	}
}
