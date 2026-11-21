// SPDX-License-Identifier: AGPL-3.0-or-later

package configinit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
)

// memFS is the operating system as writeUnix sees it, held in memory so the
// modes and owners it produces can be read back on any host.
type memFS struct {
	groups  map[string]int
	files   map[string]*memFile
	dirs    map[string]os.FileMode
	lookups []string
	seq     int
	// fail maps an operation ("mkdirAll", "writeTemp", "chown", "chmod",
	// "rename") to the error it returns.
	fail map[string]error
}

type memFile struct {
	content  string
	mode     os.FileMode
	uid, gid int
}

func newMemFS() *memFS {
	return &memFS{
		groups: map[string]int{},
		files:  map[string]*memFile{},
		dirs:   map[string]os.FileMode{},
		fail:   map[string]error{},
	}
}

func (m *memFS) sys(goos string) unixSys {
	return unixSys{
		goos: goos,
		lookupGroup: func(name string) (int, error) {
			m.lookups = append(m.lookups, name)
			gid, ok := m.groups[name]
			if !ok {
				return 0, errGroupNotFound
			}
			return gid, nil
		},
		chown: func(p string, uid, gid int) error {
			if err := m.fail["chown"]; err != nil {
				return err
			}
			m.files[p].uid, m.files[p].gid = uid, gid
			return nil
		},
		chmod: func(p string, mode os.FileMode) error {
			if err := m.fail["chmod"]; err != nil {
				return err
			}
			m.files[p].mode = mode
			return nil
		},
		mkdirAll: func(p string, mode os.FileMode) error {
			if err := m.fail["mkdirAll"]; err != nil {
				return err
			}
			if _, ok := m.dirs[p]; !ok {
				m.dirs[p] = mode
			}
			return nil
		},
		writeTemp: func(dir string, content []byte) (string, error) {
			if err := m.fail["writeTemp"]; err != nil {
				return "", err
			}
			m.seq++
			p := path.Join(dir, fmt.Sprintf(".tmp-%d", m.seq))
			m.files[p] = &memFile{content: string(content), mode: 0o600}
			return p, nil
		},
		rename: func(from, to string) error {
			if err := m.fail["rename"]; err != nil {
				return err
			}
			m.files[to] = m.files[from]
			delete(m.files, from)
			return nil
		},
		remove: func(p string) error {
			delete(m.files, p)
			return nil
		},
	}
}

func (m *memFS) paths() []string {
	var out []string
	for p := range m.files {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

func TestWriteUnix_EtcSqiIs0640RootGroup(t *testing.T) {
	m := newMemFS()
	m.groups["sqiworker"] = 1234
	err := writeUnix("/etc/sqi/sqi-worker.yaml", []byte("body"), unixOpts{Shared: true, Group: "sqiworker"}, m.sys("linux"))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.paths(); !slices.Equal(got, []string{"/etc/sqi/sqi-worker.yaml"}) {
		t.Fatalf("files = %v, want only the target (no temp left)", got)
	}
	f := m.files["/etc/sqi/sqi-worker.yaml"]
	if f.content != "body" || f.mode != 0o640 || f.uid != 0 || f.gid != 1234 {
		t.Errorf("file = %+v, want body, mode 0640, owner 0:1234", *f)
	}
	if m.dirs["/etc/sqi"] != 0o755 {
		t.Errorf("/etc/sqi mode = %v, want 0755", m.dirs["/etc/sqi"])
	}
}

func TestWriteUnix_MissingGroupRefusesBeforeWriting(t *testing.T) {
	tests := []struct {
		group string
		want  string
	}{
		{"sqiworker", "sudo useradd --system --no-create-home --shell /usr/sbin/nologin sqiworker"},
		{"sqi", "sudo useradd --system --no-create-home --shell /usr/sbin/nologin sqi"},
	}
	for _, tt := range tests {
		t.Run(tt.group, func(t *testing.T) {
			m := newMemFS()
			err := writeUnix("/etc/sqi/x.yaml", []byte("body"), unixOpts{Shared: true, Group: tt.group}, m.sys("linux"))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one containing %q", err, tt.want)
			}
			if len(m.files) != 0 || len(m.dirs) != 0 {
				t.Errorf("wrote before finding the group missing: files %v dirs %v", m.paths(), m.dirs)
			}
		})
	}
}

func TestWriteUnix_GroupLookupFailureIsNotMissingGroup(t *testing.T) {
	sys := newMemFS().sys("linux")
	sys.lookupGroup = func(string) (int, error) { return 0, errors.New("nss unavailable") }
	err := writeUnix("/etc/sqi/x.yaml", nil, unixOpts{Shared: true, Group: "sqi"}, sys)
	if err == nil || strings.Contains(err.Error(), "useradd") || !strings.Contains(err.Error(), "nss unavailable") {
		t.Errorf("error = %v, want the lookup failure and no useradd advice", err)
	}
}

func TestWriteUnix_OtherTargetIs0600(t *testing.T) {
	m := newMemFS()
	if err := writeUnix("/home/a/w.yaml", []byte("body"), unixOpts{Group: "sqiworker"}, m.sys("linux")); err != nil {
		t.Fatal(err)
	}
	f := m.files["/home/a/w.yaml"]
	if f.mode != 0o600 || f.uid != 0 || f.gid != 0 {
		t.Errorf("file = %+v, want mode 0600 and no chown", *f)
	}
	if len(m.lookups) != 0 {
		t.Errorf("looked up %v for a private file", m.lookups)
	}
	if m.dirs["/home/a"] != 0o700 {
		t.Errorf("created directory mode = %v, want 0700", m.dirs["/home/a"])
	}
}

func TestWriteUnix_ReplaceResetsMode(t *testing.T) {
	tests := []struct {
		name   string
		opts   unixOpts
		target string
		want   os.FileMode
	}{
		{"shared", unixOpts{Shared: true, Group: "sqi"}, "/etc/sqi/sqi-server.yaml", 0o640},
		{"private", unixOpts{}, "/home/a/.sqi/sqi-server.yaml", 0o600},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMemFS()
			m.groups["sqi"] = 7
			m.files[tt.target] = &memFile{content: "old", mode: 0o644, uid: 500, gid: 500}
			if err := writeUnix(tt.target, []byte("new"), tt.opts, m.sys("linux")); err != nil {
				t.Fatal(err)
			}
			f := m.files[tt.target]
			if f.content != "new" || f.mode != tt.want {
				t.Errorf("file = %+v, want new content and mode %v", *f, tt.want)
			}
			if tt.opts.Shared && (f.uid != 0 || f.gid != 7) {
				t.Errorf("owner = %d:%d, want 0:7 (the old owner must not survive)", f.uid, f.gid)
			}
			if !tt.opts.Shared && (f.uid != 0 || f.gid != 0) {
				t.Errorf("owner = %d:%d, want the new file's own", f.uid, f.gid)
			}
		})
	}
}

func TestWriteUnix_DataDirOnlyOnDarwin(t *testing.T) {
	tests := []struct {
		goos    string
		wantDir bool
	}{
		{"darwin", true},
		{"linux", false},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			m := newMemFS()
			const data = "/Users/a/Library/Application Support/sqi-worker"
			opts := unixOpts{DataDir: data}
			if err := writeUnix("/Users/a/.sqi/sqi-worker.yaml", []byte("b"), opts, m.sys(tt.goos)); err != nil {
				t.Fatal(err)
			}
			mode, made := m.dirs[data]
			if made != tt.wantDir || (made && mode != 0o700) {
				t.Errorf("data dir created = %v with mode %v, want created = %v with 0700", made, mode, tt.wantDir)
			}
			if m.dirs["/Users/a/.sqi"] != 0o700 {
				t.Errorf("~/.sqi mode = %v, want 0700", m.dirs["/Users/a/.sqi"])
			}
		})
	}
}

func TestWriteUnix_PermissionDeniedSaysSudo(t *testing.T) {
	for _, op := range []string{"mkdirAll", "writeTemp", "chown", "chmod", "rename"} {
		t.Run(op, func(t *testing.T) {
			m := newMemFS()
			m.groups["sqi"] = 7
			m.fail[op] = fs.ErrPermission
			err := writeUnix("/etc/sqi/x.yaml", []byte("b"), unixOpts{Shared: true, Group: "sqi"}, m.sys("linux"))
			if err == nil || !strings.Contains(err.Error(), "sudo") {
				t.Fatalf("error = %v, want one that says to use sudo", err)
			}
			if !errors.Is(err, fs.ErrPermission) {
				t.Errorf("error %v does not wrap fs.ErrPermission", err)
			}
		})
	}
}

func TestWriteUnix_OtherFailuresDoNotSaySudo(t *testing.T) {
	m := newMemFS()
	m.fail["rename"] = errors.New("cross-device link")
	err := writeUnix("/home/a/x.yaml", []byte("b"), unixOpts{}, m.sys("linux"))
	if err == nil || strings.Contains(err.Error(), "sudo") {
		t.Errorf("error = %v, want a failure that does not mention sudo", err)
	}
}

func TestWriteUnix_FailureRemovesTempAndKeepsOldFile(t *testing.T) {
	for _, op := range []string{"chown", "chmod", "rename"} {
		t.Run(op, func(t *testing.T) {
			m := newMemFS()
			m.groups["sqi"] = 7
			m.fail[op] = errors.New("boom")
			const target = "/etc/sqi/x.yaml"
			m.files[target] = &memFile{content: "old", mode: 0o644}
			err := writeUnix(target, []byte("new"), unixOpts{Shared: true, Group: "sqi"}, m.sys("linux"))
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := m.paths(); !slices.Equal(got, []string{target}) {
				t.Errorf("files = %v, want only the target (temp removed)", got)
			}
			if f := m.files[target]; f.content != "old" || f.mode != 0o644 {
				t.Errorf("old file changed to %+v", *f)
			}
		})
	}
}

func TestUnixWriter_SharedOnlyForTheLinuxDefault(t *testing.T) {
	linux := Platform{GOOS: "linux"}
	darwin := Platform{GOOS: "darwin", Home: "/Users/a"}
	tests := []struct {
		name       string
		goos       string
		p          Platform
		target     string
		wantShared bool
	}{
		{"linux default", "linux", linux, "/etc/sqi/sqi-worker.yaml", true},
		{"linux elsewhere", "linux", linux, "/home/a/sqi-worker.yaml", false},
		{"darwin default", "darwin", darwin, "/Users/a/.sqi/sqi-worker.yaml", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMemFS()
			m.groups["sqiworker"] = 9
			w := unixWriter(m.sys(tt.goos), "sqiworker")
			err := w(writeRequest{Binary: Worker, Platform: tt.p, Target: tt.target, Content: []byte("b")})
			if err != nil {
				t.Fatal(err)
			}
			f := m.files[tt.target]
			if shared := f.mode == 0o640 && f.gid == 9; shared != tt.wantShared {
				t.Errorf("file = %+v, shared = %v, want %v", *f, shared, tt.wantShared)
			}
		})
	}
}
