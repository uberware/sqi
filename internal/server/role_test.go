// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import "testing"

// A leader whose lease authority has lapsed must stop reporting itself as the
// leader at once, before the elector's loop has recorded the loss: a process
// that was frozen and has just resumed answers requests first.
func TestRoleState_LapsedAuthorityIsNotLeading(t *testing.T) {
	authorized := true
	r := newRoleState()
	r.setAuthority(func() bool { return authorized })
	r.promote("self", 3)

	if !r.isLeader() {
		t.Fatal("isLeader() with authority = false")
	}
	if got := r.info(); !got.IsLeader || got.Role != "leader" || got.Leader != "self" || got.Epoch != 3 {
		t.Fatalf("info() with authority = %+v, want the leader at epoch 3", got)
	}

	authorized = false
	if r.isLeader() {
		t.Error("isLeader() after authority lapsed = true")
	}
	got := r.info()
	if got.IsLeader || got.Role != "standby" {
		t.Errorf("info() after authority lapsed = %+v, want a standby that is not the leader", got)
	}
	if got.Leader != "" {
		t.Errorf("info().Leader after authority lapsed = %q, want none: the group's leader is unknown to a lapsed leader", got.Leader)
	}
	if name := r.leaderName(); name != "" {
		t.Errorf("leaderName() after authority lapsed = %q, want none", name)
	}
}

func TestRoleState_NoAuthoritySourceTrustsTheRole(t *testing.T) {
	r := newRoleState()
	r.promote("self", 1)
	if !r.isLeader() {
		t.Error("isLeader() with no authority source = false, want the recorded role")
	}
}

func TestRoleState_StandbyStaysStandbyWhenAuthorized(t *testing.T) {
	r := newRoleState()
	r.setAuthority(func() bool { return true })
	if r.isLeader() {
		t.Error("a standby reported leading because the authority source said yes")
	}
}
