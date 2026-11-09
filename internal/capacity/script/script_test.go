// SPDX-License-Identifier: AGPL-3.0-or-later

package script

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const modeVar = "SQI_SCRIPT_TEST_MODE"

// redacted is the text that stands in for a provider env value.
const redacted = "<redacted>"

// TestMain turns the test binary into the provider script the tests run. The mode arrives through
// Provider.Env, which Run applies after it filters the SQI_ variables out of the server's own
// environment.
func TestMain(m *testing.M) {
	if mode := os.Getenv(modeVar); mode != "" {
		os.Exit(runChild(mode))
	}
	os.Exit(m.Run())
}

func runChild(mode string) int {
	switch mode {
	case "echo":
		in, err := io.ReadAll(os.Stdin)
		if err != nil {
			return 1
		}
		os.Stdout.Write(mustJSON(Response{Result: "ok", Message: string(in)}))
	case "unsupported":
		os.Stdout.WriteString(`{"result":"unsupported","message":"no wake here"}`)
	case "unknownresult":
		os.Stdout.WriteString(`{"result":"maybe"}`)
	case "list":
		os.Stdout.WriteString(`{"result":"ok","instance_ids":["i-1","i-2"]}`)
	case "fail":
		os.Stderr.WriteString("first line\nboom: the last line\n")
		return 3
	case "chatty":
		os.Stderr.WriteString("one\r\ntwo\nthree")
	case "sleep":
		time.Sleep(time.Minute)
	case "garbage":
		os.Stdout.WriteString("this is not json")
	case "big":
		chunk := bytes.Repeat([]byte("a"), 64<<10)
		for range 64 {
			if _, err := os.Stdout.Write(chunk); err != nil {
				return 1
			}
		}
	case "env":
		// Only the variables the tests care about: a full environment would exceed the message cap.
		var lines []string
		for _, kv := range os.Environ() {
			up := strings.ToUpper(kv)
			if strings.HasPrefix(up, "SQI_") || strings.HasPrefix(up, "SCRIPT_TEST_") || strings.HasPrefix(up, "PROVIDER_") {
				lines = append(lines, kv)
			}
		}
		os.Stdout.Write(mustJSON(Response{Result: "ok", Message: strings.Join(lines, "\n")}))
	case "echostderr":
		os.Stderr.WriteString("calling api with token " + os.Getenv("API_TOKEN") + " now\n")
	case "echomessage":
		os.Stdout.Write(mustJSON(Response{Result: "ok", Message: "using " + os.Getenv("API_TOKEN") + " and flag " + os.Getenv("SHORT_FLAG")}))
	case "echofail":
		os.Stderr.WriteString("earlier line\ncurl: auth failed for " + os.Getenv("API_TOKEN") + "\n")
		return 4
	case "ctrl":
		os.Stdout.Write(mustJSON(Response{Result: "ok", Message: "a\nb\r\tc\x1b[31md"}))
	case "longmsg":
		os.Stdout.Write(mustJSON(Response{Result: "ok", Message: strings.Repeat("é", 5000)}))
	case "exact", "exactplus":
		const head, tail = `{"result":"ok","message":"`, `"}`
		size := maxStdout
		if mode == "exactplus" {
			size++
		}
		os.Stdout.WriteString(head + strings.Repeat("a", size-len(head)-len(tail)) + tail)
	case "empty":
	default:
		os.Stderr.WriteString("unknown test mode " + mode)
		return 2
	}
	return 0
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// provider builds a Provider that runs this test binary in the given mode.
func provider(mode string, extra map[string]string) Provider {
	// The race runtime sleeps a second before a child exits 0; the children have nothing to report.
	env := map[string]string{modeVar: mode, "GORACE": "atexit_sleep_ms=0"}
	maps.Copy(env, extra)
	return Provider{Name: "test", Command: []string{os.Args[0]}, Env: env, Timeout: time.Minute}
}

var testGroup = GroupRef{ID: "g-1", Name: "farm-a", Kind: "cloud"}

// syncBuffer is a bytes.Buffer safe for the logger's use from copy goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func capturedLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func TestRun_PassesRequestOnStdin(t *testing.T) {
	req := Request{
		Version: 1,
		Action:  "wake",
		Group:   testGroup,
		Members: []Member{{
			WorkerID:   "w-1",
			Hostname:   "node1",
			IPAddress:  "10.0.0.5",
			InstanceID: "i-abc",
			Interfaces: []NetInterface{{Name: "eth0", MAC: "aa:bb:cc:dd:ee:ff", Addrs: []string{"10.0.0.5"}}},
		}},
	}
	resp, err := Run(context.Background(), provider("echo", nil), req, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Result != "ok" {
		t.Fatalf("result = %q (%s), want ok", resp.Result, resp.Message)
	}
	var got Request
	if err := json.Unmarshal([]byte(resp.Message), &got); err != nil {
		t.Fatalf("the provider did not receive JSON on stdin: %v (%q)", err, resp.Message)
	}
	gotJSON, wantJSON := mustJSON(got), mustJSON(req)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Errorf("stdin request = %s, want %s", gotJSON, wantJSON)
	}
}

func TestRun_EmptyStdoutIsOK(t *testing.T) {
	resp, err := Run(context.Background(), provider("empty", nil), Request{Version: 1, Action: "retire", Group: testGroup}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Result != "ok" {
		t.Errorf("result = %q (%s), want ok", resp.Result, resp.Message)
	}
}

func TestRun_UnsupportedPassesThrough(t *testing.T) {
	resp, err := Run(context.Background(), provider("unsupported", nil), Request{Version: 1, Action: "wake", Group: testGroup}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Result != "unsupported" || resp.Message != "no wake here" {
		t.Errorf("response = %+v, want unsupported / no wake here", resp)
	}
}

func TestRun_ListReturnsInstanceIDs(t *testing.T) {
	resp, err := Run(context.Background(), provider("list", nil), Request{Version: 1, Action: "list", Group: testGroup}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Result != "ok" || len(resp.InstanceIDs) != 2 || resp.InstanceIDs[0] != "i-1" {
		t.Errorf("response = %+v, want ok with i-1, i-2", resp)
	}
}

func TestRun_NonZeroExitIsError(t *testing.T) {
	logger, logs := capturedLogger()
	resp, err := Run(context.Background(), provider("fail", nil), Request{Version: 1, Action: "grow", Group: testGroup}, logger)
	if err != nil {
		t.Fatalf("a non-zero exit must be an error Response, not an error: %v", err)
	}
	if resp.Result != "error" {
		t.Fatalf("result = %q, want error", resp.Result)
	}
	for _, want := range []string{"exit status 3", "boom: the last line"} {
		if !strings.Contains(resp.Message, want) {
			t.Errorf("message %q does not contain %q", resp.Message, want)
		}
	}
	if strings.Contains(resp.Message, "first line") {
		t.Errorf("message %q carries more than the last stderr line", resp.Message)
	}
	out := logs.String()
	for _, want := range []string{"first line", "boom: the last line", "provider=test", "action=grow", "group=farm-a"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q does not contain %q", out, want)
		}
	}
}

func TestRun_StderrLoggedLineByLine(t *testing.T) {
	logger, logs := capturedLogger()
	if _, err := Run(context.Background(), provider("chatty", nil), Request{Version: 1, Action: "list", Group: testGroup}, logger); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var lines []string
	for l := range strings.SplitSeq(logs.String(), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) != 3 {
		t.Fatalf("logged %d records, want 3 (one, two, three):\n%s", len(lines), logs.String())
	}
	for i, want := range []string{"line=one", "line=two", "line=three"} {
		if !strings.Contains(lines[i], want) || !strings.Contains(lines[i], "level=INFO") {
			t.Errorf("record %d = %q, want an INFO record with %q", i, lines[i], want)
		}
	}
}

func TestRun_TimeoutKills(t *testing.T) {
	p := provider("sleep", nil)
	p.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := Run(context.Background(), p, Request{Version: 1, Action: "wake", Group: testGroup}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want one wrapping context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("Run took %s to give up on a 300ms timeout", elapsed)
	}
}

func TestRun_ParentCancelReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, err := Run(ctx, provider("sleep", nil), Request{Version: 1, Action: "wake", Group: testGroup}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRun_StartFailureIsError(t *testing.T) {
	p := Provider{Name: "test", Command: []string{"definitely-not-a-real-command-sqi"}, Timeout: time.Minute}
	if _, err := Run(context.Background(), p, Request{Version: 1, Action: "wake", Group: testGroup}, nil); err == nil {
		t.Fatal("a command that cannot start must return an error")
	}
	if _, err := Run(context.Background(), Provider{Name: "empty"}, Request{Version: 1}, nil); err == nil {
		t.Fatal("an empty command must return an error")
	}
}

func TestRun_GarbageStdoutIsError(t *testing.T) {
	tests := []struct {
		name string
		mode string
	}{
		{"not json", "garbage"},
		{"unknown result", "unknownresult"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := Run(context.Background(), provider(tt.mode, nil), Request{Version: 1, Action: "wake", Group: testGroup}, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if resp.Result != "error" || resp.Message == "" {
				t.Errorf("response = %+v, want an error with a message", resp)
			}
		})
	}
}

func TestRun_StdoutCapped(t *testing.T) {
	start := time.Now()
	resp, err := Run(context.Background(), provider("big", nil), Request{Version: 1, Action: "list", Group: testGroup}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Result != "error" || !strings.Contains(resp.Message, "1 MiB") {
		t.Errorf("response = %+v, want an error naming the 1 MiB cap", resp)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("an oversized stdout took %s to settle", elapsed)
	}
}

func TestRun_EnvFilteredAndMerged(t *testing.T) {
	t.Setenv("SQI_SECRET", "parent-secret-value")
	t.Setenv("SCRIPT_TEST_KEEP", "kept")
	t.Setenv("SCRIPT_TEST_OVERRIDE", "parent")
	// Values of four bytes or more are redacted from what the script reports, so the child's
	// environment is read back in its redacted form; "tok" is too short to be scrubbed.
	p := provider("env", map[string]string{"SCRIPT_TEST_OVERRIDE": "child", "PROVIDER_TOKEN": "tok", "PROVIDER_SECRET": "longsecretvalue"})
	resp, err := Run(context.Background(), p, Request{Version: 1, Action: "list", Group: testGroup}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Result != "ok" {
		t.Fatalf("result = %q (%s)", resp.Result, resp.Message)
	}
	env := strings.Fields(resp.Message) // newlines in a message become spaces
	count := func(entry string) int {
		n := 0
		for _, e := range env {
			if e == entry {
				n++
			}
		}
		return n
	}
	if strings.Contains(resp.Message, "parent-secret-value") || strings.Contains(resp.Message, "SQI_SECRET") {
		t.Errorf("the child inherited a parent SQI_ variable:\n%s", resp.Message)
	}
	for _, want := range []string{
		"SCRIPT_TEST_KEEP=kept",
		"SCRIPT_TEST_OVERRIDE=" + redacted,
		"PROVIDER_SECRET=" + redacted,
		"PROVIDER_TOKEN=tok",
		modeVar + "=env",
	} {
		if count(want) != 1 {
			t.Errorf("child environment holds %q %d times, want once:\n%s", want, count(want), resp.Message)
		}
	}
	if count("SCRIPT_TEST_OVERRIDE=parent") != 0 {
		t.Error("the parent's value survived a p.Env override")
	}
}

func TestRun_EnvFilterIsCaseInsensitiveOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("environment variable names are case-sensitive here")
	}
	t.Setenv("sqi_lower_case_secret", "lowercase-parent-value")
	resp, err := Run(context.Background(), provider("env", nil), Request{Version: 1, Action: "list", Group: testGroup}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(strings.ToLower(resp.Message), "sqi_lower_case_secret") {
		t.Errorf("a lowercase sqi_ variable reached the child:\n%s", resp.Message)
	}
}

func TestRun_StdoutBoundary(t *testing.T) {
	tests := []struct {
		mode string
		want string
	}{
		{"exact", "ok"},
		{"exactplus", "error"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			resp, err := Run(context.Background(), provider(tt.mode, nil), Request{Version: 1, Action: "list", Group: testGroup}, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if resp.Result != tt.want {
				t.Errorf("result = %q (%.80s), want %q", resp.Result, resp.Message, tt.want)
			}
		})
	}
}

func TestRun_RedactsEchoedEnvValues(t *testing.T) {
	tests := []struct {
		name string
		mode string
		env  map[string]string
		// leaks are strings that must appear nowhere in the log or the response.
		leaks []string
		// wantMessage is the exact message expected; empty skips the check.
		wantMessage string
		wantLogged  bool
	}{
		{
			name: "stderr line", mode: "echostderr",
			leaks:      []string{"s3cr3t-token-value"},
			wantLogged: true,
		},
		{
			name: "json message", mode: "echomessage",
			leaks:       []string{"s3cr3t-token-value"},
			wantMessage: "using " + redacted + " and flag on",
		},
		{
			name: "last stderr line of a failure", mode: "echofail",
			leaks:      []string{"s3cr3t-token-value"},
			wantLogged: true,
		},
		{
			name: "value containing a shorter one", mode: "echomessage",
			env:         map[string]string{"API_TOKEN": "secretabc", "OTHER": "secret"},
			leaks:       []string{"secretabc", "abc"},
			wantMessage: "using " + redacted + " and flag on",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"API_TOKEN": "s3cr3t-token-value", "SHORT_FLAG": "on"}
			maps.Copy(env, tt.env)
			logger, logs := capturedLogger()
			resp, err := Run(context.Background(), provider(tt.mode, env), Request{Version: 1, Action: "wake", Group: testGroup}, logger)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			for _, leak := range tt.leaks {
				if strings.Contains(logs.String(), leak) || strings.Contains(resp.Message, leak) {
					t.Errorf("%q leaked.\nmessage: %q\nlog: %s", leak, resp.Message, logs.String())
				}
			}
			if tt.wantMessage != "" && resp.Message != tt.wantMessage {
				t.Errorf("message = %q, want %q", resp.Message, tt.wantMessage)
			}
			if tt.wantLogged && !strings.Contains(logs.String(), redacted) {
				t.Errorf("the log lacks %s: %s", redacted, logs.String())
			}
			if tt.mode == "echofail" && (resp.Result != "error" || !strings.Contains(resp.Message, redacted)) {
				t.Errorf("response = %+v, want an error message carrying %s", resp, redacted)
			}
		})
	}
}

func TestRun_MessageIsSanitizedAndCapped(t *testing.T) {
	resp, err := Run(context.Background(), provider("ctrl", nil), Request{Version: 1, Action: "list", Group: testGroup}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Message != "a b  c [31md" {
		t.Errorf("message = %q, want control characters replaced by spaces", resp.Message)
	}

	resp, err = Run(context.Background(), provider("longmsg", nil), Request{Version: 1, Action: "list", Group: testGroup}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(resp.Message) > 2048+len("...") || !utf8.ValidString(resp.Message) || !strings.HasSuffix(resp.Message, "...") {
		t.Errorf("message of %d bytes (valid UTF-8: %v) is not capped on a rune boundary", len(resp.Message), utf8.ValidString(resp.Message))
	}
}

func TestTruncate_KeepsRunesWhole(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "hel..."},
		{"héllo", 2, "h..."},
		{"héllo", 3, "hé..."},
		{"世界", 4, "世..."},
	}
	for _, tt := range tests {
		if got := truncate(tt.in, tt.n); got != tt.want || !utf8.ValidString(got) {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}

func TestRun_EnvNeverLogged(t *testing.T) {
	const secret = "s3cr3t-token-value"
	t.Setenv("SQI_SECRET", "another-parent-secret")
	logger, logs := capturedLogger()
	tests := []struct {
		name string
		mode string
	}{
		{"success", "echo"},
		{"failure", "fail"},
		{"garbage", "garbage"},
		{"oversized", "big"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := provider(tt.mode, map[string]string{"API_TOKEN": secret})
			resp, err := Run(context.Background(), p, Request{Version: 1, Action: "wake", Group: testGroup}, logger)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if strings.Contains(resp.Message, secret) {
				t.Errorf("response message leaks an env value: %q", resp.Message)
			}
		})
	}
	// Timeout and start failure return an error; neither may embed env values.
	p := provider("sleep", map[string]string{"API_TOKEN": secret})
	p.Timeout = 200 * time.Millisecond
	_, err := Run(context.Background(), p, Request{Version: 1, Action: "wake", Group: testGroup}, logger)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("timeout error %v must exist and not embed env values", err)
	}
	bad := Provider{Name: "test", Command: []string{"definitely-not-a-real-command-sqi"}, Env: map[string]string{"API_TOKEN": secret}}
	_, err = Run(context.Background(), bad, Request{Version: 1, Action: "wake", Group: testGroup}, logger)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("start error %v must exist and not embed env values", err)
	}
	for _, leak := range []string{secret, "another-parent-secret", "API_TOKEN"} {
		if strings.Contains(logs.String(), leak) {
			t.Errorf("log output contains %q:\n%s", leak, logs.String())
		}
	}
}
