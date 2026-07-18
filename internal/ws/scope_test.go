// SPDX-License-Identifier: AGPL-3.0-or-later

package ws

import (
	"log/slog"
	"testing"
)

func TestScopeAllows(t *testing.T) {
	tests := []struct {
		name        string
		scope       Scope
		ownerScoped bool
		owner       string
		want        bool
	}{
		{
			name:  "unscoped client receives everything",
			scope: Scope{All: true}, ownerScoped: true, owner: "bob", want: true,
		},
		{
			name:  "scoped client receives its own job",
			scope: Scope{Owner: "alice"}, ownerScoped: true, owner: "alice", want: true,
		},
		{
			name:  "owner match is case-insensitive",
			scope: Scope{Owner: "Alice"}, ownerScoped: true, owner: "alice", want: true,
		},
		{
			name:  "scoped client is denied another owner's job",
			scope: Scope{Owner: "alice"}, ownerScoped: true, owner: "bob", want: false,
		},
		{
			name:  "scoped client is denied an unresolved owner (fail closed)",
			scope: Scope{Owner: "alice"}, ownerScoped: true, owner: "", want: false,
		},
		{
			name:  "non-owner-scoped envelopes pass to everyone",
			scope: Scope{Owner: "alice"}, ownerScoped: false, owner: "", want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := Envelope{ownerScoped: tt.ownerScoped, owner: tt.owner}
			if got := tt.scope.allows(env); got != tt.want {
				t.Errorf("allows() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNotifyJobFiltersByOwner(t *testing.T) {
	h := NewHub(slog.New(slog.DiscardHandler), func(string) string { return "" })

	aliceCh := h.Register("alice-client", Scope{Owner: "alice"})
	allCh := h.Register("op-client", Scope{All: true})
	if err := h.Subscribe("alice-client", SubjectJobs, ^uint64(0)); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := h.Subscribe("op-client", SubjectJobs, ^uint64(0)); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	h.NotifyJob(JobEvent{JobID: "j1", Name: "bobs job", Owner: "bob", Status: "running"})

	if got := drain(aliceCh); len(got) != 0 {
		t.Errorf("scoped client received %d envelopes for another owner's job, want 0", len(got))
	}
	if got := drain(allCh); len(got) != 1 {
		t.Errorf("unscoped client received %d envelopes, want 1", len(got))
	}
}

// Replay must apply the same filter as live fan-out.
func TestSubscribeReplayFiltersByOwner(t *testing.T) {
	h := NewHub(slog.New(slog.DiscardHandler), func(string) string { return "" })

	// Emit first, with nobody subscribed, so the events land only in the ring.
	h.NotifyJob(JobEvent{JobID: "j1", Owner: "bob", Status: "running"})
	h.NotifyJob(JobEvent{JobID: "j2", Owner: "alice", Status: "running"})

	ch := h.Register("alice-client", Scope{Owner: "alice"})
	if err := h.Subscribe("alice-client", SubjectJobs, 0); err != nil { // 0 = replay all
		t.Fatalf("Subscribe: %v", err)
	}

	got := drain(ch)
	if len(got) != 1 {
		t.Fatalf("replayed %d envelopes, want 1 (only alice's)", len(got))
	}
}

func TestNotifyTaskResolvesOwnerThroughLookup(t *testing.T) {
	h := NewHub(slog.New(slog.DiscardHandler), func(jobID string) string {
		if jobID == "j1" {
			return "bob"
		}
		return ""
	})

	ch := h.Register("alice-client", Scope{Owner: "alice"})
	if err := h.Subscribe("alice-client", SubjectJobs, ^uint64(0)); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	h.NotifyTask(TaskEvent{JobID: "j1", TaskID: "t1", Status: "running"})

	if got := drain(ch); len(got) != 0 {
		t.Errorf("scoped client received %d task envelopes for bob's job, want 0", len(got))
	}
}

// With no resolver the hub cannot know ownership, so scoped clients get
// nothing rather than everything.
func TestNotifyTaskWithoutResolverFailsClosed(t *testing.T) {
	h := NewHub(slog.New(slog.DiscardHandler), nil)

	scopedCh := h.Register("alice-client", Scope{Owner: "alice"})
	allCh := h.Register("op-client", Scope{All: true})
	for _, id := range []string{"alice-client", "op-client"} {
		if err := h.Subscribe(id, SubjectJobs, ^uint64(0)); err != nil {
			t.Fatalf("Subscribe(%s): %v", id, err)
		}
	}

	h.NotifyTask(TaskEvent{JobID: "j1", TaskID: "t1", Status: "running"})

	if got := drain(scopedCh); len(got) != 0 {
		t.Errorf("scoped client received %d envelopes with no resolver, want 0", len(got))
	}
	if got := drain(allCh); len(got) != 1 {
		t.Errorf("unscoped client received %d envelopes, want 1", len(got))
	}
}

// drain returns everything currently buffered in ch without blocking.
func drain(ch chan Envelope) []Envelope {
	var out []Envelope
	for {
		select {
		case env := <-ch:
			out = append(out, env)
		default:
			return out
		}
	}
}
