// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"
)

// helperFDFileEnv names the file the "fdopen" helper opens, once per "open"
// command.
const helperFDFileEnv = "LOADTARGET_TEST_FDFILE"

// helperFDOpen is a child that holds files open on command. It reads one
// command per line from stdin: "open" opens helperFDFileEnv and keeps it, and
// "closeall" closes everything it holds. It answers each with "ok" or
// "err <message>" on stdout, and exits when stdin closes.
func helperFDOpen() int {
	var held []*os.File
	path := os.Getenv(helperFDFileEnv)
	go func() {
		time.Sleep(helperLifetime)
		os.Exit(0)
	}()
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		switch strings.TrimSpace(in.Text()) {
		case "open":
			f, err := os.Open(path)
			if err != nil {
				fmt.Println("err", err)
				continue
			}
			held = append(held, f)
		case "closeall":
			for _, f := range held {
				f.Close()
			}
			held = nil
		}
		fmt.Println("ok")
	}
	return 0
}
