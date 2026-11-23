// SPDX-License-Identifier: AGPL-3.0-or-later

// Package configinit implements "config init" for sqi-server and sqi-worker:
// it renders a starter configuration file and writes it with the owner, mode
// or ACL the platform needs. Rendering ([Render], [DefaultTarget], [DataDir],
// [SessionDir]) is pure: it builds text and paths for the platform it is given
// and touches no file.
package configinit

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// Binary names the program a configuration file is for.
type Binary string

const (
	// Server is sqi-server.
	Server Binary = "sqi-server"
	// Worker is sqi-worker.
	Worker Binary = "sqi-worker"
)

const (
	goosWindows = "windows"
	goosDarwin  = "darwin"
)

// configDocs is the guide that explains each binary's configuration.
var configDocs = map[Binary]string{
	Server: "docs/configuration.md",
	Worker: "docs/worker-configuration.md",
}

// Platform is what defaults depend on. Tests build it for any OS.
type Platform struct {
	GOOS        string // "linux", "darwin", "windows"
	Home        string // user home (macOS defaults)
	ProgramData string // %ProgramData% (Windows defaults)
}

// Values are the settings a starter configuration carries.
type Values struct {
	Version string // version of the writing binary
	NATSURL string // worker only; "" leaves discovery on
	DataDir string // from DataDir
	// SessionDir is the worker's worker.session_dir, written only when it is
	// not "" (config init --user gives one: see SessionDir).
	SessionDir string
}

// DefaultTarget returns the path "config init" writes b's configuration to
// when no path is given.
func DefaultTarget(b Binary, p Platform) string {
	name := string(b) + ".yaml"
	switch p.GOOS {
	case goosWindows:
		return winJoin(p.ProgramData, "sqi", name)
	case goosDarwin:
		return path.Join(p.Home, ".sqi", name)
	default:
		return path.Join("/etc/sqi", name)
	}
}

// DataDir returns the data directory the starter configuration names for b.
// On Windows it sits beside the target file, so the default location and a
// per-user one share one rule.
func DataDir(b Binary, p Platform, target string) string {
	switch p.GOOS {
	case goosWindows:
		leaf := "data"
		if b == Worker {
			leaf = "worker"
		}
		return winJoin(winDir(target), leaf)
	case goosDarwin:
		return path.Join(p.Home, "Library", "Application Support", string(b))
	default:
		return path.Join("/var/lib", string(b))
	}
}

// SessionDir returns the worker.session_dir a Windows starter for an
// account (config init --user) names: a sessions directory beside target.
// Otherwise it is "" and the worker uses its default, under
// %ProgramData%\sqi on Windows, which only SYSTEM and Administrators can
// write, so a service running as an account could create no session there.
func SessionDir(b Binary, p Platform, target, account string) string {
	if b != Worker || p.GOOS != goosWindows || account == "" {
		return ""
	}
	return winJoin(winDir(target), "sessions")
}

// ValidateNATSURL checks a worker's nats.url: one or more comma-separated
// nats, tls, ws or wss URLs, each with a host and, when present, a valid port.
func ValidateNATSURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("NATS URL must not be empty")
	}
	for entry := range strings.SplitSeq(raw, ",") {
		if err := validateNATSEntry(strings.TrimSpace(entry)); err != nil {
			return err
		}
	}
	return nil
}

func validateNATSEntry(entry string) error {
	u, err := url.Parse(entry)
	if err != nil {
		return fmt.Errorf("NATS URL %q is not valid: %w", entry, err)
	}
	switch u.Scheme {
	case "nats", "tls", "ws", "wss":
	default:
		return fmt.Errorf("NATS URL %q must start with nats://, tls://, ws:// or wss://", entry)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("NATS URL %q has no host", entry)
	}
	return nil
}

// Render returns the starter configuration for b.
func Render(b Binary, p Platform, v Values) ([]byte, error) {
	if v.DataDir == "" {
		return nil, errors.New("data directory must not be empty")
	}
	if !isAbs(p, v.DataDir) {
		return nil, fmt.Errorf("data directory %q must be an absolute path", v.DataDir)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s configuration, written by %s %s.\n", b, b, oneLine(v.Version))
	fmt.Fprintf(&sb, "# Every setting and its default is documented in config/%s.example.yaml\n", b)
	fmt.Fprintf(&sb, "# and in %s.\n\n", configDocs[b])
	switch b {
	case Worker:
		if err := renderWorker(&sb, v); err != nil {
			return nil, err
		}
	case Server:
		renderServer(&sb, p, v)
	default:
		return nil, fmt.Errorf("unknown binary %q", b)
	}
	return []byte(sb.String()), nil
}

func renderWorker(sb *strings.Builder, v Values) error {
	if v.NATSURL != "" {
		if err := ValidateNATSURL(v.NATSURL); err != nil {
			return err
		}
		fmt.Fprintf(sb, "nats:\n  url: %s\n\n", quote(normalizeNATSURL(v.NATSURL)))
	} else {
		sb.WriteString("# Workers find the server by mDNS discovery when nats.url is unset.\n")
		sb.WriteString("# To name the server instead, uncomment and edit:\n")
		sb.WriteString("# nats:\n#   url: 'nats://server.example.com:4222'\n\n")
	}
	fmt.Fprintf(sb, "worker:\n  data_dir: %s\n", quote(v.DataDir))
	if v.SessionDir != "" {
		fmt.Fprintf(sb, "  session_dir: %s\n", quote(v.SessionDir))
	}
	return nil
}

func renderServer(sb *strings.Builder, p Platform, v Values) {
	fmt.Fprintf(sb, "store:\n  sqlite_path: %s\n\n", quote(join(p, v.DataDir, "sqi.db")))
	fmt.Fprintf(sb, "nats:\n  embedded:\n    data_dir: %s\n", quote(join(p, v.DataDir, "nats")))
}

// normalizeNATSURL trims each entry of a validated comma-separated list.
func normalizeNATSURL(raw string) string {
	entries := strings.Split(raw, ",")
	for i, e := range entries {
		entries[i] = strings.TrimSpace(e)
	}
	return strings.Join(entries, ",")
}

// isAbs reports whether dir is absolute for p's platform. It does not use
// filepath, which answers for the host rather than for p.
func isAbs(p Platform, dir string) bool {
	if p.GOOS != goosWindows {
		return strings.HasPrefix(dir, "/")
	}
	if strings.HasPrefix(dir, `\\`) {
		return true
	}
	return len(dir) >= 3 && dir[1] == ':' && (dir[2] == '\\' || dir[2] == '/') &&
		(dir[0] >= 'A' && dir[0] <= 'Z' || dir[0] >= 'a' && dir[0] <= 'z')
}

func join(p Platform, dir, leaf string) string {
	if p.GOOS == goosWindows {
		return winJoin(dir, leaf)
	}
	return path.Join(dir, leaf)
}

// quote returns s as a single-quoted YAML scalar, which reads backslashes
// literally and needs only its own quote doubled.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// winJoin joins Windows path elements with backslashes.
func winJoin(elems ...string) string {
	var out string
	for _, e := range elems {
		if e == "" {
			continue
		}
		if out == "" {
			out = e
			continue
		}
		out = strings.TrimRight(out, `\/`) + `\` + strings.TrimLeft(e, `\/`)
	}
	return out
}

// winDir returns the directory part of a Windows path.
func winDir(p string) string {
	i := strings.LastIndexAny(p, `\/`)
	if i < 0 {
		return "."
	}
	if i == 2 && p[1] == ':' { // "C:\file" keeps its root
		return p[:3]
	}
	return p[:i]
}
