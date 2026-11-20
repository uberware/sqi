// SPDX-License-Identifier: AGPL-3.0-or-later

package logstreamer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/logstreamer"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// startJetStream starts an in-process nats-server with JetStream and no
// streams, and returns a connection to it.
func startJetStream(t *testing.T) *nats.Conn {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
		JetStream: true, StoreDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("nats-server not ready within 5s")
	}
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// TestJetStreamTransport_DeliversOnceTheStreamExists pins that chunks the
// broker could not store are delivered once it can: here the log stream does
// not exist while the task prints, as when a stream cannot store, and every
// chunk lands exactly once after it is created. A core publish loses all five.
func TestJetStreamTransport_DeliversOnceTheStreamExists(t *testing.T) {
	nc := startJetStream(t)
	tr, err := logstreamer.NewJetStreamTransport(nc)
	if err != nil {
		t.Fatal(err)
	}
	k := fastKnobs()
	k.FlushWait = 100 * time.Millisecond
	p := logstreamer.New(tr, testWorkerID, logstreamer.WithKnobs(slowFlushCfg(1, 1<<20), k), discard())
	const n = 5
	for i := 1; i <= n; i++ {
		p.HandleLine(context.Background(), "task-js", "attempt-js", "", "stdout", fmt.Sprintf("line %d", i))
	}
	if err := p.FlushLogs(context.Background(), "task-js", "attempt-js"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     bus.StreamLogs,
		Subjects: []string{bus.SubjectTaskLogsPrefix + ".>"},
		Storage:  jetstream.FileStorage,
	})
	if err != nil {
		t.Fatal(err)
	}
	if dropped := p.Flush(ctx); dropped != 0 {
		t.Fatalf("Flush dropped %d chunks, want 0", dropped)
	}

	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != n {
		t.Fatalf("the stream holds %d chunks, want %d", info.State.Msgs, n)
	}
	seen := map[int64]int{}
	for seq := uint64(1); seq <= n; seq++ {
		m, err := stream.GetMsg(ctx, seq)
		if err != nil {
			t.Fatal(err)
		}
		var c protocol.LogChunkMsg
		if err := json.Unmarshal(m.Data, &c); err != nil {
			t.Fatal(err)
		}
		seen[c.SeqNum]++
	}
	for s := int64(1); s <= n; s++ {
		if seen[s] != 1 {
			t.Errorf("chunk seq %d stored %d times, want once (seen %v)", s, seen[s], seen)
		}
	}
}

// TestJetStreamTransport_StoresAWindowInOrder pins that chunks sent several
// at a time are stored once each and in the order they were published, which
// is the order the log viewer shows them in.
func TestJetStreamTransport_StoresAWindowInOrder(t *testing.T) {
	nc := startJetStream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     bus.StreamLogs,
		Subjects: []string{bus.SubjectTaskLogsPrefix + ".>"},
		Storage:  jetstream.FileStorage,
	})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := logstreamer.NewJetStreamTransport(nc)
	if err != nil {
		t.Fatal(err)
	}
	p := logstreamer.New(tr, testWorkerID, logstreamer.WithKnobs(slowFlushCfg(1, 1<<20), fastKnobs()), discard())
	const n = 200
	for i := 1; i <= n; i++ {
		p.HandleLine(context.Background(), "task-o", "attempt-o", "", "stdout", fmt.Sprintf("line %d", i))
	}
	if dropped := p.Flush(ctx); dropped != 0 {
		t.Fatalf("Flush dropped %d chunks, want 0", dropped)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != n {
		t.Fatalf("the stream holds %d chunks, want %d", info.State.Msgs, n)
	}
	for seq := uint64(1); seq <= n; seq++ {
		m, err := stream.GetMsg(ctx, seq)
		if err != nil {
			t.Fatal(err)
		}
		var c protocol.LogChunkMsg
		if err := json.Unmarshal(m.Data, &c); err != nil {
			t.Fatal(err)
		}
		if c.SeqNum != int64(seq) {
			t.Fatalf("stream message %d is chunk %d, want chunks stored in publish order", seq, c.SeqNum)
		}
	}
}
