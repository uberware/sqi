// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// TestGuardBucket_ReadsTheKeyFromTheStreamLeader pins how the guard reads its
// key. A key-value read is a direct get, which any replica may answer, and a
// replica that is behind serves the key as it was: absent after it was
// created, or an older holder after a takeover. The guard's reads go to the
// stream leader instead, and still see a deleted or purged key as absent.
func TestGuardBucket_ReadsTheKeyFromTheStreamLeader(t *testing.T) {
	b := startEmbedded(t, EmbeddedConfig{})
	c, err := b.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	var leader, direct atomic.Int32
	spy := func(subj string, n *atomic.Int32) {
		sub, err := c.nc.Subscribe(subj, func(*nats.Msg) { n.Add(1) })
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sub.Unsubscribe() }) //nolint:errcheck // the connection closes at cleanup anyway
	}
	spy("$JS.API.STREAM.MSG.GET.KV_"+GuardBucket, &leader)
	spy("$JS.API.DIRECT.GET.KV_"+GuardBucket+".>", &direct)
	if err := c.nc.Flush(); err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	kv, err := guardBucket(ctx, c.js, 1, time.Now().Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	value := []byte(`{"instance_id":"self"}`)
	rev, err := kv.Create(ctx, guardKey, value)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := readGuardKey(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil || string(entry.Value()) != string(value) || entry.Revision() != rev {
		t.Fatalf("read = %v, want the key at revision %d", entry, rev)
	}

	if err := kv.Delete(ctx, guardKey); err != nil {
		t.Fatal(err)
	}
	if entry, err := readGuardKey(ctx, kv); err != nil || entry != nil {
		t.Fatalf("after a delete: read = %v, %v; want absent", entry, err)
	}
	if _, err := kv.Put(ctx, guardKey, value); err != nil {
		t.Fatal(err)
	}
	if err := kv.Purge(ctx, guardKey); err != nil {
		t.Fatal(err)
	}
	if entry, err := readGuardKey(ctx, kv); err != nil || entry != nil {
		t.Fatalf("after a purge: read = %v, %v; want absent", entry, err)
	}

	time.Sleep(50 * time.Millisecond) // the spies' deliveries
	if leader.Load() < 3 || direct.Load() != 0 {
		t.Errorf("reads answered by the stream leader = %d, by direct get = %d; want 3 or more and 0", leader.Load(), direct.Load())
	}
}

// TestGuardBucket_LeaderReadsUseTheJetStreamDomain pins the leader read's
// API subject when the JetStream context names a domain or an API prefix, as
// every other JetStream request does, and that reads through such a context
// return the key. A subscriber cannot see the difference (the server maps a
// domain's API onto its own before delivery), so the subject is checked on
// the wrapper.
func TestGuardBucket_LeaderReadsUseTheJetStreamDomain(t *testing.T) {
	const domain = "hub"
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
		JetStream: true, JetStreamDomain: domain, StoreDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.Start()
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats-server not ready within 5s")
	}
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)

	cases := []struct {
		name   string
		newJS  func() (jetstream.JetStream, error)
		prefix string
	}{
		{"default", func() (jetstream.JetStream, error) { return jetstream.New(nc) }, "$JS.API."},
		{"domain", func() (jetstream.JetStream, error) { return jetstream.NewWithDomain(nc, domain) }, "$JS.hub.API."},
		{"API prefix", func() (jetstream.JetStream, error) { return jetstream.NewWithAPIPrefix(nc, "$JS.hub.API") }, "$JS.hub.API."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			js, err := tc.newJS()
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			kv, err := guardBucket(ctx, js, 1, time.Now().Add(10*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			lr, ok := kv.(*leaderReadKV)
			if !ok {
				t.Fatalf("guardBucket returned %T, want *leaderReadKV", kv)
			}
			if lr.prefix != tc.prefix {
				t.Errorf("API prefix = %q, want %q", lr.prefix, tc.prefix)
			}
			value := []byte(`{"instance_id":"` + tc.name + `"}`)
			rev, err := kv.Put(ctx, guardKey, value)
			if err != nil {
				t.Fatal(err)
			}
			entry, err := readGuardKey(ctx, kv)
			if err != nil {
				t.Fatal(err)
			}
			if entry == nil || string(entry.Value()) != string(value) || entry.Revision() != rev {
				t.Fatalf("read = %v, want the key at revision %d", entry, rev)
			}
		})
	}
}
