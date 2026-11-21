// SPDX-License-Identifier: AGPL-3.0-or-later

package configinit_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/configinit"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

var (
	linux   = configinit.Platform{GOOS: "linux"}
	darwin  = configinit.Platform{GOOS: "darwin", Home: "/Users/a"}
	windows = configinit.Platform{GOOS: "windows", ProgramData: `C:\ProgramData`}
	winSp   = configinit.Platform{GOOS: "windows", ProgramData: `C:\ProgramData\sqi winservice 123`}
)

func TestDefaultTarget(t *testing.T) {
	tests := []struct {
		name string
		b    configinit.Binary
		p    configinit.Platform
		want string
	}{
		{"linux worker", configinit.Worker, linux, "/etc/sqi/sqi-worker.yaml"},
		{"darwin worker", configinit.Worker, darwin, "/Users/a/.sqi/sqi-worker.yaml"},
		{"windows worker", configinit.Worker, windows, `C:\ProgramData\sqi\sqi-worker.yaml`},
		{"linux server", configinit.Server, linux, "/etc/sqi/sqi-server.yaml"},
		{"darwin server", configinit.Server, darwin, "/Users/a/.sqi/sqi-server.yaml"},
		{"windows server", configinit.Server, windows, `C:\ProgramData\sqi\sqi-server.yaml`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := configinit.DefaultTarget(tt.b, tt.p); got != tt.want {
				t.Errorf("DefaultTarget = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDataDir(t *testing.T) {
	const custom = `D:\farm\sqi-render\sqi-worker.yaml`
	tests := []struct {
		name   string
		b      configinit.Binary
		p      configinit.Platform
		target string
		want   string
	}{
		{"linux worker", configinit.Worker, linux, "/etc/sqi/sqi-worker.yaml", "/var/lib/sqi-worker"},
		{"linux server", configinit.Server, linux, "/etc/sqi/sqi-server.yaml", "/var/lib/sqi-server"},
		{"darwin worker", configinit.Worker, darwin, "/Users/a/.sqi/sqi-worker.yaml", "/Users/a/Library/Application Support/sqi-worker"},
		{"darwin server", configinit.Server, darwin, "/Users/a/.sqi/sqi-server.yaml", "/Users/a/Library/Application Support/sqi-server"},
		{"windows worker default", configinit.Worker, windows, `C:\ProgramData\sqi\sqi-worker.yaml`, `C:\ProgramData\sqi\worker`},
		{"windows server default", configinit.Server, windows, `C:\ProgramData\sqi\sqi-server.yaml`, `C:\ProgramData\sqi\data`},
		{"windows worker custom", configinit.Worker, windows, custom, `D:\farm\sqi-render\worker`},
		{"windows server custom", configinit.Server, windows, custom, `D:\farm\sqi-render\data`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := configinit.DataDir(tt.b, tt.p, tt.target); got != tt.want {
				t.Errorf("DataDir = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateNATSURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string // "" accepts; otherwise a substring of the error
	}{
		{"nats", "nats://h:4222", ""},
		{"tls", "tls://h:4222", ""},
		{"ws", "ws://h:8080", ""},
		{"wss no port", "wss://h", ""},
		{"cluster list", "tls://a:4222,tls://b:4222,tls://c:4222", ""},
		{"empty", "", "empty"},
		{"no scheme", "h:4222", "h:4222"},
		{"http", "http://h", "http://h"},
		{"no host", "nats://", "nats://"},
		{"bad port", "nats://h:notaport", "notaport"},
		{"one bad entry", "nats://a:4222,h:4222", "h:4222"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := configinit.ValidateNATSURL(tt.raw)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("rejected %q: %v", tt.raw, err)
			case tt.wantErr != "" && err == nil:
				t.Errorf("accepted %q", tt.raw)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Errorf("error %q does not name %q", err, tt.wantErr)
			}
		})
	}
}

func writeTemp(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "starter.yaml")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRender_RoundTripsThroughStrictLoader(t *testing.T) {
	platforms := []struct {
		name string
		p    configinit.Platform
		sep  string
	}{
		{"linux", linux, "/"},
		{"darwin", darwin, "/"},
		{"windows", windows, `\`},
		{"windows-spaces", winSp, `\`},
	}
	for _, pl := range platforms {
		for _, natsURL := range []string{"", "tls://a:4222,tls://b:4222"} {
			t.Run("worker/"+pl.name+"/"+natsURL, func(t *testing.T) {
				target := configinit.DefaultTarget(configinit.Worker, pl.p)
				v := configinit.Values{
					Version: "1.2.3",
					NATSURL: natsURL,
					DataDir: configinit.DataDir(configinit.Worker, pl.p, target),
				}
				out, err := configinit.Render(configinit.Worker, pl.p, v)
				if err != nil {
					t.Fatal(err)
				}
				cfg, err := workerconfig.Load(writeTemp(t, out), workerconfig.FlagOverrides{})
				if err != nil {
					t.Fatalf("strict loader rejected:\n%s\n%v", out, err)
				}
				if cfg.Worker.DataDir != v.DataDir {
					t.Errorf("worker.data_dir = %q, want %q", cfg.Worker.DataDir, v.DataDir)
				}
				if natsURL != "" && cfg.NATS.URL != natsURL {
					t.Errorf("nats.url = %q, want %q", cfg.NATS.URL, natsURL)
				}
			})
		}
		t.Run("server/"+pl.name, func(t *testing.T) {
			target := configinit.DefaultTarget(configinit.Server, pl.p)
			v := configinit.Values{Version: "1.2.3", DataDir: configinit.DataDir(configinit.Server, pl.p, target)}
			out, err := configinit.Render(configinit.Server, pl.p, v)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(writeTemp(t, out), config.FlagOverrides{})
			if err != nil {
				t.Fatalf("strict loader rejected:\n%s\n%v", out, err)
			}
			if want := v.DataDir + pl.sep + "sqi.db"; cfg.Store.SQLitePath != want {
				t.Errorf("store.sqlite_path = %q, want %q", cfg.Store.SQLitePath, want)
			}
			if want := v.DataDir + pl.sep + "nats"; cfg.NATS.Embedded.DataDir != want {
				t.Errorf("nats.embedded.data_dir = %q, want %q", cfg.NATS.Embedded.DataDir, want)
			}
		})
	}
}

func TestRender_Header(t *testing.T) {
	for _, b := range []configinit.Binary{configinit.Server, configinit.Worker} {
		t.Run(string(b), func(t *testing.T) {
			out, err := configinit.Render(b, linux, configinit.Values{Version: "9.8.7", DataDir: "/d"})
			if err != nil {
				t.Fatal(err)
			}
			first, _, _ := strings.Cut(string(out), "\n")
			if !strings.HasPrefix(first, "#") || !strings.Contains(first, string(b)) || !strings.Contains(first, "9.8.7") {
				t.Errorf("first line %q must be a comment naming %s and the version", first, b)
			}
			if !strings.Contains(string(out), "config/"+string(b)+".example.yaml") {
				t.Errorf("header does not mention config/%s.example.yaml", b)
			}
		})
	}
}

func TestRender_WindowsPathsSingleQuoted(t *testing.T) {
	winPath := regexp.MustCompile(`[A-Za-z]:\\`)
	for _, b := range []configinit.Binary{configinit.Server, configinit.Worker} {
		t.Run(string(b), func(t *testing.T) {
			target := configinit.DefaultTarget(b, winSp)
			out, err := configinit.Render(b, winSp, configinit.Values{DataDir: configinit.DataDir(b, winSp, target)})
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for line := range strings.SplitSeq(string(out), "\n") {
				if strings.HasPrefix(line, "#") || !winPath.MatchString(line) {
					continue
				}
				n++
				_, val, _ := strings.Cut(line, ": ")
				if !strings.HasPrefix(val, "'") || !strings.HasSuffix(val, "'") {
					t.Errorf("Windows path not single-quoted: %q", line)
				}
			}
			if n == 0 {
				t.Error("no Windows path found in output")
			}
		})
	}
}

func TestRender_QuotesEmbeddedSingleQuote(t *testing.T) {
	out, err := configinit.Render(configinit.Worker, linux, configinit.Values{DataDir: "/var/it's"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := workerconfig.Load(writeTemp(t, out), workerconfig.FlagOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Worker.DataDir != "/var/it's" {
		t.Errorf("data_dir = %q", cfg.Worker.DataDir)
	}
}

func TestRender_Errors(t *testing.T) {
	tests := []struct {
		name string
		b    configinit.Binary
		v    configinit.Values
	}{
		{"unknown binary", "sqi-other", configinit.Values{DataDir: "/d"}},
		{"empty data dir", configinit.Worker, configinit.Values{}},
		{"bad nats url", configinit.Worker, configinit.Values{DataDir: "/d", NATSURL: "h:4222"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := configinit.Render(tt.b, linux, tt.v); err == nil {
				t.Error("expected an error")
			}
		})
	}
}
