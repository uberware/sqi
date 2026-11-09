// SPDX-License-Identifier: AGPL-3.0-or-later

// Package capacityexamples_test runs the example capacity provider scripts in this directory
// against stub cloud and BMC command-line tools, and checks the exact commands each one runs and
// the response it writes.
//
// The stubs record their argument vector and print canned JSON; they are not the real tools, so a
// pass shows the scripts build the commands their authors intended, not that the real tools accept
// them. The test needs bash and jq and is skipped on Windows and where either is missing.
package capacityexamples_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// stubScript is every stub tool. It records its argument vector (NUL-separated, the record ended by
// 0x1e), prints the canned answer for the first argument that names one (<responses>/<tool>/<arg>.json)
// and exits with the status in <responses>/<tool>.exit when that file exists.
const stubScript = `#!/usr/bin/env bash
tool=$(basename "$0")
printf '%s\0' "$tool" "$@" >> "$STUB_LOG"
printf '\x1e' >> "$STUB_LOG"
for a in "$@"; do
  if [ -f "$STUB_RESPONSES/$tool/$a.json" ]; then
    cat "$STUB_RESPONSES/$tool/$a.json"
    break
  fi
done
if [ -f "$STUB_RESPONSES/$tool.exit" ]; then
  exit "$(cat "$STUB_RESPONSES/$tool.exit")"
fi
`

var stubTools = []string{"aws", "gcloud", "az", "ipmitool"}

// tc is one script run.
type tc struct {
	name   string
	script string
	// request is the JSON request; {{DIR}} is replaced by the case's temporary directory.
	request string
	// env is the provider's env map; {{DIR}} is replaced as in request.
	env map[string]string
	// responses is the canned stub output, by "<tool>/<argument>".
	responses map[string]string
	// bmcFile, when set, is written to a file named by IPMI_BMC_FILE.
	bmcFile string
	// failTool makes the named stub tool exit 1 after recording its call.
	failTool string
	// wantResp is the exact JSON object the script must write.
	wantResp string
	// wantCalls is every recorded stub call, in order, as its argv joined by spaces.
	wantCalls []string
	// idFormat, when set, is the form every instance ID a list answers must have: the form the
	// worker's cloud_instance_id detector reports for that vendor (internal/worker/preempt).
	idFormat string
	// secret, when set, must appear in no recorded call, stdout or stderr.
	secret string
}

func request(action string, fields string) string {
	body := `{"version":1,"action":"` + action + `","group":{"id":"g-1","name":"render","kind":"sized"}`
	if fields != "" {
		body += "," + fields
	}
	return body + "}"
}

func TestExampleScripts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the example scripts are bash scripts")
	}
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH", tool)
		}
	}

	var cases []tc
	cases = append(cases, awsCases()...)
	cases = append(cases, gcpCases()...)
	cases = append(cases, azureCases()...)
	cases = append(cases, ipmiCases()...)
	for _, c := range cases {
		t.Run(c.script+"/"+c.name, func(t *testing.T) { runCase(t, c) })
	}
}

func runCase(t *testing.T, c tc) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	resp := filepath.Join(dir, "resp")
	mustMkdir(t, bin)
	for _, tool := range stubTools {
		mustWrite(t, filepath.Join(bin, tool), stubScript, 0o755)
	}
	for key, body := range c.responses {
		path := filepath.Join(resp, filepath.FromSlash(key)+".json")
		mustMkdir(t, filepath.Dir(path))
		mustWrite(t, path, body, 0o644)
	}
	if c.failTool != "" {
		mustMkdir(t, resp)
		mustWrite(t, filepath.Join(resp, c.failTool+".exit"), "1", 0o644)
	}
	logPath := filepath.Join(dir, "calls.log")

	env := []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + dir,
		"STUB_LOG=" + logPath,
		"STUB_RESPONSES=" + resp,
	}
	for k, v := range c.env {
		env = append(env, k+"="+strings.ReplaceAll(v, "{{DIR}}", dir))
	}
	if c.bmcFile != "" {
		bmc := filepath.Join(dir, "bmc.txt")
		mustWrite(t, bmc, c.bmcFile, 0o644)
		env = append(env, "IPMI_BMC_FILE="+bmc)
	}

	script, err := filepath.Abs(c.script)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", script)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(strings.ReplaceAll(c.request, "{{DIR}}", dir))
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("script failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Fatal("request text was executed: the canary file exists")
	}
	assertSingleJSONObject(t, stdout.String(), c.wantResp)
	if c.idFormat != "" {
		assertIDFormat(t, stdout.String(), c.idFormat)
	}
	calls := readCalls(t, logPath)
	if (len(calls) > 0 || len(c.wantCalls) > 0) && !reflect.DeepEqual(calls, c.wantCalls) {
		t.Errorf("recorded calls differ\n got: %q\nwant: %q", calls, c.wantCalls)
	}
	if c.secret != "" {
		for what, text := range map[string]string{"calls": strings.Join(calls, "\n"), "stdout": stdout.String(), "stderr": stderr.String()} {
			if strings.Contains(text, c.secret) {
				t.Errorf("the secret appears in %s", what)
			}
		}
	}
}

// assertSingleJSONObject checks out is exactly one JSON object equal to want.
func assertSingleJSONObject(t *testing.T, out, want string) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	var got map[string]any
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\nstdout: %q", err, out)
	}
	if dec.More() {
		t.Fatalf("stdout holds more than one JSON value: %q", out)
	}
	var wantObj map[string]any
	if err := json.Unmarshal([]byte(want), &wantObj); err != nil {
		t.Fatalf("bad want JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(got, wantObj) {
		t.Errorf("response differs\n got: %s\nwant: %s", strings.TrimSpace(out), want)
	}
}

// assertIDFormat checks every instance ID in out matches format, and that there is one.
func assertIDFormat(t *testing.T, out, format string) {
	t.Helper()
	var resp struct {
		IDs []string `json:"instance_ids"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.IDs) == 0 {
		t.Error("the list answered no instance IDs")
	}
	re := regexp.MustCompile(format)
	for _, id := range resp.IDs {
		if !re.MatchString(id) {
			t.Errorf("instance ID %q does not match the worker's cloud_instance_id form %s", id, format)
		}
	}
}

// readCalls returns every call the stubs recorded, each as its argv joined by single spaces. An
// argument holding a space would make that form ambiguous, so it fails the test.
func readCalls(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for rec := range strings.SplitSeq(string(data), "\x1e") {
		if rec == "" {
			continue
		}
		argv := strings.Split(strings.TrimSuffix(rec, "\x00"), "\x00")
		for _, a := range argv {
			if strings.ContainsAny(a, " \t\n") {
				t.Fatalf("an argument holds whitespace: %q", argv)
			}
		}
		calls = append(calls, strings.Join(argv, " "))
	}
	return calls
}

// mustJSON encodes v; a canned answer that cannot be encoded is a bug in the test.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// TestExampleScripts_Layout pins what every script must carry: the shebang, the SPDX header,
// strict mode, the executable bit, and nothing that would echo a secret or run request text.
func TestExampleScripts_Layout(t *testing.T) {
	for _, name := range []string{"aws-asg.sh", "gcp-mig.sh", "azure-vmss.sh", "ipmi.sh"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			lines := strings.Split(text, "\n")
			if lines[0] != "#!/usr/bin/env bash" {
				t.Errorf("first line is %q, want the bash shebang", lines[0])
			}
			if !strings.Contains(text, "# SPDX-License-Identifier: AGPL-3.0-or-later") {
				t.Error("missing SPDX header")
			}
			if !regexp.MustCompile(`(?m)^set -euo pipefail$`).MatchString(text) {
				t.Error("missing `set -euo pipefail`")
			}
			for _, banned := range []string{"set -x", "set -o xtrace", "eval ", "bash -c", "sh -c"} {
				if strings.Contains(text, banned) {
					t.Errorf("the script contains %q", banned)
				}
			}
			if runtime.GOOS != "windows" {
				info, err := os.Stat(name)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode()&0o111 == 0 {
					t.Error("the script is not executable")
				}
			}
		})
	}
}

// ── AWS ───────────────────────────────────────────────────────────────────────

const (
	awsA       = "i-0123456789abcdef0"
	awsB       = "i-0123456789abcdef1"
	awsC       = "i-0123456789abcdef2"
	awsGone    = "i-0123456789abcdef3" // terminating
	awsForeign = "i-0fedcba987654321f"
)

func awsDescribeJSON(desired int) string {
	b := mustJSON(map[string]any{"AutoScalingGroups": []any{map[string]any{
		"AutoScalingGroupName": "render-asg",
		"DesiredCapacity":      desired,
		"Instances": []any{
			map[string]any{"InstanceId": awsA, "LifecycleState": "InService"},
			map[string]any{"InstanceId": awsB, "LifecycleState": "InService"},
			map[string]any{"InstanceId": awsC, "LifecycleState": "Pending"},
			map[string]any{"InstanceId": awsGone, "LifecycleState": "Terminating"},
		},
	}}})
	return b
}

const awsDescribeCall = "aws autoscaling describe-auto-scaling-groups --auto-scaling-group-names render-asg --output json"

func awsCases() []tc {
	env := map[string]string{"AWS_ASG_NAME": "render-asg"}
	describe := func(desired int) map[string]string {
		return map[string]string{"aws/describe-auto-scaling-groups": awsDescribeJSON(desired)}
	}
	terminate := func(id string) string {
		return "aws autoscaling terminate-instance-in-auto-scaling-group --instance-id " + id + " --should-decrement-desired-capacity"
	}
	member := func(id string) string { return `{"worker_id":"w-` + id + `","instance_id":"` + id + `"}` }
	s := "aws-asg.sh"
	return []tc{
		{
			name: "grow raises the size", script: s, env: env, responses: describe(3),
			request:   request("grow", `"desired":5,"current":3`),
			wantResp:  `{"result":"ok","message":"desired capacity set to 5"}`,
			wantCalls: []string{awsDescribeCall, "aws autoscaling set-desired-capacity --auto-scaling-group-name render-asg --desired-capacity 5"},
		},
		{
			// R19: sqi's current can be below the cloud's real size; a lower desired must not shrink it.
			name: "grow never lowers the cloud size", script: s, env: env, responses: describe(3),
			request:   request("grow", `"desired":2,"current":2`),
			wantResp:  `{"result":"ok","message":"no change: desired <= current size"}`,
			wantCalls: []string{awsDescribeCall},
		},
		{
			name: "grow to the cloud size changes nothing", script: s, env: env, responses: describe(3),
			request:   request("grow", `"desired":3,"current":1`),
			wantResp:  `{"result":"ok","message":"no change: desired <= current size"}`,
			wantCalls: []string{awsDescribeCall},
		},
		{
			// The group's desired capacity (2) is below the instances it runs (3 not terminating).
			name: "grow counts running instances as well as the desired capacity", script: s, env: env, responses: describe(2),
			request:   request("grow", `"desired":3,"current":0`),
			wantResp:  `{"result":"ok","message":"no change: desired <= current size"}`,
			wantCalls: []string{awsDescribeCall},
		},
		{
			name: "grow with no desired changes nothing", script: s, env: env, responses: describe(3),
			request:   request("grow", ""),
			wantResp:  `{"result":"ok","message":"no change: desired <= current size"}`,
			wantCalls: []string{awsDescribeCall},
		},
		{
			name: "grow with a negative desired is refused", script: s, env: env, responses: describe(3),
			request:  request("grow", `"desired":-1`),
			wantResp: `{"result":"error","message":"desired must be a non-negative integer"}`,
		},
		{
			name: "retire a member and skip a foreign instance", script: s, env: env, responses: describe(3),
			request:   request("retire", `"members":[`+member(awsA)+`,`+member(awsForeign)+`]`),
			wantResp:  `{"result":"ok","message":"terminated 1; 1 not a member, skipped"}`,
			wantCalls: []string{awsDescribeCall, terminate(awsA)},
		},
		{
			name: "retire two members", script: s, env: env, responses: describe(3),
			request:   request("retire", `"members":[`+member(awsA)+`,`+member(awsB)+`]`),
			wantResp:  `{"result":"ok","message":"terminated 2; 0 not a member, skipped"}`,
			wantCalls: []string{awsDescribeCall, terminate(awsA), terminate(awsB)},
		},
		{
			name: "retire an orphan named by instance ID alone", script: s, env: env, responses: describe(3),
			request:   request("retire", `"members":[{"instance_id":"`+awsC+`"}]`),
			wantResp:  `{"result":"ok","message":"terminated 1; 0 not a member, skipped"}`,
			wantCalls: []string{awsDescribeCall, terminate(awsC)},
		},
		{
			name: "retire a non-member terminates nothing", script: s, env: env, responses: describe(3),
			request:   request("retire", `"members":[{"instance_id":"`+awsForeign+`"}]`),
			wantResp:  `{"result":"ok","message":"terminated 0; 1 not a member, skipped"}`,
			wantCalls: []string{awsDescribeCall},
		},
		{
			name: "retire skips an instance already terminating", script: s, env: env, responses: describe(3),
			request:   request("retire", `"members":[{"instance_id":"`+awsGone+`"}]`),
			wantResp:  `{"result":"ok","message":"terminated 0; 1 not a member, skipped"}`,
			wantCalls: []string{awsDescribeCall},
		},
		{
			name: "retire a duplicate once", script: s, env: env, responses: describe(3),
			request:   request("retire", `"members":[`+member(awsA)+`,{"instance_id":"`+awsA+`"}]`),
			wantResp:  `{"result":"ok","message":"terminated 1; 0 not a member, skipped"}`,
			wantCalls: []string{awsDescribeCall, terminate(awsA)},
		},
		{
			name: "retire with no members", script: s, env: env, responses: describe(3),
			request:   request("retire", `"members":[]`),
			wantResp:  `{"result":"ok","message":"no instances to retire"}`,
			wantCalls: nil,
		},
		{
			name: "retire rejects a hostile instance ID", script: s, env: env, responses: describe(3),
			request:  request("retire", `"members":[{"instance_id":"i-123; rm -rf /"}]`),
			wantResp: `{"result":"error","message":"invalid or missing instance_id in members"}`,
		},
		{
			name: "retire rejects a command substitution", script: s, env: env, responses: describe(3),
			request:  request("retire", `"members":[{"instance_id":"$(touch {{DIR}}/pwned)"}]`),
			wantResp: `{"result":"error","message":"invalid or missing instance_id in members"}`,
		},
		{
			name: "retire rejects the whole request when one ID is hostile", script: s, env: env, responses: describe(3),
			request:  request("retire", `"members":[`+member(awsA)+`,{"instance_id":"`+awsB+"\\n--force"+`"}]`),
			wantResp: `{"result":"error","message":"invalid or missing instance_id in members"}`,
		},
		{
			name: "retire rejects a member with no instance ID", script: s, env: env, responses: describe(3),
			request:  request("retire", `"members":[{"worker_id":"w-1"}]`),
			wantResp: `{"result":"error","message":"invalid or missing instance_id in members"}`,
		},
		{
			name: "list answers EC2 instance IDs not terminating", script: s, env: env, responses: describe(3),
			idFormat:  `^i-[0-9a-f]{8}([0-9a-f]{9})?$`,
			request:   request("list", ""),
			wantResp:  `{"result":"ok","instance_ids":["` + awsA + `","` + awsB + `","` + awsC + `"]}`,
			wantCalls: []string{awsDescribeCall},
		},
		{
			name: "list of a group that does not exist", script: s, env: env,
			responses: map[string]string{"aws/describe-auto-scaling-groups": `{"AutoScalingGroups":[]}`},
			request:   request("list", ""),
			wantResp:  `{"result":"error","message":"auto scaling group not found"}`,
			wantCalls: []string{awsDescribeCall},
		},
		{
			name: "wake is unsupported", script: s, env: env,
			request:  request("wake", `"members":[{"worker_id":"w-1"}]`),
			wantResp: `{"result":"unsupported"}`,
		},
		{
			name: "an unknown action is unsupported", script: s, env: env,
			request:  request("frobnicate", ""),
			wantResp: `{"result":"unsupported"}`,
		},
		{
			name: "a missing group name is an error", script: s,
			request:  request("list", ""),
			wantResp: `{"result":"error","message":"AWS_ASG_NAME must be set and must not start with a dash"}`,
		},
		{
			name: "a group name starting with a dash is an error", script: s,
			env:      map[string]string{"AWS_ASG_NAME": "--oops"},
			request:  request("list", ""),
			wantResp: `{"result":"error","message":"AWS_ASG_NAME must be set and must not start with a dash"}`,
		},
	}
}

// ── GCP ───────────────────────────────────────────────────────────────────────

const gcpURL = "https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a/instances/"

func gcpListJSON() string {
	row := func(name, action string) map[string]any {
		return map[string]any{"instance": gcpURL + name, "instanceStatus": "RUNNING", "currentAction": action}
	}
	b := mustJSON([]any{row("render-a1b2", "NONE"), row("render-c3d4", "CREATING"), row("render-gone", "DELETING")})
	return b
}

func gcpCases() []tc {
	env := map[string]string{"GCP_MIG_NAME": "render-mig", "GCP_ZONE": "us-central1-a"}
	resp := func(target int) map[string]string {
		return map[string]string{
			"gcloud/describe":       `{"name":"render-mig","targetSize":` + strconv.Itoa(target) + `}`,
			"gcloud/list-instances": gcpListJSON(),
		}
	}
	const base = "gcloud compute instance-groups managed "
	describe := base + "describe render-mig --format=json --zone us-central1-a"
	list := base + "list-instances render-mig --format=json --zone us-central1-a"
	s := "gcp-mig.sh"
	return []tc{
		{
			name: "grow raises the size", script: s, env: env, responses: resp(2),
			request:   request("grow", `"desired":5,"current":2`),
			wantResp:  `{"result":"ok","message":"size set to 5"}`,
			wantCalls: []string{describe, list, base + "resize render-mig --size 5 --zone us-central1-a"},
		},
		{
			name: "grow never lowers the cloud size", script: s, env: env, responses: resp(4),
			request:   request("grow", `"desired":2,"current":2`),
			wantResp:  `{"result":"ok","message":"no change: desired <= current size"}`,
			wantCalls: []string{describe, list},
		},
		{
			// Two instances are not being deleted and the target size is 1.
			name: "grow counts running instances as well as the target size", script: s, env: env, responses: resp(1),
			request:   request("grow", `"desired":2,"current":0`),
			wantResp:  `{"result":"ok","message":"no change: desired <= current size"}`,
			wantCalls: []string{describe, list},
		},
		{
			name: "grow with a regional group and a project", script: s, responses: resp(2),
			env:      map[string]string{"GCP_MIG_NAME": "render-mig", "GCP_REGION": "us-central1", "GCP_PROJECT": "my-proj"},
			request:  request("grow", `"desired":3,"current":2`),
			wantResp: `{"result":"ok","message":"size set to 3"}`,
			wantCalls: []string{
				base + "describe render-mig --format=json --region us-central1 --project my-proj",
				base + "list-instances render-mig --format=json --region us-central1 --project my-proj",
				base + "resize render-mig --size 3 --region us-central1 --project my-proj",
			},
		},
		{
			name: "retire members and skip a foreign instance", script: s, env: env, responses: resp(2),
			request:   request("retire", `"members":[{"worker_id":"w-1","instance_id":"render-a1b2"},{"instance_id":"other-vm-zzzz"},{"instance_id":"render-c3d4"}]`),
			wantResp:  `{"result":"ok","message":"deleted 2; 1 not a member, skipped"}`,
			wantCalls: []string{list, base + "delete-instances render-mig --instances=render-a1b2,render-c3d4 --zone us-central1-a"},
		},
		{
			name: "retire a non-member deletes nothing", script: s, env: env, responses: resp(2),
			request:   request("retire", `"members":[{"instance_id":"other-vm-zzzz"}]`),
			wantResp:  `{"result":"ok","message":"deleted 0; 1 not a member, skipped"}`,
			wantCalls: []string{list},
		},
		{
			name: "retire skips an instance already being deleted", script: s, env: env, responses: resp(2),
			request:   request("retire", `"members":[{"instance_id":"render-gone"}]`),
			wantResp:  `{"result":"ok","message":"deleted 0; 1 not a member, skipped"}`,
			wantCalls: []string{list},
		},
		{
			name: "retire rejects a hostile name", script: s, env: env, responses: resp(2),
			request:  request("retire", `"members":[{"instance_id":"render-a1b2; rm -rf /"}]`),
			wantResp: `{"result":"error","message":"invalid or missing instance_id in members"}`,
		},
		{
			name: "retire rejects a command substitution", script: s, env: env, responses: resp(2),
			request:  request("retire", `"members":[{"instance_id":"$(touch {{DIR}}/pwned)"}]`),
			wantResp: `{"result":"error","message":"invalid or missing instance_id in members"}`,
		},
		{
			name: "retire rejects a name that looks like a flag", script: s, env: env, responses: resp(2),
			request:  request("retire", `"members":[{"instance_id":"--project=evil"}]`),
			wantResp: `{"result":"error","message":"invalid or missing instance_id in members"}`,
		},
		{
			name: "list answers instance names not being deleted", script: s, env: env, responses: resp(2),
			idFormat:  `^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`,
			request:   request("list", ""),
			wantResp:  `{"result":"ok","instance_ids":["render-a1b2","render-c3d4"]}`,
			wantCalls: []string{list},
		},
		{
			name: "wake is unsupported", script: s, env: env,
			request:  request("wake", ""),
			wantResp: `{"result":"unsupported"}`,
		},
		{
			name: "an unknown action is unsupported", script: s, env: env,
			request:  request("frobnicate", ""),
			wantResp: `{"result":"unsupported"}`,
		},
		{
			name: "a missing group name is an error", script: s,
			env:      map[string]string{"GCP_ZONE": "us-central1-a"},
			request:  request("list", ""),
			wantResp: `{"result":"error","message":"GCP_MIG_NAME must be a valid instance group name"}`,
		},
		{
			name: "neither zone nor region is an error", script: s,
			env:      map[string]string{"GCP_MIG_NAME": "render-mig"},
			request:  request("list", ""),
			wantResp: `{"result":"error","message":"set exactly one of GCP_ZONE and GCP_REGION"}`,
		},
		{
			name: "both zone and region is an error", script: s,
			env:      map[string]string{"GCP_MIG_NAME": "render-mig", "GCP_ZONE": "us-central1-a", "GCP_REGION": "us-central1"},
			request:  request("list", ""),
			wantResp: `{"result":"error","message":"set exactly one of GCP_ZONE and GCP_REGION"}`,
		},
	}
}

// ── Azure ─────────────────────────────────────────────────────────────────────

const azVMSS = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/render-rg/providers/Microsoft.Compute/virtualMachineScaleSets/render-vmss/virtualMachines/"

func azListJSON() string {
	row := func(id, state string) map[string]any {
		return map[string]any{"instanceId": id, "id": azVMSS + id, "provisioningState": state}
	}
	b := mustJSON([]any{row("0", "Succeeded"), row("1", "Creating"), row("3", "Deleting")})
	return b
}

func azureCases() []tc {
	env := map[string]string{"AZURE_RESOURCE_GROUP": "render-rg", "AZURE_VMSS_NAME": "render-vmss"}
	resp := func(capacity string) map[string]string {
		return map[string]string{
			"az/show":           `{"name":"render-vmss","sku":{"name":"Standard_D4s_v3","capacity":` + capacity + `}}`,
			"az/list-instances": azListJSON(),
		}
	}
	const rg = " --resource-group render-rg --name render-vmss"
	show := "az vmss show" + rg + " --output json"
	list := "az vmss list-instances" + rg + " --output json"
	s := "azure-vmss.sh"
	return []tc{
		{
			name: "grow raises the size", script: s, env: env, responses: resp("2"),
			request:   request("grow", `"desired":5,"current":2`),
			wantResp:  `{"result":"ok","message":"capacity set to 5"}`,
			wantCalls: []string{show, list, "az vmss scale" + rg + " --new-capacity 5"},
		},
		{
			name: "grow never lowers the cloud size", script: s, env: env, responses: resp("4"),
			request:   request("grow", `"desired":2,"current":2`),
			wantResp:  `{"result":"ok","message":"no change: desired <= current size"}`,
			wantCalls: []string{show, list},
		},
		{
			name: "grow counts running instances as well as the capacity", script: s, env: env, responses: resp("1"),
			request:   request("grow", `"desired":2,"current":0`),
			wantResp:  `{"result":"ok","message":"no change: desired <= current size"}`,
			wantCalls: []string{show, list},
		},
		{
			name: "grow with a subscription", script: s, responses: resp("2"),
			env: map[string]string{
				"AZURE_RESOURCE_GROUP": "render-rg", "AZURE_VMSS_NAME": "render-vmss", "AZURE_SUBSCRIPTION": "sub-1",
			},
			request:  request("grow", `"desired":3,"current":2`),
			wantResp: `{"result":"ok","message":"capacity set to 3"}`,
			wantCalls: []string{
				show + " --subscription sub-1", list + " --subscription sub-1",
				"az vmss scale" + rg + " --new-capacity 3 --subscription sub-1",
			},
		},
		{
			name: "retire by scale-set instance ID and by resource ID", script: s, env: env, responses: resp("2"),
			request:   request("retire", `"members":[{"worker_id":"w-1","instance_id":"0"},{"instance_id":"`+azVMSS+`1"},{"instance_id":"7"}]`),
			wantResp:  `{"result":"ok","message":"deleted 2; 1 not a member, skipped"}`,
			wantCalls: []string{list, "az vmss delete-instances" + rg + " --instance-ids 0 1"},
		},
		{
			name: "retire a non-member deletes nothing", script: s, env: env, responses: resp("2"),
			request:   request("retire", `"members":[{"instance_id":"7"}]`),
			wantResp:  `{"result":"ok","message":"deleted 0; 1 not a member, skipped"}`,
			wantCalls: []string{list},
		},
		{
			name: "retire skips an instance already being deleted", script: s, env: env, responses: resp("2"),
			request:   request("retire", `"members":[{"instance_id":"3"}]`),
			wantResp:  `{"result":"ok","message":"deleted 0; 1 not a member, skipped"}`,
			wantCalls: []string{list},
		},
		{
			name: "retire rejects a hostile instance ID", script: s, env: env, responses: resp("2"),
			request:  request("retire", `"members":[{"instance_id":"1; rm -rf /"}]`),
			wantResp: `{"result":"error","message":"invalid or missing instance_id in members"}`,
		},
		{
			name: "retire rejects a command substitution", script: s, env: env, responses: resp("2"),
			request:  request("retire", `"members":[{"instance_id":"$(touch {{DIR}}/pwned)"}]`),
			wantResp: `{"result":"error","message":"invalid or missing instance_id in members"}`,
		},
		{
			name: "retire rejects a resource ID with a path escape", script: s, env: env, responses: resp("2"),
			request:  request("retire", `"members":[{"instance_id":"/subscriptions/x/../../virtualMachines/1"}]`),
			wantResp: `{"result":"error","message":"invalid or missing instance_id in members"}`,
		},
		{
			name: "list answers scale-set instance IDs, as the worker reports them", script: s, env: env, responses: resp("2"),
			idFormat:  `^[0-9]+$`,
			request:   request("list", ""),
			wantResp:  `{"result":"ok","instance_ids":["0","1"]}`,
			wantCalls: []string{list},
		},
		{
			name: "wake is unsupported", script: s, env: env,
			request:  request("wake", ""),
			wantResp: `{"result":"unsupported"}`,
		},
		{
			name: "an unknown action is unsupported", script: s, env: env,
			request:  request("frobnicate", ""),
			wantResp: `{"result":"unsupported"}`,
		},
		{
			name: "a missing resource group is an error", script: s,
			env:      map[string]string{"AZURE_VMSS_NAME": "render-vmss"},
			request:  request("list", ""),
			wantResp: `{"result":"error","message":"AZURE_RESOURCE_GROUP and AZURE_VMSS_NAME must be set and must not start with a dash"}`,
		},
	}
}

// ── IPMI ──────────────────────────────────────────────────────────────────────

const (
	ipmiSecret = "s3cr3t-pw-value"
	ipmiBMC    = "node-a 10.0.0.11\n# a comment\n\nnode-b bmc-b.example.net\n"
)

func ipmiCases() []tc {
	env := map[string]string{"IPMI_USER": "admin", "IPMI_PASSWORD": ipmiSecret}
	call := func(host, verb string) string {
		return "ipmitool -I lanplus -H " + host + " -U admin -E chassis power " + verb
	}
	node := func(w, h string) string { return `{"worker_id":"` + w + `","hostname":"` + h + `"}` }
	s := "ipmi.sh"
	return []tc{
		{
			name: "wake powers each member on", script: s, env: env, bmcFile: ipmiBMC, secret: ipmiSecret,
			request:   request("wake", `"members":[`+node("w-1", "node-a")+`,`+node("w-2", "node-b")+`]`),
			wantResp:  `{"result":"ok","message":"power on sent to 2 members"}`,
			wantCalls: []string{call("10.0.0.11", "on"), call("bmc-b.example.net", "on")},
		},
		{
			name: "retire powers a member off softly", script: s, env: env, bmcFile: ipmiBMC, secret: ipmiSecret,
			request:   request("retire", `"members":[`+node("w-1", "node-a")+`]`),
			wantResp:  `{"result":"ok","message":"soft power off sent to 1 members"}`,
			wantCalls: []string{call("10.0.0.11", "soft")},
		},
		{
			name: "an interface other than lanplus", script: s, bmcFile: ipmiBMC, secret: ipmiSecret,
			env:       map[string]string{"IPMI_USER": "admin", "IPMI_PASSWORD": ipmiSecret, "IPMI_INTERFACE": "lan"},
			request:   request("wake", `"members":[`+node("w-1", "node-a")+`]`),
			wantResp:  `{"result":"ok","message":"power on sent to 1 members"}`,
			wantCalls: []string{"ipmitool -I lan -H 10.0.0.11 -U admin -E chassis power on"},
		},
		{
			name: "a hostname with no BMC fails but the rest are woken", script: s, env: env, bmcFile: ipmiBMC, secret: ipmiSecret,
			request:   request("wake", `"members":[`+node("w-1", "node-a")+`,`+node("w-2", "node-unknown")+`]`),
			wantResp:  `{"result":"error","message":"power on sent to 1 of 2 members; 1 failed (unmapped hostname or ipmitool error)"}`,
			wantCalls: []string{call("10.0.0.11", "on")},
		},
		{
			name: "a hostile hostname runs nothing", script: s, env: env, bmcFile: ipmiBMC, secret: ipmiSecret,
			request:  request("wake", `"members":[{"worker_id":"w-1","hostname":"node-a; touch {{DIR}}/pwned"},{"worker_id":"w-2","hostname":"$(touch {{DIR}}/pwned)"}]`),
			wantResp: `{"result":"error","message":"power on sent to 0 of 2 members; 2 failed (unmapped hostname or ipmitool error)"}`,
		},
		{
			name: "a member with no hostname fails", script: s, env: env, bmcFile: ipmiBMC, secret: ipmiSecret,
			request:  request("retire", `"members":[{"instance_id":"i-1"}]`),
			wantResp: `{"result":"error","message":"soft power off sent to 0 of 1 members; 1 failed (unmapped hostname or ipmitool error)"}`,
		},
		{
			name: "an ipmitool failure is an error", script: s, env: env, bmcFile: ipmiBMC, secret: ipmiSecret, failTool: "ipmitool",
			request:   request("wake", `"members":[`+node("w-1", "node-a")+`]`),
			wantResp:  `{"result":"error","message":"power on sent to 0 of 1 members; 1 failed (unmapped hostname or ipmitool error)"}`,
			wantCalls: []string{call("10.0.0.11", "on")},
		},
		{
			name: "a missing password runs nothing", script: s, bmcFile: ipmiBMC,
			env:      map[string]string{"IPMI_USER": "admin"},
			request:  request("wake", `"members":[`+node("w-1", "node-a")+`]`),
			wantResp: `{"result":"error","message":"IPMI_USER and IPMI_PASSWORD must be set"}`,
		},
		{
			name: "a missing BMC file is an error", script: s, env: env,
			request:  request("wake", `"members":[`+node("w-1", "node-a")+`]`),
			wantResp: `{"result":"error","message":"IPMI_BMC_FILE must name a readable file"}`,
		},
		{
			name: "wake with no members", script: s, env: env, bmcFile: ipmiBMC,
			request:  request("wake", `"members":[]`),
			wantResp: `{"result":"ok","message":"no members"}`,
		},
		{
			name: "grow is unsupported", script: s, env: env, bmcFile: ipmiBMC,
			request:  request("grow", `"desired":3`),
			wantResp: `{"result":"unsupported"}`,
		},
		{
			name: "list is unsupported", script: s, env: env, bmcFile: ipmiBMC,
			request:  request("list", ""),
			wantResp: `{"result":"unsupported"}`,
		},
		{
			name: "an unknown action is unsupported", script: s, env: env, bmcFile: ipmiBMC,
			request:  request("frobnicate", ""),
			wantResp: `{"result":"unsupported"}`,
		},
	}
}
