// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"errors"
	"strings"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/brokerauth"
)

// grantViolationWait is how long assertWorkerGrants waits for the broker to
// report a permissions violation it expects.
const grantViolationWait = 2 * time.Second

// assertWorkerGrants checks that nc, a connection authorized as workerID,
// holds exactly [brokerauth.WorkerPermissions]: every subject the grant allows
// can be published and subscribed, and the same subjects of another worker,
// the JetStream API, every worker's status traffic, the process-global reply
// inboxes and another worker's reply inboxes cannot.
//
// It replaces nc's error handler, through which the broker reports
// violations, and must be the only thing publishing on nc while it runs.
func assertWorkerGrants(t testing.TB, nc *nats.Conn, workerID string) {
	t.Helper()
	other := workerID + "-other"
	violations := make(chan error, 64)
	nc.SetErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
		select {
		case violations <- err:
		default:
		}
	})
	flush := func() {
		t.Helper()
		if err := nc.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
	}
	// wantViolation waits for the next violation and requires it to name
	// subject. The broker reports violations in order, so had anything
	// allowed been refused before it, that violation would arrive first.
	wantViolation := func(what, subject string) {
		t.Helper()
		select {
		case err := <-violations:
			if !errors.Is(err, nats.ErrPermissionViolation) || !strings.Contains(err.Error(), `"`+subject+`"`) {
				t.Fatalf("%s %s: first violation = %v; want a permissions violation for it", what, subject, err)
			}
		case <-time.After(grantViolationWait):
			t.Fatalf("%s %s was not refused", what, subject)
		}
	}

	perms := brokerauth.WorkerPermissions(workerID)
	for _, pattern := range perms.Publish.Allow {
		subject := concreteSubject(pattern)
		if err := nc.Publish(subject, []byte("x")); err != nil {
			t.Fatalf("Publish %s: %v", subject, err)
		}
	}
	var subs []*nats.Subscription
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe() //nolint:errcheck // best-effort cleanup; refused subscriptions are already gone
		}
	}()
	subscribe := func(subject string) {
		t.Helper()
		s, err := nc.SubscribeSync(subject)
		if err != nil {
			t.Fatalf("SubscribeSync %s: %v", subject, err)
		}
		subs = append(subs, s)
	}
	for _, pattern := range perms.Subscribe.Allow {
		subscribe(pattern)
	}
	flush()

	for _, denied := range []string{
		"task.status." + other + ".j",
		"task.logs." + other + ".t",
		"worker.register." + other,
		"worker.heartbeat." + other,
		"worker.deregister." + other,
		"worker.diag." + other,
		"work.lease." + other + ".q",
		// The JetStream API: a worker publishes into streams through its
		// own subjects, never by managing them.
		"$JS.API.STREAM.INFO." + StreamLogs,
	} {
		if err := nc.Publish(denied, []byte("x")); err != nil {
			t.Fatalf("Publish %s: %v", denied, err)
		}
		flush()
		wantViolation("publish to", denied)
	}

	for _, denied := range []string{
		"task.status.>",
		"_INBOX.>",
		brokerauth.InboxPrefix(other) + ".>",
	} {
		subscribe(denied)
		flush()
		wantViolation("subscribe to", denied)
	}

	select {
	case err := <-violations:
		t.Fatalf("unexpected extra violation: %v", err)
	default:
	}
}
