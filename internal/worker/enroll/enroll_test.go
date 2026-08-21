// SPDX-License-Identifier: AGPL-3.0-or-later

package enroll_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/worker/enroll"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestEnsureCredential_ExistingSeedIsLoadedWithNoHTTPRequest(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "worker.nk")

	wantSeed, wantPub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	if err := brokerauth.SaveSeed(credFile, wantSeed); err != nil {
		t.Fatalf("SaveSeed: %v", err)
	}

	requested := false
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		requested = true
	}))
	defer srv.Close()

	cfg := enroll.Config{
		WorkerID:       "worker-a",
		CredentialFile: credFile,
		JoinToken:      "should-not-be-used",
		ServerURL:      srv.URL,
	}
	seed, pub, err := enroll.EnsureCredential(context.Background(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("EnsureCredential: %v", err)
	}
	if string(seed) != string(wantSeed) {
		t.Errorf("seed = %q, want %q", seed, wantSeed)
	}
	if pub != wantPub {
		t.Errorf("public key = %q, want %q", pub, wantPub)
	}
	if requested {
		t.Error("EnsureCredential made an HTTP request despite an existing seed file")
	}
}

func TestEnsureCredential_NoSeedWithTokenEnrollsAndWritesSeed(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "worker.nk")

	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/workers/enroll" {
			t.Errorf("path = %s, want /api/v1/workers/enroll", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	cfg := enroll.Config{
		WorkerID:       "worker-a",
		CredentialFile: credFile,
		JoinToken:      "tok-123",
		ServerURL:      srv.URL,
	}
	seed, pub, err := enroll.EnsureCredential(context.Background(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("EnsureCredential: %v", err)
	}
	if len(seed) == 0 || pub == "" {
		t.Fatalf("EnsureCredential returned empty seed/publicKey: seed=%d pub=%q", len(seed), pub)
	}

	if gotBody["join_token"] != "tok-123" {
		t.Errorf("join_token = %q, want tok-123", gotBody["join_token"])
	}
	if gotBody["worker_id"] != "worker-a" {
		t.Errorf("worker_id = %q, want worker-a", gotBody["worker_id"])
	}
	if gotBody["public_key"] != pub {
		t.Errorf("public_key = %q, want %q", gotBody["public_key"], pub)
	}

	info, err := os.Stat(credFile)
	if err != nil {
		t.Fatalf("Stat credential file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credential file mode = %o, want 600", perm)
	}

	// The credential file now contains the same seed that was returned, so a
	// subsequent boot loads it without re-enrolling.
	onDisk, err := brokerauth.LoadSeed(credFile)
	if err != nil {
		t.Fatalf("LoadSeed after enrollment: %v", err)
	}
	if string(onDisk) != string(seed) {
		t.Errorf("seed on disk = %q, want %q", onDisk, seed)
	}
}

func TestEnsureCredential_NoSeedNoTokenReturnsErrNoCredential(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "worker.nk")

	requested := false
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		requested = true
	}))
	defer srv.Close()

	cfg := enroll.Config{
		WorkerID:       "worker-a",
		CredentialFile: credFile,
		ServerURL:      srv.URL,
	}
	_, _, err := enroll.EnsureCredential(context.Background(), cfg, discardLogger())
	if !errors.Is(err, enroll.ErrNoCredential) {
		t.Fatalf("EnsureCredential error = %v, want ErrNoCredential", err)
	}
	if requested {
		t.Error("EnsureCredential made an HTTP request with no seed and no token")
	}
}

func TestEnsureCredential_401MentionsTokenAndLeavesNoSeed(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "worker.nk")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := enroll.Config{
		WorkerID:       "worker-a",
		CredentialFile: credFile,
		JoinToken:      "stale-token",
		ServerURL:      srv.URL,
	}
	_, _, err := enroll.EnsureCredential(context.Background(), cfg, discardLogger())
	if err == nil {
		t.Fatal("EnsureCredential: want error for a 401 response, got nil")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error %q does not mention the token", err.Error())
	}

	if _, statErr := os.Stat(credFile); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("Stat(credFile) error = %v, want os.ErrNotExist", statErr)
	}
}

func TestEnsureCredential_409MentionsWorkerIDAlreadyEnrolledAndLeavesNoSeed(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "worker.nk")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	cfg := enroll.Config{
		WorkerID:       "worker-a",
		CredentialFile: credFile,
		JoinToken:      "tok-123",
		ServerURL:      srv.URL,
	}
	_, _, err := enroll.EnsureCredential(context.Background(), cfg, discardLogger())
	if err == nil {
		t.Fatal("EnsureCredential: want error for a 409 response, got nil")
	}
	if !strings.Contains(err.Error(), "worker-a") || !strings.Contains(err.Error(), "already enrolled") {
		t.Errorf("error %q does not mention the worker id already being enrolled with a different key", err.Error())
	}

	if _, statErr := os.Stat(credFile); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("Stat(credFile) error = %v, want os.ErrNotExist", statErr)
	}
}

func TestEnsureCredential_JoinTokenFileTakesPrecedenceOverJoinToken(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "worker.nk")
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		gotToken = body["join_token"]
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	cfg := enroll.Config{
		WorkerID:       "worker-a",
		CredentialFile: credFile,
		JoinToken:      "should-not-be-used",
		JoinTokenFile:  tokenFile,
		ServerURL:      srv.URL,
	}
	if _, _, err := enroll.EnsureCredential(context.Background(), cfg, discardLogger()); err != nil {
		t.Fatalf("EnsureCredential: %v", err)
	}
	if gotToken != "file-token" {
		t.Errorf("join_token = %q, want file-token", gotToken)
	}
}
