// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// stopWait bounds how long stall waits for a process to stop. A thread takes
// a pending SIGSTOP as soon as it is scheduled or leaves the kernel, which is
// microseconds on an idle host and milliseconds on a loaded one; only a
// thread in uninterruptible or killable-only sleep (such as a parent in
// vfork, waiting for its child to exec) holds it up for longer.
const stopWait = 10 * time.Second

// awaitStop returns once the kernel reports the process stopped. kill(2) only
// queues SIGSTOP: each thread keeps running until it takes it, so a server
// just sent SIGSTOP can still accept and answer a request. The kernel
// reports a stop to the parent only once every thread has stopped. WNOWAIT
// leaves the report in place, and the goroutine reaping the process
// (exec.Cmd.Wait) asks only for exits, so neither disturbs the other.
func (p *proc) awaitStop() error {
	deadline := time.Now().Add(stopWait)
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, p.pid(), &info, unix.WSTOPPED|unix.WNOWAIT|unix.WNOHANG, nil)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ECHILD):
			return fmt.Errorf("target: %s (pid %d) exited before it stopped", p.name, p.pid())
		case err != nil:
			return fmt.Errorf("target: wait for %s (pid %d) to stop: %w", p.name, p.pid(), err)
		case info.Signo == int32(unix.SIGCHLD):
			// Only stops were asked for, so a report is the stop.
			return nil
		}
		if p.exited() {
			return fmt.Errorf("target: %s (pid %d) exited before it stopped", p.name, p.pid())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("target: %s (pid %d) had not stopped %s after SIGSTOP", p.name, p.pid(), stopWait)
		}
		time.Sleep(time.Millisecond)
	}
}
