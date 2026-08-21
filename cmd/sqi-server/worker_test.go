// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// TestWorkerCmd_TokenIssue verifies that "worker token issue" prints the raw
// token to stdout exactly once and that only its hash is ever stored.
func TestWorkerCmd_TokenIssue(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	prepareRoot([]string{"worker", "token", "issue", "--db", dbPath, "--name", "ci-runner"})
	out := captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("Execute(worker token issue) error = %v", err)
		}
	})

	token := strings.TrimSpace(out)
	if token == "" {
		t.Fatal("expected a token on stdout, got empty output")
	}
	if strings.Count(out, token) != 1 {
		t.Errorf("expected the token to appear exactly once in stdout output; got:\n%s", out)
	}
	if !strings.HasPrefix(token, "sqiw_") {
		t.Errorf("token %q does not have the expected join-token prefix", token)
	}

	// The store must hold a record whose hash matches the printed token, and
	// nothing else exposes the raw value: WorkerJoinToken only ever carries
	// TokenHash, never the token itself.
	st, err := sqlite.Open(context.Background(), dbPath, sqlite.Options{AutoMigrate: false})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st.Close()

	hash := brokerauth.HashJoinToken(token)
	got, err := st.GetWorkerJoinTokenByHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("GetWorkerJoinTokenByHash: %v", err)
	}
	if got.Name != "ci-runner" {
		t.Errorf("stored token Name = %q; want %q", got.Name, "ci-runner")
	}
	if got.TokenHash == token {
		t.Error("stored TokenHash equals the raw token; only the hash must be persisted")
	}
}

// TestWorkerCmd_TokenIssue_TTLOutOfBounds verifies that an out-of-range
// --ttl is rejected before anything is written to the store.
func TestWorkerCmd_TokenIssue_TTLOutOfBounds(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	tests := []struct {
		name string
		ttl  string
	}{
		{"below floor", "30s"},
		{"above ceiling", "48h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prepareRoot([]string{"worker", "token", "issue", "--db", dbPath, "--ttl", tt.ttl})
			_ = captureStdout(t, func() {
				if err := Execute(); err == nil {
					t.Fatal("expected an error for an out-of-bounds --ttl, got nil")
				}
			})
		})
	}
}

// TestWorkerCmd_Enroll_InvalidPublicKey verifies that enrolling with a
// malformed public key fails and names the expected "U" nkey prefix.
func TestWorkerCmd_Enroll_InvalidPublicKey(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	prepareRoot([]string{"worker", "enroll", "--db", dbPath, "--worker-id", "w1", "--public-key", "not-a-valid-key"})
	var runErr error
	_ = captureStdout(t, func() {
		runErr = Execute()
	})
	if runErr == nil {
		t.Fatal("expected an error for an invalid public key, got nil")
	}
	if !strings.Contains(runErr.Error(), "'U'") {
		t.Errorf("error should name the expected 'U' nkey prefix; got: %v", runErr)
	}
}

// TestWorkerCmd_Enroll_WarnsThatARunningServerNeedsARestart pins the one
// thing an operator cannot discover from a successful enroll: the command
// opens the SQLite file from a separate process, so it cannot reload a
// running broker's authorized-key set (built once at Broker.Start). Without
// this warning the sequence reads as a success followed by an unrelated
// worker that exits on an authorization error. The revoke command carries
// the mirror-image warning; both must keep saying so.
func TestWorkerCmd_Enroll_WarnsThatARunningServerNeedsARestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	_, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}

	prepareRoot([]string{"worker", "enroll", "--db", dbPath, "--worker-id", "w1", "--public-key", pub})
	out := captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("enroll: unexpected error: %v", err)
		}
	})

	for _, want := range []string{
		"RUNNING sqi-server",
		"restarts",
		"/api/v1/workers/enroll",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("enroll output does not mention %q; got: %s", want, out)
		}
	}
}

// TestWorkerCmd_Enroll_LongHelpWarnsAboutARunningServer keeps the same
// warning on the command's own help text, so an operator reading
// "worker enroll --help" before running anything learns it too.
func TestWorkerCmd_Enroll_LongHelpWarnsAboutARunningServer(t *testing.T) {
	if !strings.Contains(workerEnrollCmd.Long, "RUNNING sqi-server") {
		t.Errorf("workerEnrollCmd.Long does not warn that a running server will not see the credential; got:\n%s", workerEnrollCmd.Long)
	}
	if !strings.Contains(workerEnrollCmd.Long, "POST /api/v1/workers/enroll") {
		t.Errorf("workerEnrollCmd.Long does not name the REST alternative; got:\n%s", workerEnrollCmd.Long)
	}
}

// TestWorkerCmd_Enroll_DuplicateWorkerIDFails verifies that enrolling the
// same worker ID twice with two different keys fails the second time.
func TestWorkerCmd_Enroll_DuplicateWorkerIDFails(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	_, pub1, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	_, pub2, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}

	prepareRoot([]string{"worker", "enroll", "--db", dbPath, "--worker-id", "w1", "--public-key", pub1})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("first enroll: unexpected error: %v", err)
		}
	})

	prepareRoot([]string{"worker", "enroll", "--db", dbPath, "--worker-id", "w1", "--public-key", pub2})
	var secondErr error
	_ = captureStdout(t, func() {
		secondErr = Execute()
	})
	if secondErr == nil {
		t.Fatal("expected the second enroll for the same worker ID to fail, got nil")
	}
}

// TestWorkerCmd_RotationAfterRevoke walks the whole key-rotation flow:
// enroll -> (keygen --force, stood in for by generating a second local
// keypair, exactly as sqi-worker keygen would produce) -> re-enrolling that
// worker ID must fail while the old credential is still active -> revoke ->
// re-enroll with the new key succeeds -> the new key, not the old one, is
// what's active. It holds only because
// internal/store/migrations/00030_broker_auth.sql scopes worker_id
// uniqueness to active rows, not the whole table; scoped to the whole table,
// revocation is a one-way door and the worker ID can never be used again.
func TestWorkerCmd_RotationAfterRevoke(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	_, pub1, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed (first key): %v", err)
	}
	_, pub2, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed (rotated key): %v", err)
	}

	prepareRoot([]string{"worker", "enroll", "--db", dbPath, "--worker-id", "w1", "--public-key", pub1})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("initial enroll: unexpected error: %v", err)
		}
	})

	// Rotating the key locally (what "sqi-worker keygen --force" does) does
	// not by itself free up the worker ID on the server: the old credential
	// is still active, so re-enrolling must still fail here.
	prepareRoot([]string{"worker", "enroll", "--db", dbPath, "--worker-id", "w1", "--public-key", pub2})
	var beforeRevokeErr error
	_ = captureStdout(t, func() {
		beforeRevokeErr = Execute()
	})
	if beforeRevokeErr == nil {
		t.Fatal("expected re-enrolling an active worker ID with a new key to fail before revoking the old credential")
	}

	prepareRoot([]string{"worker", "revoke", "w1", "--db", dbPath})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("revoke: unexpected error: %v", err)
		}
	})

	// With the old credential revoked, the same worker ID must be free to
	// enroll again with the new key.
	prepareRoot([]string{"worker", "enroll", "--db", dbPath, "--worker-id", "w1", "--public-key", pub2})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("re-enroll after revoke: unexpected error: %v", err)
		}
	})

	prepareRoot([]string{"worker", "list", "--db", dbPath})
	out := captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("list: unexpected error: %v", err)
		}
	})
	if !strings.Contains(out, pub2) {
		t.Errorf("list output missing the rotated (new) public key; got:\n%s", out)
	}
	if strings.Contains(out, pub1) {
		t.Errorf("list output still shows the revoked (old) public key; got:\n%s", out)
	}
}

// TestWorkerCmd_Revoke_UnknownWorkerFails verifies that revoking a worker
// with no credential exits non-zero with an accurate message — one that
// does not claim the worker itself doesn't exist, since the same error also
// covers "already revoked".
func TestWorkerCmd_Revoke_UnknownWorkerFails(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	prepareRoot([]string{"worker", "revoke", "does-not-exist", "--db", dbPath})
	var runErr error
	_ = captureStdout(t, func() {
		runErr = Execute()
	})
	if runErr == nil {
		t.Fatal("expected an error revoking an unknown worker, got nil")
	}
	if strings.Contains(runErr.Error(), "does not exist") {
		t.Errorf("error must not claim the worker does not exist (it may also be already-revoked); got: %v", runErr)
	}
	if !strings.Contains(runErr.Error(), "already be revoked") {
		t.Errorf("error should mention the already-revoked possibility; got: %v", runErr)
	}
}

// TestWorkerCmd_Revoke_TwiceFailsTheSecondTime pins the documented behavior
// that RevokeWorkerCredential collapses "unknown worker" and "already
// revoked" into the same not-found outcome.
func TestWorkerCmd_Revoke_TwiceFailsTheSecondTime(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	_, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	prepareRoot([]string{"worker", "enroll", "--db", dbPath, "--worker-id", "w1", "--public-key", pub})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("enroll: unexpected error: %v", err)
		}
	})

	prepareRoot([]string{"worker", "revoke", "w1", "--db", dbPath})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("first revoke: unexpected error: %v", err)
		}
	})

	prepareRoot([]string{"worker", "revoke", "w1", "--db", dbPath})
	var secondErr error
	_ = captureStdout(t, func() {
		secondErr = Execute()
	})
	if secondErr == nil {
		t.Fatal("expected the second revoke to fail, got nil")
	}
}

// TestWorkerCmd_List verifies that "worker list" reflects enrollment and
// stops listing a worker once its credential is revoked (list only shows
// active credentials).
func TestWorkerCmd_List(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	_, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	prepareRoot([]string{"worker", "enroll", "--db", dbPath, "--worker-id", "w1", "--public-key", pub, "--name", "render-01"})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("enroll: unexpected error: %v", err)
		}
	})

	prepareRoot([]string{"worker", "list", "--db", dbPath})
	out := captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("list: unexpected error: %v", err)
		}
	})
	if !strings.Contains(out, "w1") || !strings.Contains(out, "render-01") || !strings.Contains(out, pub) {
		t.Errorf("list output missing enrolled worker fields; got:\n%s", out)
	}

	prepareRoot([]string{"worker", "revoke", "w1", "--db", dbPath})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("revoke: unexpected error: %v", err)
		}
	})

	prepareRoot([]string{"worker", "list", "--db", dbPath})
	out = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("list after revoke: unexpected error: %v", err)
		}
	})
	if strings.Contains(out, "w1") {
		t.Errorf("list output should not include a revoked worker; got:\n%s", out)
	}
}

// TestWorkerCmd_OpenWorkerStore_EmptyDBPath verifies the validation guard
// fires without needing a cobra round-trip.
func TestWorkerCmd_OpenWorkerStore_EmptyDBPath(t *testing.T) {
	origDB := workerFlags.DBPath
	t.Cleanup(func() { workerFlags.DBPath = origDB })

	workerFlags.DBPath = ""
	_, err := openWorkerStore(context.Background())
	if err == nil {
		t.Fatal("expected an error for an empty --db path, got nil")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention 'empty'; got: %v", err)
	}
}
