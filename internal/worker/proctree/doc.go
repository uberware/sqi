// SPDX-License-Identifier: AGPL-3.0-or-later

// Package proctree contains a launched process and everything it spawns, so a
// worker can wait for, signal and stop the whole tree rather than only its
// direct child. A task's process and an OpenJD environment action's process
// are both launched this way.
//
// A [Tree] is the containment for one launched os/exec.Cmd. It is built in
// three steps: [Configure] before Start, then [Supervise] right after Start,
// then [Tree.Release] once the process has been dealt with.
//
// On POSIX the tree is the process group [Configure] makes the command the
// leader of; there is nothing to create at [Supervise] and nothing to undo at
// [Tree.Release]. On Windows [Configure] starts the process suspended,
// [Supervise] assigns it to a fresh job object and only then resumes it, so
// every process the tree creates starts inside the job, and the job (not the
// parent-PID chain, which an orphaned grandchild escapes) is what [Tree.Kill]
// terminates and [Tree.Alive] counts.
//
// [Tree.Notify] asks the tree to stop (SIGTERM, or a best-effort taskkill),
// [Tree.Kill] stops it for certain, and [Resolve] turns an OpenJD
// Action.cancelation method into the choice between the two.
//
// [Tree.Release] on a normal exit does not kill: a background process still
// running in the tree when its command exits successfully is left running on
// both platforms. On Windows the job's kill-on-close flag, which stops the
// tree if the worker process itself exits mid-run, is cleared first.
//
// On POSIX nothing outlives the worker to stop a tree when the worker itself
// is killed, so a launch site marks each tree's environment with a fresh token
// ([MarkEnv]) and records it in the worker's [Ledger] with [Tree.Track], which
// reads the token back from the command; [Tree.Release] deletes the record. The next worker to start on
// the same data directory runs [Ledger.Sweep] before it registers, killing
// every process that still carries a recorded token. Windows records nothing:
// its job objects already die with the worker.
//
// The package imports only the standard library, golang.org/x/sys/windows and
// internal/worker/protocol, so both internal/worker/executor and
// internal/worker/session can use it.
package proctree
