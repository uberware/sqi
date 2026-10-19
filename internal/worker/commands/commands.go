// SPDX-License-Identifier: AGPL-3.0-or-later

// Package commands lets a worker's operator say where a command a job runs
// really lives.
//
// An OpenJD template names the program it runs ("nuke", "blender"). Nothing
// guarantees that name exists on a host: Nuke installs as Nuke17.1.exe, and a
// shell alias is invisible because the worker starts commands directly, with
// no shell. The operator's worker.commands table closes that gap. It maps a
// command name to the executable to run in its place, with optional leading
// arguments (for example Nuke's --nc flag for a non-commercial license).
//
// The table is operator configuration, never job input, so a job cannot use it
// to reach anything it could not already name. The package is stdlib-only: it
// is imported by the config loader, the session manager and the executor.
package commands

import (
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Command is what a command name resolves to.
type Command struct {
	// Path is the executable to run: an absolute path, or a bare name the
	// worker's own PATH resolves when the process starts.
	Path string `yaml:"path"`
	// Args go before the arguments the job supplies. They are literal: no
	// format-string expansion.
	Args []string `yaml:"args"`
}

// Table maps a command name, as a job writes it, to what to run instead.
type Table map[string]Command

// Problem is one reason a Table entry is unusable.
type Problem struct {
	// Key is the table key the problem is about.
	Key     string
	Message string
}

// Validate reports every unusable entry, ordered by key so the result is
// stable.
func (t Table) Validate() []Problem {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	var problems []Problem
	for _, k := range keys {
		for _, msg := range validateEntry(k, t[k]) {
			problems = append(problems, Problem{Key: k, Message: msg})
		}
	}
	return problems
}

func validateEntry(name string, c Command) []string {
	var msgs []string
	switch {
	case name == "":
		msgs = append(msgs, "the command name is empty")
	case strings.TrimSpace(name) != name:
		msgs = append(msgs, "the command name has leading or trailing whitespace")
	case hasSeparator(name):
		msgs = append(msgs, "the command name contains a path separator; "+
			"a job that names a path is naming a file, so only a bare name can be mapped")
	}
	switch {
	case c.Path == "":
		msgs = append(msgs, "path is required")
	case hasSeparator(c.Path) && !filepath.IsAbs(c.Path):
		msgs = append(msgs, fmt.Sprintf("path %q is relative; use an absolute path, or a bare name resolved through PATH", c.Path))
	}
	return msgs
}

// hasSeparator reports whether s contains a path separator of this host.
func hasSeparator(s string) bool {
	return strings.ContainsAny(s, "/"+string(filepath.Separator))
}

// Resolver applies a Table. The zero value and nil resolve nothing.
type Resolver struct {
	table Table
	// fold makes names case-insensitive, as Windows treats them.
	fold bool
}

// NewResolver returns a Resolver for t on this host. t is not copied; the
// caller must not change it afterwards.
func NewResolver(t Table) *Resolver { return newResolver(t, runtime.GOOS) }

func newResolver(t Table, goos string) *Resolver {
	return &Resolver{table: t, fold: goos == "windows"}
}

// lookup finds the entry for a command name a job wrote.
func (r *Resolver) lookup(name string) (Command, bool) {
	if r == nil || len(r.table) == 0 || hasSeparator(name) {
		return Command{}, false
	}
	if c, ok := r.table[name]; ok {
		return c, true
	}
	if r.fold {
		for k, c := range r.table {
			if strings.EqualFold(k, name) {
				return c, true
			}
		}
	}
	return Command{}, false
}

// Resolve returns what to run for command and the arguments to run it with. A
// name with no entry is returned unchanged. A mapped name runs the entry's
// Path, with the entry's Args before args. The returned slice is always the
// caller's to keep: it never aliases args or the table.
func (r *Resolver) Resolve(command string, args []string) (resolved string, resolvedArgs []string) {
	c, ok := r.lookup(command)
	if !ok {
		return command, args
	}
	out := make([]string, 0, len(c.Args)+len(args))
	out = append(out, c.Args...)
	out = append(out, args...)
	return c.Path, out
}
