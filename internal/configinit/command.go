// SPDX-License-Identifier: AGPL-3.0-or-later

package configinit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/winsvc"
)

// Spec describes the binary a "config init" command is mounted in.
type Spec struct {
	Binary    Binary
	Version   string // version.Version
	UnixGroup string // the group that reads the system-wide file: "sqiworker" / "sqi"
}

// writeRequest is what the platform write step is given.
type writeRequest struct {
	Binary   Binary
	Platform Platform
	Target   string // absolute path of the file to write
	Content  []byte
	DataDir  string // the data directory the content names
	// SessionDir is the session directory the content names, created like
	// DataDir; "" when it names none.
	SessionDir string
	Force      bool   // replace a file already at Target
	Account    string // Windows --user, normalized; "" for none
}

// writer is the platform write step: it puts req.Content at req.Target with
// the permissions that platform needs, replacing a file already there when
// req.Force is set.
type writer func(req writeRequest) error

// NewCommand returns the "init" command, which writes a starter configuration
// for spec.Binary. It reads the root command's persistent --config flag for
// the file to write.
func NewCommand(spec Spec) *cobra.Command {
	return newCommand(spec, platformWriter(spec))
}

func newCommand(spec Spec, w writer) *cobra.Command {
	var force bool
	var natsURL string
	var user string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a starter configuration file",
		Long: fmt.Sprintf(`Write a starter %[1]s configuration file.

The file goes to --config when given, else to the default location for this
operating system. An existing file is left alone unless --force is passed.
The file holds only what the program needs to start; every other setting
keeps its default and is documented in config/%[1]s.example.yaml.`, spec.Binary),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url := ""
			if cmd.Flags().Changed("nats-url") {
				if err := ValidateNATSURL(natsURL); err != nil {
					return err
				}
				url = natsURL
			}
			return run(cmd, spec, w, force, url, user)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "replace the file if it exists")
	addUserFlag(cmd, &user)
	if spec.Binary == Worker {
		cmd.Flags().StringVar(&natsURL, "nats-url", "",
			"URL of the sqi-server's NATS broker (comma-separated for a cluster); default is to discover it by mDNS")
	}
	return cmd
}

func run(cmd *cobra.Command, spec Spec, w writer, force bool, natsURL, user string) error {
	p, err := hostPlatform()
	if err != nil {
		return err
	}
	target, err := resolveTarget(configFlag(cmd), spec.Binary, p)
	if err != nil {
		return err
	}
	dataDir := DataDir(spec.Binary, p, target)
	account := winsvc.NormalizeAccount(user)
	sessionDir := SessionDir(spec.Binary, p, target, account)
	content, err := Render(spec.Binary, p, Values{
		Version: spec.Version, NATSURL: natsURL, DataDir: dataDir, SessionDir: sessionDir,
	})
	if err != nil {
		return err
	}
	req := writeRequest{
		Binary: spec.Binary, Platform: p, Target: target, Content: content, DataDir: dataDir,
		SessionDir: sessionDir, Force: force, Account: account,
	}
	if err := w(req); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "wrote %s\n", target)
	n := nextInfo{GOOS: p.GOOS, Binary: spec.Binary, Target: target, Custom: !isDefaultTarget(target, spec.Binary, p)}
	if account != "" {
		n.User = user
	}
	if p.GOOS == goosWindows {
		n.Exe = string(spec.Binary) + ".exe"
		if exe, err := executable(); err == nil {
			n.Exe = exe
		}
	}
	fmt.Fprintln(out, nextStep(n))
	return nil
}

// executable is os.Executable, replaced in tests.
var executable = os.Executable

// isDefaultTarget reports whether target is the file b's service reads when
// no --config is given, in the spelling resolveTarget gives it.
func isDefaultTarget(target string, b Binary, p Platform) bool {
	def, err := resolveTarget("", b, p)
	if err != nil {
		return false
	}
	if p.GOOS == goosWindows {
		return strings.EqualFold(def, target)
	}
	return def == target
}

// configFlag reads the binary's persistent root --config flag.
func configFlag(cmd *cobra.Command) string {
	if fl := cmd.Flag("config"); fl != nil {
		return fl.Value.String()
	}
	return ""
}

func hostPlatform() (Platform, error) {
	p := Platform{GOOS: runtime.GOOS}
	switch p.GOOS {
	case goosWindows:
		p.ProgramData = winsvc.ProgramDataDir()
	case goosDarwin:
		home, err := os.UserHomeDir()
		if err != nil {
			return Platform{}, fmt.Errorf("find the home directory: %w", err)
		}
		p.Home = home
	}
	return p, nil
}

// resolveTarget is the file to write, absolute and in the one spelling the
// platform opens (canonicalTarget), which everything after it uses: what is
// checked, what is written and what is printed are the same path.
func resolveTarget(flag string, b Binary, p Platform) (string, error) {
	if flag == "" {
		flag = DefaultTarget(b, p)
	}
	return canonicalTarget(flag)
}

// refuseExisting refuses a file already at target unless force is set. The
// platform writers call it, after any check that must come first.
func refuseExisting(target string, force bool) error {
	_, err := os.Lstat(target)
	switch {
	case err == nil:
		if force {
			return nil
		}
		return fmt.Errorf("%s exists; pass --force to replace it", target)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("check %s: %w", target, err)
	}
}

// nextInfo is what the printed next step depends on.
type nextInfo struct {
	GOOS   string
	Binary Binary
	Target string // the file written
	Custom bool   // Target is not the default file the service reads
	User   string // Windows --user as given; "" for none
	Exe    string // Windows: the running executable's full path
}

// nextStep is the line that tells the operator how to start what was
// configured. Where the shipped systemd unit or launchd agent would read the
// default file instead of a custom one, it says so rather than give a
// command that starts the service on the wrong file.
func nextStep(n nextInfo) string {
	switch n.GOOS {
	case goosWindows:
		return windowsNextStep(n)
	case goosDarwin:
		if n.Binary == Worker {
			const plist = "net.uberware.sqi-worker.plist"
			if n.Custom {
				return "next: deploy/launchd/" + plist + " reads ~/.sqi/sqi-worker.yaml; edit its --config to " +
					n.Target + " before you load it"
			}
			return "next: cp deploy/launchd/" + plist + " ~/Library/LaunchAgents/ && launchctl load ~/Library/LaunchAgents/" + plist
		}
		if n.Custom {
			return fmt.Sprintf("next: %s serve --config %s", n.Binary, posixArg(n.Target))
		}
		return fmt.Sprintf("next: %s serve", n.Binary)
	default:
		if n.Custom {
			return fmt.Sprintf("next: deploy/systemd/%[1]s.service reads /etc/sqi/%[1]s.yaml; edit its ExecStart's --config to %[2]s before you install it",
				n.Binary, n.Target)
		}
		return fmt.Sprintf("next: sudo cp deploy/systemd/%[1]s.service /etc/systemd/system/ && sudo systemctl enable --now %[1]s", n.Binary)
	}
}

// windowsNextStep is the service install command for n, to paste into
// PowerShell: the full path of the binary that wrote the file, run with the
// call operator, and every argument single-quoted. --start is left off for an
// account, which may need more rights granted before its service can start.
func windowsNextStep(n nextInfo) string {
	q := func(s string) string { return "'" + winsvc.QuotePowerShell(s) + "'" }
	line := "next: & " + q(n.Exe) + " service install"
	if n.User != "" {
		line += " --user " + q(n.User)
	}
	if n.Custom || n.User != "" {
		line += " --config " + q(n.Target)
	}
	if n.User == "" {
		line += " --start"
	}
	return line
}

// posixArg single-quotes s for a POSIX shell unless it is made only of
// characters no shell treats specially.
func posixArg(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+=:,@") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
