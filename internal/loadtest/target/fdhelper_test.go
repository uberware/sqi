// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
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

// helperLimitFileEnv names the file the "limitrec" helper records its
// open-file limit in; the helper writes its pid to <file>.ready once it handles
// signals.
const helperLimitFileEnv = "LOADTARGET_TEST_LIMITFILE"

// helperLimitRec is a fake server that, on an interrupt or a terminate, writes
// the soft and hard open-file limit it has at that moment ("<soft> <hard>"),
// and exits. It shows what limit a process had when the harness stopped it.
func helperLimitRec() int {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	path := os.Getenv(helperLimitFileEnv)
	if err := os.WriteFile(path+".ready", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return 3
	}
	select {
	case <-sigs:
	case <-time.After(helperLifetime):
		return 0
	}
	raw, err := os.ReadFile("/proc/self/limits")
	if err != nil {
		return 3
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if f := strings.Fields(line); strings.HasPrefix(line, "Max open files") && len(f) >= 5 {
			if err := os.WriteFile(path, []byte(f[3]+" "+f[4]), 0o600); err != nil {
				return 3
			}
			return 0
		}
	}
	return 3
}
