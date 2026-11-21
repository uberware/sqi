// SPDX-License-Identifier: AGPL-3.0-or-later

package configinit

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

var (
	workerSpec = Spec{Binary: Worker, Version: "1.2.3", UnixGroup: "sqiworker"}
	serverSpec = Spec{Binary: Server, Version: "1.2.3", UnixGroup: "sqi"}
)

// recorder is a writer that notes what it was asked to write. With create
// set it also writes the file, so a second run sees an existing target.
type recorder struct {
	reqs   []writeRequest
	create bool
	err    error
}

func (r *recorder) write(req writeRequest) error {
	r.reqs = append(r.reqs, req)
	if r.err != nil {
		return r.err
	}
	if r.create {
		return os.WriteFile(req.Target, req.Content, 0o600)
	}
	return nil
}

// runInit mounts the command under a root with the persistent --config flag
// both binaries define, and runs "init" with args.
func runInit(t *testing.T, spec Spec, w writer, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: string(spec.Binary), SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringP("config", "c", "", "config file")
	root.AddCommand(newCommand(spec, w))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"init"}, args...))
	err := root.Execute()
	return out.String(), err
}

func target(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "starter.yaml")
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestInit_RefusesExistingWithoutForce pins that the POSIX writer refuses an
// existing file before it writes anything; the Windows writer's refusal is
// pinned in write_windows_test.go, after its elevation check.
func TestInit_RefusesExistingWithoutForce(t *testing.T) {
	tgt := target(t)
	if err := os.WriteFile(tgt, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newMemFS()
	_, err := runInit(t, workerSpec, unixWriter(m.sys("linux"), "sqiworker"), "--config", tgt)
	if want := tgt + " exists; pass --force to replace it"; err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if len(m.files) != 0 || len(m.dirs) != 0 {
		t.Errorf("the writer wrote %q for an existing file", m.paths())
	}
	if got := readFile(t, tgt); got != "mine" {
		t.Errorf("existing file changed to %q", got)
	}
}

func TestInit_ForceReplaces(t *testing.T) {
	tgt := target(t)
	if err := os.WriteFile(tgt, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &recorder{create: true}
	out, err := runInit(t, workerSpec, r.write, "--config", tgt, "--force")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.reqs) != 1 {
		t.Fatalf("writer ran %d times, want 1", len(r.reqs))
	}
	if got := readFile(t, tgt); !strings.Contains(got, "worker:") {
		t.Errorf("file was not replaced: %q", got)
	}
	if !strings.Contains(out, "wrote "+tgt) {
		t.Errorf("output %q does not say wrote %s", out, tgt)
	}
}

func TestInit_RejectsBadNATSURL(t *testing.T) {
	r := &recorder{}
	_, err := runInit(t, workerSpec, r.write, "--config", target(t), "--nats-url", "h:4222")
	if err == nil || !strings.Contains(err.Error(), "h:4222") {
		t.Fatalf("error = %v, want one naming the URL", err)
	}
	if len(r.reqs) != 0 {
		t.Error("the writer ran for a bad URL")
	}
}

func TestInit_RejectsEmptyNATSURLWhenGiven(t *testing.T) {
	r := &recorder{}
	if _, err := runInit(t, workerSpec, r.write, "--config", target(t), "--nats-url", ""); err == nil {
		t.Error("an explicit empty --nats-url was accepted")
	}
}

func TestInit_UsesConfigFlag(t *testing.T) {
	t.Run("absolute", func(t *testing.T) {
		tgt := target(t)
		r := &recorder{}
		if _, err := runInit(t, workerSpec, r.write, "--config", tgt); err != nil {
			t.Fatal(err)
		}
		if len(r.reqs) != 1 || r.reqs[0].Target != tgt {
			t.Errorf("requests = %+v, want one for %s", r.reqs, tgt)
		}
	})
	t.Run("relative is made absolute", func(t *testing.T) {
		r := &recorder{}
		if _, err := runInit(t, workerSpec, r.write, "-c", "rel-starter.yaml"); err != nil {
			t.Fatal(err)
		}
		got := r.reqs[0].Target
		if !filepath.IsAbs(got) || filepath.Base(got) != "rel-starter.yaml" {
			t.Errorf("target = %q, want an absolute path ending in rel-starter.yaml", got)
		}
	})
}

func TestInit_RequestCarriesWhatTheWriterNeeds(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		args []string
		want []string // substrings of the rendered content
	}{
		{"worker", workerSpec, []string{"--nats-url", "nats://h:4222"}, []string{"sqi-worker 1.2.3", "'nats://h:4222'", "worker:"}},
		{"server", serverSpec, nil, []string{"sqi-server 1.2.3", "sqlite_path"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tgt := target(t)
			r := &recorder{}
			if _, err := runInit(t, tt.spec, r.write, append([]string{"--config", tgt}, tt.args...)...); err != nil {
				t.Fatal(err)
			}
			req := r.reqs[0]
			if req.Binary != tt.spec.Binary || req.Platform.GOOS != runtime.GOOS || req.DataDir == "" {
				t.Errorf("request = %+v", req)
			}
			for _, w := range tt.want {
				if !strings.Contains(string(req.Content), w) {
					t.Errorf("content lacks %q:\n%s", w, req.Content)
				}
			}
		})
	}
}

func TestInit_ServerHasNoNATSURLFlag(t *testing.T) {
	if newCommand(serverSpec, (&recorder{}).write).Flags().Lookup("nats-url") != nil {
		t.Error("the server command has a --nats-url flag")
	}
	if newCommand(workerSpec, (&recorder{}).write).Flags().Lookup("nats-url") == nil {
		t.Error("the worker command has no --nats-url flag")
	}
	if _, err := runInit(t, serverSpec, (&recorder{}).write, "--config", target(t), "--nats-url", "nats://h:4222"); err == nil {
		t.Error("the server accepted --nats-url")
	}
}

func TestInit_WriterErrorIsReturnedAndNothingIsClaimed(t *testing.T) {
	r := &recorder{err: errors.New("disk full")}
	out, err := runInit(t, workerSpec, r.write, "--config", target(t))
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(out, "wrote") || strings.Contains(out, "next:") {
		t.Errorf("output %q claims success", out)
	}
}

// TestInit_PrintsNextStep pins that run hands nextStep what it needs: the
// file written, whether it is the default one, and on Windows the running
// executable.
func TestInit_PrintsNextStep(t *testing.T) {
	orig := executable
	executable = func() (string, error) { return `C:\Program Files\sqi\sqi-worker.exe`, nil }
	t.Cleanup(func() { executable = orig })
	tgt := target(t)
	out, err := runInit(t, workerSpec, (&recorder{}).write, "--config", tgt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "wrote "+tgt+"\n") {
		t.Errorf("output %q does not start with wrote %s", out, tgt)
	}
	want := map[string]string{
		"windows": "next: & 'C:\\Program Files\\sqi\\sqi-worker.exe' service install --config '" + tgt + "' --start\n",
		"darwin":  "/sqi-worker.yaml; edit its --config to " + tgt,
		"linux":   "/etc/sqi/sqi-worker.yaml; edit its ExecStart's --config to " + tgt,
	}[runtime.GOOS]
	if want == "" {
		want = "edit its ExecStart's --config to " + tgt
	}
	if !strings.Contains(out, want) {
		t.Errorf("output %q does not contain %q", out, want)
	}
}

func TestNextStep_EveryPlatform(t *testing.T) {
	const exe = `C:\Program Files\sqi\sqi-worker.exe`
	tests := []struct {
		name string
		n    nextInfo
		want string
	}{
		{
			"linux worker",
			nextInfo{GOOS: "linux", Binary: Worker, Target: "/etc/sqi/sqi-worker.yaml"},
			"next: sudo cp deploy/systemd/sqi-worker.service /etc/systemd/system/ && sudo systemctl enable --now sqi-worker",
		},
		{
			"linux server",
			nextInfo{GOOS: "linux", Binary: Server, Target: "/etc/sqi/sqi-server.yaml"},
			"next: sudo cp deploy/systemd/sqi-server.service /etc/systemd/system/ && sudo systemctl enable --now sqi-server",
		},
		{
			"linux other file",
			nextInfo{GOOS: "linux", Binary: Worker, Target: "/srv/sqi/w.yaml", Custom: true},
			"next: deploy/systemd/sqi-worker.service reads /etc/sqi/sqi-worker.yaml; edit its ExecStart's --config to /srv/sqi/w.yaml before you install it",
		},
		{
			"darwin worker",
			nextInfo{GOOS: "darwin", Binary: Worker, Target: "/Users/a/.sqi/sqi-worker.yaml"},
			"next: cp deploy/launchd/net.uberware.sqi-worker.plist ~/Library/LaunchAgents/ && launchctl load ~/Library/LaunchAgents/net.uberware.sqi-worker.plist",
		},
		{
			"darwin worker, other file",
			nextInfo{GOOS: "darwin", Binary: Worker, Target: "/Users/a/w.yaml", Custom: true},
			"next: deploy/launchd/net.uberware.sqi-worker.plist reads ~/.sqi/sqi-worker.yaml; edit its --config to /Users/a/w.yaml before you load it",
		},
		{"darwin server", nextInfo{GOOS: "darwin", Binary: Server, Target: "/Users/a/.sqi/sqi-server.yaml"}, "next: sqi-server serve"},
		{
			"darwin server, other file",
			nextInfo{GOOS: "darwin", Binary: Server, Target: "/Users/a/my sqi/s.yaml", Custom: true},
			"next: sqi-server serve --config '/Users/a/my sqi/s.yaml'",
		},
		{
			"windows",
			nextInfo{GOOS: "windows", Binary: Worker, Target: `C:\ProgramData\sqi\sqi-worker.yaml`, Exe: exe},
			"next: & 'C:\\Program Files\\sqi\\sqi-worker.exe' service install --start",
		},
		{
			"windows, other file",
			nextInfo{GOOS: "windows", Binary: Server, Target: `D:\sqi\sqi-server.yaml`, Custom: true, Exe: exe},
			"next: & 'C:\\Program Files\\sqi\\sqi-worker.exe' service install --config 'D:\\sqi\\sqi-server.yaml' --start",
		},
		{"windows, --user", nextInfo{
			GOOS: "windows", Binary: Worker, Target: `C:\ProgramData\sqi-render\sqi-worker.yaml`, Custom: true,
			User: `STUDIO\render-svc`, Exe: exe,
		}, "next: & 'C:\\Program Files\\sqi\\sqi-worker.exe' service install --user 'STUDIO\\render-svc' --config 'C:\\ProgramData\\sqi-render\\sqi-worker.yaml'"},
		{
			"windows quoting",
			nextInfo{GOOS: "windows", Binary: Worker, Target: `C:\it's\w.yaml`, Custom: true, Exe: `C:\it's\sqi-worker.exe`},
			"next: & 'C:\\it''s\\sqi-worker.exe' service install --config 'C:\\it''s\\w.yaml' --start",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextStep(tt.n); got != tt.want {
				t.Errorf("nextStep =\n %q\nwant\n %q", got, tt.want)
			}
		})
	}
}

func TestInit_UserFlagOnlyOnWindows(t *testing.T) {
	for _, spec := range []Spec{workerSpec, serverSpec} {
		got := newCommand(spec, (&recorder{}).write).Flags().Lookup("user") != nil
		if want := runtime.GOOS == "windows"; got != want {
			t.Errorf("%s: has --user = %v, want %v", spec.Binary, got, want)
		}
	}
}
