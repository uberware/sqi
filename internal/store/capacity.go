// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
)

// CapacityGroupKind says how a capacity group's machines are known.
type CapacityGroupKind string

const (
	// CapacityGroupNamed is a group of specific, already-provisioned machines.
	CapacityGroupNamed CapacityGroupKind = "named"
	// CapacityGroupSized is a group of interchangeable machines, all matching
	// the group's [MachineProfile].
	CapacityGroupSized CapacityGroupKind = "sized"
)

// CapacityProvider names the mechanism that starts and stops a group's
// machines.
type CapacityProvider string

const (
	// CapacityProviderExternal leaves starting and stopping to something
	// outside sqi.
	CapacityProviderExternal CapacityProvider = "external"
	// CapacityProviderWakeOnLAN ("wake_on_lan") and "script:<name>" pass
	// [CapacityGroup.Validate], but no provider implements them and the API
	// accepts only [CapacityProviderExternal].
	CapacityProviderWakeOnLAN CapacityProvider = "wake_on_lan"
)

// Capacity group retire actions.
const (
	CapacityRetireSleep    = "sleep"
	CapacityRetireShutdown = "shutdown"
)

// Capacity group limits.
const (
	// MaxCapacityGroupMachines is the largest MaxMachines a group may declare.
	MaxCapacityGroupMachines = 10000
	// MinCapacityGroupTimeout is the shortest IdleTimeout and BootTimeout.
	MinCapacityGroupTimeout = time.Minute
)

// PowerState is where a worker is in the capacity machinery's power cycle. It
// is separate from the worker's liveness ([WorkerStatus]) and from an
// operator's disabled flag.
type PowerState string

const (
	// PowerAwake is a running worker, and the state of every worker that is
	// not managed by a capacity group.
	PowerAwake PowerState = "awake"
	// PowerWaking is a machine the server has asked to start and that has not
	// registered yet.
	PowerWaking PowerState = "waking"
	// PowerRetiring is a worker told to sleep or shut down; it takes no new
	// work while it finishes what it holds.
	PowerRetiring PowerState = "retiring"
	// PowerAsleep is a machine that has deregistered and is suspended or off.
	PowerAsleep PowerState = "asleep"
	// PowerWakeFailed is a machine that did not register within its group's
	// boot timeout.
	PowerWakeFailed PowerState = "wake_failed"
)

// NetInterface is one network interface of a worker's machine: what a
// wake-on-LAN packet is addressed with. The JSON tags are the wire form, and
// the form the interfaces column stores.
type NetInterface struct {
	Name  string   `json:"name"`
	MAC   string   `json:"mac"`
	Addrs []string `json:"addrs,omitempty"`
}

// MachineProfile describes the machine a group's machines are expected to be.
// It is stored as JSON; a zero value means the field is not declared.
type MachineProfile struct {
	CPUCount        int               `json:"cpu_count,omitempty"`
	RAMMb           int               `json:"ram_mb,omitempty"`
	GPUCount        int               `json:"gpu_count,omitempty"`
	GPUMemoryMb     int               `json:"gpu_memory_mb,omitempty"`
	OS              string            `json:"os,omitempty"`
	Arch            string            `json:"arch,omitempty"`
	ComputeLocation string            `json:"compute_location,omitempty"`
	Tags            map[string]string `json:"tags,omitempty"`
	Extensions      []string          `json:"extensions,omitempty"`
	ExprLimits      WorkerExprLimits  `json:"expr_limits,omitzero"`
}

// CapacityGroup is a set of machines the server may grow and shrink to follow
// demand, and the policy for doing so.
type CapacityGroup struct {
	ID     string
	FarmID string
	Name   string
	Kind   CapacityGroupKind
	// Provider starts and stops the group's machines.
	Provider CapacityProvider
	Enabled  bool
	// QueueIDs are the queues whose demand the group serves.
	QueueIDs []string
	// MinMachines is the number of machines kept up regardless of demand;
	// MaxMachines is the most the group may have up.
	MinMachines int
	MaxMachines int
	// Priority orders groups competing for the same demand; higher first.
	Priority int
	// MaxStepUp is the most machines one scale-up decision may add.
	MaxStepUp int
	// ScaleUpCooldown is the shortest time between scale-ups.
	ScaleUpCooldown time.Duration
	// IdleTimeout is how long a machine sits idle before it is retired;
	// BootTimeout is how long a started machine has to register.
	IdleTimeout time.Duration
	BootTimeout time.Duration
	// RetireAction is what an idle machine is told to do: "sleep" or
	// "shutdown".
	RetireAction string
	Profile      MachineProfile
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CapacityGroupStore is the persistence interface for [CapacityGroup] records.
type CapacityGroupStore interface {
	// CreateCapacityGroup inserts a new group. Returns [ErrConflict] if the
	// name is taken or the farm does not exist.
	CreateCapacityGroup(ctx context.Context, g CapacityGroup) (CapacityGroup, error)

	// GetCapacityGroup returns the group with the given ID, or [ErrNotFound].
	GetCapacityGroup(ctx context.Context, id string) (CapacityGroup, error)

	// GetCapacityGroupByName returns the group with the given name, or
	// [ErrNotFound].
	GetCapacityGroupByName(ctx context.Context, name string) (CapacityGroup, error)

	// ListCapacityGroups returns all groups ordered by priority descending,
	// then name.
	ListCapacityGroups(ctx context.Context) ([]CapacityGroup, error)

	// UpdateCapacityGroup replaces the mutable fields of an existing group and
	// updates UpdatedAt. The ID and farm do not change. Returns [ErrNotFound]
	// or [ErrConflict].
	UpdateCapacityGroup(ctx context.Context, g CapacityGroup) (CapacityGroup, error)

	// DeleteCapacityGroup removes a group by ID. Returns [ErrNotFound] if it
	// does not exist, or [ErrConflict] while any worker belongs to it.
	DeleteCapacityGroup(ctx context.Context, id string) error
}

// CapacityGroupState is what the server has decided about a group at run
// time, kept apart from the operator's [CapacityGroup] fields so that
// replacing those can never overwrite it.
type CapacityGroupState struct {
	GroupID string
	// Requested is how many machines the server last asked the group's
	// provider for, and RequestedAt when.
	Requested   int
	RequestedAt *time.Time
	// DegradedReason says why the group is degraded, empty when it is not;
	// DegradedAt is when it became so.
	DegradedReason string
	DegradedAt     *time.Time
	// BackoffUntil is the earliest time the server asks for more machines
	// again, or nil.
	BackoffUntil *time.Time
	UpdatedAt    time.Time
}

// CapacityPowerStore holds the capacity machinery's power transitions of group
// members and each group's [CapacityGroupState]. Every write is made by the
// leader and fenced, and each one that touches a worker locks that worker's
// row first, so a transition and a lease to the same worker are ordered.
//
// A transition that does not apply (the worker is unknown, or not in the state
// or condition it requires) is not an error: the methods report what moved.
type CapacityPowerStore interface {
	// MarkWaking moves each of workerIDs that is a member of groupID, offline,
	// not disabled and in [PowerAwake], [PowerAsleep] or [PowerWakeFailed] to
	// [PowerWaking] at now, with no reason, and returns the IDs it moved in
	// ascending order.
	MarkWaking(ctx context.Context, groupID string, workerIDs []string, now time.Time) ([]string, error)

	// FailWakes moves every member of groupID in [PowerWaking] whose
	// PowerStateAt is before cutoff to [PowerWakeFailed] at now with reason,
	// and returns the rows moved, by ID.
	FailWakes(ctx context.Context, groupID string, cutoff, now time.Time, reason string) ([]Worker, error)

	// BeginRetirement moves workerID from [PowerAwake] to [PowerRetiring] at
	// now, with no reason, only while it is a member of groupID, online, not
	// disabled and holds no assigned or running task (the predicate
	// [WorkerStore.DeleteEphemeralWorker] uses). ok is false, with no error,
	// when any condition fails. It is decided under the worker's row lock, so
	// a lease to the worker either committed first and is seen, or waits and
	// is refused ([TaskStore.LeaseTask] refuses a retiring worker).
	BeginRetirement(ctx context.Context, groupID, workerID string, now time.Time) (w Worker, ok bool, err error)

	// FinishRetirement completes the retirement of workerID, which must be in
	// [PowerRetiring]. A named member becomes [PowerAsleep] and offline at now,
	// with no reason, and is returned. An ephemeral member is deleted and its
	// active credential revoked at now, as [WorkerStore.DeleteEphemeralWorker]
	// does, and returned as it was last stored (deleted = true). ok is false,
	// with no error, when the worker is unknown or not retiring. A retiring
	// member holding an assigned or running task, which BeginRetirement and
	// the lease guard leave no way to reach, is refused with [ErrConflict] and
	// left retiring, rather than put to sleep or deleted under its task.
	FinishRetirement(ctx context.Context, workerID string, now time.Time) (w Worker, deleted, ok bool, err error)

	// AbortRetirement moves workerID from [PowerRetiring] back to [PowerAwake]
	// at now with reason; ok is false, with no error, when it is not retiring.
	AbortRetirement(ctx context.Context, workerID string, now time.Time, reason string) (bool, error)

	// ExpireRetirements aborts, as AbortRetirement does, every member of
	// groupID in [PowerRetiring] whose PowerStateAt is before cutoff, and
	// returns their IDs in ascending order.
	ExpireRetirements(ctx context.Context, groupID string, cutoff, now time.Time, reason string) ([]string, error)

	// ListCapacityGroupStates returns every recorded state by group ID; a
	// group with none is absent.
	ListCapacityGroupStates(ctx context.Context) (map[string]CapacityGroupState, error)

	// PutCapacityGroupState stores s as its group's state, replacing any
	// earlier one whole. A zero UpdatedAt is stamped with the current time.
	// It returns [ErrNotFound] when the group does not exist. A group's state
	// is deleted with the group.
	PutCapacityGroupState(ctx context.Context, s CapacityGroupState) error
}

// ProfileWorker returns a synthetic worker standing for one machine of g: the
// group's farm and the hardware, platform, tags, extensions and EXPR limits its
// profile declares. The worker owns copies of the profile's tags and
// extensions.
//
// QueueID is the group's sole queue when it serves exactly one, and empty
// otherwise, which is what a real worker registered against several queues
// carries (no single queue affinity).
func (g CapacityGroup) ProfileWorker() Worker {
	p := g.Profile
	w := Worker{
		FarmID:          g.FarmID,
		ComputeLocation: p.ComputeLocation,
		OS:              p.OS,
		Arch:            p.Arch,
		CPUCount:        p.CPUCount,
		RAMMb:           p.RAMMb,
		GPUInfo:         GPUInfo{Count: p.GPUCount, VRAMMb: p.GPUMemoryMb},
		Tags:            maps.Clone(p.Tags),
		Extensions:      slices.Clone(p.Extensions),
		ExprLimits:      p.ExprLimits,
	}
	if len(g.QueueIDs) == 1 {
		w.QueueID = g.QueueIDs[0]
	}
	return w
}

var capacityGroupNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Validate reports every rule the group breaks, joined, or nil. The API calls
// it before writing a group; the stores do not.
func (g CapacityGroup) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !capacityGroupNameRE.MatchString(g.Name) {
		bad("name %q must be 1-63 characters: letters, digits, '.', '_' or '-', starting with a letter or digit", g.Name)
	}
	switch g.Kind {
	case CapacityGroupNamed, CapacityGroupSized:
	default:
		bad("kind %q must be %q or %q", g.Kind, CapacityGroupNamed, CapacityGroupSized)
	}
	if !validCapacityProvider(g.Provider) {
		bad("provider %q must be %q, %q or \"script:<name>\"", g.Provider, CapacityProviderExternal, CapacityProviderWakeOnLAN)
	}
	errs = append(errs, g.validateScaling()...)
	if g.Kind == CapacityGroupSized && g.Profile.CPUCount < 1 {
		bad("a sized group's profile must declare cpu_count of at least 1")
	}
	switch g.RetireAction {
	case CapacityRetireSleep, CapacityRetireShutdown:
	default:
		bad("retire_action %q must be %q or %q", g.RetireAction, CapacityRetireSleep, CapacityRetireShutdown)
	}
	return errors.Join(errs...)
}

// validateScaling checks the machine counts and the timing rules.
func (g CapacityGroup) validateScaling() []error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch {
	case g.MinMachines < 0:
		bad("min_machines %d must be at least 0", g.MinMachines)
	case g.MaxMachines < 1:
		bad("max_machines %d must be at least 1", g.MaxMachines)
	case g.MaxMachines > MaxCapacityGroupMachines:
		bad("max_machines %d must be at most %d", g.MaxMachines, MaxCapacityGroupMachines)
	case g.MinMachines > g.MaxMachines:
		bad("min_machines %d must not exceed max_machines %d", g.MinMachines, g.MaxMachines)
	}
	switch {
	case g.MaxStepUp < 1:
		bad("max_step_up %d must be at least 1", g.MaxStepUp)
	case g.MaxStepUp > MaxCapacityGroupMachines:
		bad("max_step_up %d must be at most %d", g.MaxStepUp, MaxCapacityGroupMachines)
	}
	if g.IdleTimeout < MinCapacityGroupTimeout {
		bad("idle_timeout %s must be at least %s", g.IdleTimeout, MinCapacityGroupTimeout)
	}
	if g.BootTimeout < MinCapacityGroupTimeout {
		bad("boot_timeout %s must be at least %s", g.BootTimeout, MinCapacityGroupTimeout)
	}
	if g.ScaleUpCooldown < 0 {
		bad("scale_up_cooldown %s must not be negative", g.ScaleUpCooldown)
	}
	return errs
}

func validCapacityProvider(p CapacityProvider) bool {
	if p == CapacityProviderExternal || p == CapacityProviderWakeOnLAN {
		return true
	}
	name, ok := strings.CutPrefix(string(p), "script:")
	return ok && name != ""
}
