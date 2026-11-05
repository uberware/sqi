// SPDX-License-Identifier: AGPL-3.0-or-later

// Command noop is the task a load run's real workers execute: it ignores its
// arguments, prints one line and exits 0, so a real sqi-worker exercises the
// full task lifecycle at no cost of its own.
package main

import "fmt"

func main() {
	fmt.Println("sqi-load noop task")
}
