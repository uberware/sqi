// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/store/fake"
)

func TestBootstrapAdmin(t *testing.T) {
	ctx := context.Background()
	logger := testLogger()

	// Empty DB + creds → seeds an admin.
	st := fake.New()
	if err := bootstrapAdmin(ctx, st, BootstrapParams{Username: "admin", Password: "pw"}, logger); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	u, err := st.GetUserByUsername(ctx, "admin")
	if err != nil || u.Role != "admin" {
		t.Fatalf("admin not seeded: %+v err=%v", u, err)
	}
	if u.PasswordHash == "pw" {
		t.Fatal("password stored in plaintext")
	}
	ok, err := password.Verify(u.PasswordHash, "pw")
	if err != nil {
		t.Fatalf("password.Verify: %v", err)
	}
	if !ok {
		t.Fatal("seeded password does not verify")
	}

	// Idempotent: non-empty DB → no-op (no second admin, no overwrite).
	if err := bootstrapAdmin(ctx, st, BootstrapParams{Username: "other", Password: "x"}, logger); err != nil {
		t.Fatalf("bootstrap idempotent: %v", err)
	}
	n, err := st.CountUsers(ctx)
	if err != nil {
		t.Fatalf("CountUsers: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 user after idempotent bootstrap, got %d", n)
	}
	// The original admin's password must be untouched by the no-op call.
	u2, err := st.GetUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if u2.PasswordHash != u.PasswordHash {
		t.Fatal("idempotent bootstrap overwrote the existing admin's password hash")
	}
	if _, err := st.GetUserByUsername(ctx, "other"); err == nil {
		t.Fatal("idempotent bootstrap created a second admin")
	}

	// Empty DB + no creds → no user, no error (warn path).
	st2 := fake.New()
	if err := bootstrapAdmin(ctx, st2, BootstrapParams{}, logger); err != nil {
		t.Fatalf("bootstrap no-creds: %v", err)
	}
	n2, err := st2.CountUsers(ctx)
	if err != nil {
		t.Fatalf("CountUsers: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("expected 0 users, got %d", n2)
	}
}

// TestBootstrapAdmin_WarnsWhenNoCredentials asserts that with auth enabled,
// an empty store, and no bootstrap credentials configured, the server logs a
// warning (rather than silently doing nothing or failing closed).
func TestBootstrapAdmin_WarnsWhenNoCredentials(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	st := fake.New()
	if err := bootstrapAdmin(ctx, st, BootstrapParams{}, logger); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "WARN") {
		t.Fatalf("expected a WARN log line, got: %s", out)
	}
	if !strings.Contains(out, "bootstrap") {
		t.Fatalf("expected the warning to mention bootstrap credentials, got: %s", out)
	}
}

// TestBootstrapAdmin_LogsWhenSkippedWithCredsConfigured asserts that when
// bootstrap credentials are configured but the users table is already
// non-empty (e.g. a user created while auth was off), bootstrap logs an
// explanation instead of silently doing nothing.
func TestBootstrapAdmin_LogsWhenSkippedWithCredsConfigured(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	st := fake.New()
	// Seed a user directly (simulating one created while auth was off), then
	// run bootstrap with credentials configured.
	if err := bootstrapAdmin(ctx, st, BootstrapParams{Username: "first", Password: "pw1"}, logger); err != nil {
		t.Fatalf("seed bootstrap: %v", err)
	}
	buf.Reset()

	if err := bootstrapAdmin(ctx, st, BootstrapParams{Username: "second", Password: "pw2"}, logger); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "bootstrap") {
		t.Fatalf("expected a log line explaining the skipped bootstrap, got: %s", out)
	}
	if strings.Contains(out, "pw2") {
		t.Fatalf("bootstrap logged the plaintext password: %s", out)
	}
}

// TestBootstrapAdmin_NoLogWhenSkippedWithoutCreds asserts the ordinary
// every-restart no-op (existing users, no bootstrap credentials configured)
// produces no log line — logging here would be noise on every boot.
func TestBootstrapAdmin_NoLogWhenSkippedWithoutCreds(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	st := fake.New()
	if err := bootstrapAdmin(ctx, st, BootstrapParams{Username: "first", Password: "pw1"}, logger); err != nil {
		t.Fatalf("seed bootstrap: %v", err)
	}
	buf.Reset()

	if err := bootstrapAdmin(ctx, st, BootstrapParams{}, logger); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if out := buf.String(); out != "" {
		t.Fatalf("expected no log output for the ordinary no-creds no-op, got: %s", out)
	}
}

// TestBootstrapAdmin_PasswordNeverLogged captures all log output emitted by a
// successful bootstrap and asserts the plaintext password appears nowhere in
// it. This branch has already shipped one password-leak bug (a config field
// leaked via YAML marshal); this test guards the bootstrap path specifically.
func TestBootstrapAdmin_PasswordNeverLogged(t *testing.T) {
	ctx := context.Background()
	const secret = "correct-horse-battery-staple-swordfish"
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	st := fake.New()
	if err := bootstrapAdmin(ctx, st, BootstrapParams{Username: "admin", Password: secret}, logger); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("bootstrap logged the plaintext password: %s", buf.String())
	}

	// Also ensure the stored hash itself does not embed the raw plaintext.
	u, err := st.GetUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if strings.Contains(u.PasswordHash, secret) {
		t.Fatal("stored password hash contains the plaintext password")
	}
}
