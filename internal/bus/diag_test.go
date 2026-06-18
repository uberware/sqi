// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestWorkerDiagSubject(t *testing.T) {
	if got := WorkerDiagSubject("w1"); got != "worker.diag.w1" {
		t.Fatalf("WorkerDiagSubject = %q", got)
	}
}

func TestClient_PublishSubscribeWorkerDiag(t *testing.T) {
	b := startBroker(t)
	client := newClient(t, b)

	var (
		mu      sync.Mutex
		gotSubj string
		gotData []byte
	)
	sub, err := client.SubscribeWorkerDiag(func(subject string, data []byte) {
		mu.Lock()
		gotSubj, gotData = subject, data
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	if err := client.PublishWorkerDiag(context.Background(), "w1", []byte(`{"msg":"hi"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		done := gotData != nil
		mu.Unlock()
		if done {
			break
		}
		select {
		case <-deadline:
			t.Fatal("did not receive diag message")
		case <-time.After(10 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if gotSubj != "worker.diag.w1" || string(gotData) != `{"msg":"hi"}` {
		t.Fatalf("got subj=%q data=%q", gotSubj, gotData)
	}
}
