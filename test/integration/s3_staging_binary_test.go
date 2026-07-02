// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// s3_staging_binary_test.go — hermetic end-to-end test of the stage_locally path
// delivery against an S3-rooted storage location.
//
// This proves a real s3:// staging round-trip through the real sqi-server and
// sqi-worker binaries: a job references an s3:// storage root with the
// stage_locally delivery; the worker stages the input DOWN from an object store
// into worker-local scratch, runs the task, and stages the output back UP to the
// object store. The object store is an in-process fake S3 (gofakes3) behind an
// httptest.Server, and the operator sync command is a test-only s3sync helper
// (minio-go) built from this repo. No Docker, no external MinIO, no env gating.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ── s3sync helper binary build cache ──────────────────────────────────────────

var (
	s3SyncBinaryOnce sync.Once
	s3SyncBinaryPath string
	s3SyncBinaryDir  string // intentionally left for the OS temp reaper (not registered in TestMain cleanup)
	errS3SyncBinary  error
)

// buildS3SyncBinary compiles the test-only s3sync helper command into a temp
// directory and returns the path to the executable. The build runs once per
// test-binary invocation and the result is cached via sync.Once, mirroring
// buildWorkerBinary. The temp directory outlives individual tests, so it uses
// os.MkdirTemp (not t.TempDir); it is a package-scoped var but is intentionally
// left for the OS temp reaper — the worker binary dir is the one TestMain owns.
func buildS3SyncBinary(tb testing.TB) string {
	tb.Helper()

	s3SyncBinaryOnce.Do(func() {
		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			errS3SyncBinary = errors.New("runtime.Caller(0) returned no file path")
			return
		}
		repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

		tmpDir, err := os.MkdirTemp("", "sqi-s3sync-*") //nolint:usetesting // must outlive the calling test; cached via sync.Once
		if err != nil {
			errS3SyncBinary = fmt.Errorf("create temp dir for s3sync binary: %w", err)
			return
		}
		s3SyncBinaryDir = tmpDir

		name := "s3sync"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		out := filepath.Join(tmpDir, name)

		buildCtx, buildCancel := context.WithTimeout(context.Background(), 3*time.Minute)
		cmd := exec.CommandContext(buildCtx, "go", "build", "-o", out, "github.com/uberware/sqi/test/integration/s3sync")
		cmd.Dir = repoRoot
		output, buildErr := cmd.CombinedOutput()
		buildCancel()
		if buildErr != nil {
			errS3SyncBinary = fmt.Errorf("go build s3sync: %w\n%s", buildErr, output)
			return
		}
		s3SyncBinaryPath = out
	})

	if errS3SyncBinary != nil {
		tb.Fatalf("s3sync helper binary failed to build: %v", errS3SyncBinary)
	}
	return s3SyncBinaryPath
}

// ── Fake S3 + client helpers ──────────────────────────────────────────────────

// startFakeS3 boots an in-process fake S3 (gofakes3) behind an httptest.Server
// and returns the server. It is closed automatically via t.Cleanup.
func startFakeS3(t *testing.T) *httptest.Server {
	t.Helper()
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	srv := httptest.NewServer(faker.Server())
	t.Cleanup(srv.Close)
	return srv
}

// newTestS3Client builds a path-style minio-go client against endpoint, which is
// the raw httptest URL (e.g. http://127.0.0.1:PORT).
func newTestS3Client(t *testing.T, endpoint string) *minio.Client {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse endpoint %q: %v", endpoint, err)
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4("test", "test", ""),
		Secure:       u.Scheme == "https",
		Region:       "us-east-1",
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatalf("new s3 client: %v", err)
	}
	return client
}

// setS3Env exports the AWS_* variables for the current test so the worker
// subprocess — and the s3sync subprocess it spawns per staged file — inherit
// them via os.Environ(). t.Setenv forbids t.Parallel(); these tests are serial.
func setS3Env(t *testing.T, endpoint string) {
	t.Helper()
	t.Setenv("AWS_ENDPOINT_URL", endpoint)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
}

// writeStagingWorkerConfig writes a worker config file enabling local staging
// with the given sync command and returns its path.
func writeStagingWorkerConfig(t *testing.T, syncCommand string) string {
	t.Helper()
	scratchDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	body := "staging:\n  scratch_dir: " + scratchDir + "\n"
	if syncCommand != "" {
		body += "  sync_command: " + syncCommand + "\n"
	}
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write staging config: %v", err)
	}
	return cfgPath
}

// s3StagingJobYAML builds an OpenJD template that stages an IN file down and an
// OUT file up through the named storage location, then copies IN → OUT so the
// staged output carries the staged input's content.
func s3StagingJobYAML(inLoc, outLoc string) string {
	return fmt.Sprintf(`specificationVersion: "jobtemplate-2023-09"
name: S3 Staging Integration Test
extensions: [ SQI_PATH_TRANSLATION ]
SQI_PATH_TRANSLATION:
  deliveries:
    - swap_in_place
    - stage_locally
parameterDefinitions:
  - name: InFile
    type: PATH
    objectType: FILE
    dataFlow: IN
    default: %q
  - name: OutFile
    type: PATH
    objectType: FILE
    dataFlow: OUT
    default: %q
steps:
  - name: Copy
    script:
      actions:
        onRun:
          command: bash
          args:
            - "-c"
            - 'cat "{{Param.InFile}}" > "{{Param.OutFile}}"'
`, inLoc, outLoc)
}

// createS3StorageLocation creates a storage location whose default root is the
// given s3://bucket URI. It sends NO `type` field (Task 2 derives it and rejects
// an explicit type).
func createS3StorageLocation(t *testing.T, ts *testServer, name, bucket string) {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"roots":{"default":"s3://%s"}}`, name, bucket)
	var resp struct {
		ID string `json:"id"`
	}
	mustDoJSON(t, http.MethodPost, apiURL(ts, "/api/v1/storage-locations"),
		[]byte(body), "application/json", http.StatusCreated, &resp)
	if resp.ID == "" {
		t.Fatal("createS3StorageLocation: server returned empty location ID")
	}
}

// submitS3StagingJob submits an s3-rooted stage_locally job and returns its ID.
func submitS3StagingJob(t *testing.T, ts *testServer, farmID, queueID, inLoc, outLoc string) string {
	t.Helper()
	yamlBody := s3StagingJobYAML(inLoc, outLoc)
	url := fmt.Sprintf("%s?farm_id=%s&queue_id=%s&owner=s3-staging-test",
		apiURL(ts, "/api/v1/jobs"), farmID, queueID)
	var resp jobResp
	mustDoJSON(t, http.MethodPost, url, []byte(yamlBody), "application/x-yaml", http.StatusCreated, &resp)
	if resp.ID == "" {
		t.Fatal("submitS3StagingJob: server returned empty job ID")
	}
	return resp.ID
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestS3Staging_RoundTrip proves a full s3:// staging round-trip: input staged
// down from fake S3, task run, output staged back up. It asserts the output
// object lands in the bucket carrying the input's content.
func TestS3Staging_RoundTrip(t *testing.T) {
	const bucket = "sqi-staging"

	srv := startFakeS3(t)
	setS3Env(t, srv.URL)

	ctx := context.Background()
	client := newTestS3Client(t, srv.URL)
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
		t.Fatalf("make bucket: %v", err)
	}
	const content = "hello-s3"
	if _, err := client.PutObject(ctx, bucket, "in/scene.txt",
		strings.NewReader(content), int64(len(content)), minio.PutObjectOptions{}); err != nil {
		t.Fatalf("put input object: %v", err)
	}

	syncBin := buildS3SyncBinary(t)

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	cfgPath := writeStagingWorkerConfig(t, syncBin+" {src} {dest}")
	startRealWorkerWithOptions(t, ts, farmID, queueID, []string{"--config", cfgPath}, nil)

	createS3StorageLocation(t, ts, "s3loc", bucket)

	jobID := submitS3StagingJob(t, ts, farmID, queueID,
		"loc://s3loc/in/scene.txt", "loc://s3loc/out/scene.txt")
	t.Logf("submitted s3 staging job %s", jobID)

	final := pollJobStatus(t, ts, jobID,
		[]string{"completed", "failed", "canceled"}, 90*time.Second)
	if final != "completed" {
		t.Fatalf("job final status = %q, want completed", final)
	}

	// The output object must exist in the bucket with the staged input's content.
	obj, err := client.GetObject(ctx, bucket, "out/scene.txt", minio.GetObjectOptions{})
	if err != nil {
		t.Fatalf("get output object: %v", err)
	}
	defer func() { _ = obj.Close() }()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, obj); err != nil {
		t.Fatalf("read output object (missing or unreadable): %v", err)
	}
	if got := buf.String(); got != content {
		t.Errorf("output object content = %q, want %q", got, content)
	}
}

// TestS3Staging_UnconfiguredWorkerFails proves that an s3-rooted stage_locally
// job fails when the worker has no sync_command configured — the worker rejects
// the task pre-exec because it is not configured for staging. This path fails
// before any S3 I/O.
func TestS3Staging_UnconfiguredWorkerFails(t *testing.T) {
	const bucket = "sqi-staging"

	srv := startFakeS3(t)
	setS3Env(t, srv.URL)

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	// Staging config with a scratch dir but NO sync_command → not configured.
	cfgPath := writeStagingWorkerConfig(t, "")
	startRealWorkerWithOptions(t, ts, farmID, queueID, []string{"--config", cfgPath}, nil)

	createS3StorageLocation(t, ts, "s3loc", bucket)

	jobID := submitS3StagingJob(t, ts, farmID, queueID,
		"loc://s3loc/in/scene.txt", "loc://s3loc/out/scene.txt")
	t.Logf("submitted unconfigured-worker s3 staging job %s", jobID)

	final := pollJobStatus(t, ts, jobID,
		[]string{"completed", "failed", "canceled"}, 90*time.Second)
	if final != "failed" {
		t.Fatalf("job final status = %q, want failed", final)
	}
}

// TestS3Staging_BadObjectFails proves that an s3-rooted stage_locally job fails
// when the IN object does not exist in the bucket: the s3sync copy-in fails
// (NoSuchKey), stage-in errors, and the task fails pre-exec.
func TestS3Staging_BadObjectFails(t *testing.T) {
	const bucket = "sqi-staging"

	srv := startFakeS3(t)
	setS3Env(t, srv.URL)

	ctx := context.Background()
	client := newTestS3Client(t, srv.URL)
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
		t.Fatalf("make bucket: %v", err)
	}

	syncBin := buildS3SyncBinary(t)

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	cfgPath := writeStagingWorkerConfig(t, syncBin+" {src} {dest}")
	startRealWorkerWithOptions(t, ts, farmID, queueID, []string{"--config", cfgPath}, nil)

	createS3StorageLocation(t, ts, "s3loc", bucket)

	// IN object was never put into the bucket → copy-in fails with NoSuchKey.
	jobID := submitS3StagingJob(t, ts, farmID, queueID,
		"loc://s3loc/in/missing.txt", "loc://s3loc/out/scene.txt")
	t.Logf("submitted bad-object s3 staging job %s", jobID)

	final := pollJobStatus(t, ts, jobID,
		[]string{"completed", "failed", "canceled"}, 90*time.Second)
	if final != "failed" {
		t.Fatalf("job final status = %q, want failed", final)
	}
}
