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
	Force    bool   // replace a file already at Target
	Account  string // Windows --user, normalized; "" for none
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
	if err := refuseExisting(target, force); err != nil {
		return err
	}
	dataDir := DataDir(spec.Binary, p, target)
	content, err := Render(spec.Binary, p, Values{Version: spec.Version, NATSURL: natsURL, DataDir: dataDir})
	if err != nil {
		return err
	}
	account := winsvc.NormalizeAccount(user)
	req := writeRequest{
		Binary: spec.Binary, Platform: p, Target: target, Content: content, DataDir: dataDir,
		Force: force, Account: account,
	}
	if err := w(req); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "wrote %s\n", target)
	next := nextStep(p.GOOS, spec.Binary)
	if account != "" {
		next += fmt.Sprintf(" --user %s --config %s", shellArg(user), shellArg(target))
	}
	fmt.Fprintln(out, next)
	return nil
}

// shellArg double-quotes s when it holds a space, so a printed command line
// can be pasted as it is.
func shellArg(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
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

// nextStep is the line that tells the operator how to start what was
// configured.
func nextStep(goos string, b Binary) string {
	switch goos {
	case goosWindows:
		return fmt.Sprintf("next: %s service install --start", b)
	case goosDarwin:
		if b == Worker {
			const plist = "net.uberware.sqi-worker.plist"
			return "next: cp deploy/launchd/" + plist + " ~/Library/LaunchAgents/ && launchctl load ~/Library/LaunchAgents/" + plist
		}
		return fmt.Sprintf("next: %s serve", b)
	default:
		return fmt.Sprintf("next: sudo cp deploy/systemd/%[1]s.service /etc/systemd/system/ && sudo systemctl enable --now %[1]s", b)
	}
}
