// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
)

// identityBroker is a broker whose only working method is Identity.
type identityBroker struct {
	bus.Broker

	identity string
}

func (b identityBroker) Identity() string { return b.identity }

// layoutServer is a Server holding only what reconcileBrokerLayout reads.
func layoutServer(t *testing.T, st store.Store, b bus.Broker) (*Server, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	s := &Server{
		logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		store:  st,
		broker: b,
	}
	return s, &buf
}

// seedOpenAttempts leaves one running and one assigned task, each with an open
// attempt, and returns their task IDs.
func seedOpenAttempts(t *testing.T, st store.Store) []string {
	t.Helper()
	ctx := t.Context()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "f1", Name: "f"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "q1", FarmID: "f1", Name: "q"}); err != nil {
		t.Fatal(err)
	}
	_, attempts := storetest.NewJob("j1", "f1", "q1").
		Step("s1", store.StepStatusReady).
		Task("t1", "s1", store.TaskStatusRunning).
		Task("t2", "s1", store.TaskStatusAssigned).
		SubmitLeasing(t, st, "w1")
	if len(attempts) != 2 {
		t.Fatalf("seeded %d attempts, want 2", len(attempts))
	}
	return []string{"t1", "t2"}
}

func requireFenced(t *testing.T, st store.Store, taskIDs []string) {
	t.Helper()
	for _, id := range taskIDs {
		task, err := st.GetTask(t.Context(), id)
		if err != nil || task.Status != store.TaskStatusReady || task.FailedAttempts != 0 {
			t.Fatalf("task %s = %+v, %v; want ready with no failed attempt consumed", id, task, err)
		}
	}
	open, err := st.ListOpenAttempts(t.Context())
	if err != nil || len(open) != 0 {
		t.Fatalf("open attempts = %v, %v; want none", open, err)
	}
}

func requireStillOpen(t *testing.T, st store.Store, taskIDs []string) {
	t.Helper()
	for _, id := range taskIDs {
		task, err := st.GetTask(t.Context(), id)
		if err != nil || (task.Status != store.TaskStatusRunning && task.Status != store.TaskStatusAssigned) {
			t.Fatalf("task %s = %+v, %v; want it left running or assigned", id, task, err)
		}
	}
}

func recordedIdentity(t *testing.T, st store.Store) string {
	t.Helper()
	got, err := st.GetBrokerIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestReconcileBrokerLayout_FirstStartFencesOpenAttempts(t *testing.T) {
	st := fake.New()
	ids := seedOpenAttempts(t, st)
	attempts, err := st.ListOpenAttempts(t.Context())
	if err != nil || len(attempts) != 2 {
		t.Fatalf("open attempts = %v, %v; want 2", attempts, err)
	}
	s, logs := layoutServer(t, st, bus.NewEmbedded(bus.EmbeddedConfig{DataDir: t.TempDir()}, slog.Default()))

	if err := s.reconcileBrokerLayout(t.Context()); err != nil {
		t.Fatalf("reconcileBrokerLayout: %v", err)
	}

	requireFenced(t, st, ids)
	for _, a := range attempts {
		got, err := st.GetTaskAttempt(t.Context(), a.AttemptID)
		if err != nil || got.Status != store.AttemptStatusFailed || got.Message != store.FailureReasonWorkerShutdown {
			t.Fatalf("attempt %s = %+v, %v; want failed with %q", a.AttemptID, got, err, store.FailureReasonWorkerShutdown)
		}
	}
	if got := recordedIdentity(t, st); got != "embedded:SQI" {
		t.Fatalf("recorded identity = %q, want embedded:SQI", got)
	}
	out := logs.String()
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "embedded:SQI") ||
		!strings.Contains(out, "no broker identity recorded; open attempts fenced") || !strings.Contains(out, "fenced_attempts=2") {
		t.Fatalf("expected an INFO log naming the fence and the new identity, got:\n%s", out)
	}
	if strings.Contains(out, "layout changed") {
		t.Fatalf("a database with no recorded identity logged a layout change:\n%s", out)
	}
}

// TestReconcileBrokerLayout_NewDatabaseLogsFirstRecord checks that a new
// database, with nothing recorded and nothing open, records the identity
// without claiming a layout change or a fence.
func TestReconcileBrokerLayout_NewDatabaseLogsFirstRecord(t *testing.T) {
	st := fake.New()
	s, logs := layoutServer(t, st, identityBroker{identity: "external:SQI@c1"})

	if err := s.reconcileBrokerLayout(t.Context()); err != nil {
		t.Fatalf("reconcileBrokerLayout: %v", err)
	}

	if got := recordedIdentity(t, st); got != "external:SQI@c1" {
		t.Fatalf("recorded identity = %q, want external:SQI@c1", got)
	}
	out := logs.String()
	if !strings.Contains(out, `msg="bus: broker identity recorded"`) || !strings.Contains(out, "identity=external:SQI@c1") {
		t.Fatalf("expected the first-record INFO naming the identity, got:\n%s", out)
	}
	if strings.Contains(out, "fenced") || strings.Contains(out, "layout changed") {
		t.Fatalf("a new database logged a fence or a layout change:\n%s", out)
	}
}

func TestReconcileBrokerLayout_SameIdentityFencesNothing(t *testing.T) {
	st := fake.New()
	ids := seedOpenAttempts(t, st)
	if err := st.SetBrokerIdentity(t.Context(), "embedded:SQI", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	s, _ := layoutServer(t, st, bus.NewEmbedded(bus.EmbeddedConfig{DataDir: t.TempDir()}, slog.Default()))

	if err := s.reconcileBrokerLayout(t.Context()); err != nil {
		t.Fatalf("reconcileBrokerLayout: %v", err)
	}

	requireStillOpen(t, st, ids)
	if got := recordedIdentity(t, st); got != "embedded:SQI" {
		t.Fatalf("recorded identity = %q, want embedded:SQI", got)
	}
}

func TestReconcileBrokerLayout_IdentityChangeFences(t *testing.T) {
	st := fake.New()
	ids := seedOpenAttempts(t, st)
	if err := st.SetBrokerIdentity(t.Context(), "embedded:SQI", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	s, logs := layoutServer(t, st, identityBroker{identity: "external:SQI@c1"})

	if err := s.reconcileBrokerLayout(t.Context()); err != nil {
		t.Fatalf("reconcileBrokerLayout: %v", err)
	}

	requireFenced(t, st, ids)
	if got := recordedIdentity(t, st); got != "external:SQI@c1" {
		t.Fatalf("recorded identity = %q, want external:SQI@c1", got)
	}
	out := logs.String()
	for _, want := range []string{"embedded:SQI", "external:SQI@c1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log does not name %q:\n%s", want, out)
		}
	}
}

// failingFenceStore fails to list open attempts, so the fence cannot run.
type failingFenceStore struct {
	store.Store
}

func (failingFenceStore) ListOpenAttempts(context.Context) ([]store.OpenAttempt, error) {
	return nil, context.DeadlineExceeded
}

func TestReconcileBrokerLayout_FenceFailureRecordsNothing(t *testing.T) {
	st := fake.New()
	s, _ := layoutServer(t, failingFenceStore{Store: st}, identityBroker{identity: "external:SQI@c1"})

	if err := s.reconcileBrokerLayout(t.Context()); err == nil {
		t.Fatal("reconcileBrokerLayout succeeded though the fence failed")
	}
	if got := recordedIdentity(t, st); got != "" {
		t.Fatalf("recorded identity = %q after a failed fence, want it left unrecorded so the next start retries", got)
	}
}

func TestReconcileBrokerLayout_LeftoverGlobalStreamsLogged(t *testing.T) {
	dir := t.TempDir()
	leftover := filepath.Join(dir, "jetstream", "$G")
	if err := os.MkdirAll(filepath.Join(leftover, "streams"), 0o750); err != nil {
		t.Fatal(err)
	}
	st := fake.New()
	if err := st.SetBrokerIdentity(t.Context(), "embedded:SQI", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	s, logs := layoutServer(t, st, bus.NewEmbedded(bus.EmbeddedConfig{DataDir: dir}, slog.Default()))
	s.cfg.NATS.Embedded.DataDir = dir

	if err := s.reconcileBrokerLayout(t.Context()); err != nil {
		t.Fatalf("reconcileBrokerLayout: %v", err)
	}

	var hits []string
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, leftover) {
			hits = append(hits, line)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("want exactly one log line naming %s, got %d:\n%s", leftover, len(hits), logs.String())
	}
	if !strings.Contains(hits[0], "level=INFO") || !strings.Contains(hits[0], "abandoned") || !strings.Contains(hits[0], "safe to delete") {
		t.Fatalf("log line = %q; want INFO saying the directory is abandoned and safe to delete", hits[0])
	}
	if _, err := os.Stat(leftover); err != nil {
		t.Fatalf("leftover directory was touched: %v", err)
	}
}

func TestReconcileBrokerLayout_NoLeftoverNoLog(t *testing.T) {
	st := fake.New()
	s, logs := layoutServer(t, st, bus.NewEmbedded(bus.EmbeddedConfig{DataDir: t.TempDir()}, slog.Default()))
	if err := s.reconcileBrokerLayout(t.Context()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "abandoned") {
		t.Fatalf("logged an abandoned directory with none present:\n%s", logs.String())
	}
}

func TestReconcileBrokerLayout_ExternalBrokerIsNotChecked(t *testing.T) {
	// A broker with no local store directory must not be probed for one.
	st := fake.New()
	s, logs := layoutServer(t, st, identityBroker{identity: "external:SQI@c1"})
	if err := s.reconcileBrokerLayout(t.Context()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "abandoned") {
		t.Fatalf("unexpected leftover log:\n%s", logs.String())
	}
}

// TestLegacyGlobalStreamsDir_MatchesNatsServer pins the path the leftover check
// looks at against where nats-server puts the global account's streams.
func TestLegacyGlobalStreamsDir_MatchesNatsServer(t *testing.T) {
	dir := t.TempDir()
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: dir, NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats-server did not become ready")
	}
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateStream(t.Context(), jetstream.StreamConfig{
		Name: "LEGACY", Subjects: []string{"legacy.>"}, Storage: jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("create stream in $G: %v", err)
	}

	want := legacyGlobalStreamsDir(dir)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("legacyGlobalStreamsDir(%q) = %q does not exist after a stream was created in $G: %v", dir, want, err)
	}
}
