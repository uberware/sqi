// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// leaderReadKV is the guard bucket with Get answered by the bucket's stream
// leader. A key-value Get is a direct get, which any replica may answer, and
// on a replicated bucket a replica that is behind serves the key as it was:
// absent just after it was created, or the previous holder after a takeover.
// Every decision the guard makes from a read (whether a standby may join, who
// holds the key, whether this server lost it) needs the key as it is. Writes
// go to the leader already.
type leaderReadKV struct {
	jetstream.KeyValue

	nc     *nats.Conn
	prefix string // the JetStream API subject prefix, ending in "."
}

// leaderReads wraps kv, opened from js, so its Get reads from the leader.
func leaderReads(js jetstream.JetStream, kv jetstream.KeyValue) jetstream.KeyValue {
	o := js.Options()
	prefix := o.APIPrefix
	switch {
	case o.Domain != "":
		prefix = "$JS." + o.Domain + ".API."
	case prefix == "":
		prefix = jetstream.DefaultAPIPrefix
	}
	if !strings.HasSuffix(prefix, ".") {
		prefix += "."
	}
	return &leaderReadKV{KeyValue: kv, nc: js.Conn(), prefix: prefix}
}

// msgGetResponse is the JetStream API's reply to a STREAM.MSG.GET request.
type msgGetResponse struct {
	Message *struct {
		Seq    uint64    `json:"seq"`
		Header []byte    `json:"hdrs"`
		Data   []byte    `json:"data"`
		Time   time.Time `json:"time"`
	} `json:"message"`
	Error *jetstream.APIError `json:"error"`
}

// Get returns key's latest value as the stream leader has it, or
// [jetstream.ErrKeyNotFound] when it has none, or was deleted, purged or
// expired.
func (k *leaderReadKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	bucket := k.Bucket()
	req, err := json.Marshal(struct {
		LastBySubject string `json:"last_by_subj"`
	}{"$KV." + bucket + "." + key})
	if err != nil {
		return nil, err
	}
	msg, err := k.nc.RequestWithContext(ctx, k.prefix+"STREAM.MSG.GET.KV_"+bucket, req)
	if err != nil {
		return nil, fmt.Errorf("bus: read %s from the stream leader: %w", key, err)
	}
	var resp msgGetResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return nil, fmt.Errorf("bus: read %s from the stream leader: %w", key, err)
	}
	switch {
	case resp.Error != nil && resp.Error.ErrorCode == jetstream.JSErrCodeMessageNotFound:
		return nil, jetstream.ErrKeyNotFound
	case resp.Error != nil:
		return nil, resp.Error
	case resp.Message == nil:
		return nil, jetstream.ErrInvalidJetStreamResponse
	}
	m := resp.Message
	if len(m.Header) > 0 {
		hdr, err := nats.DecodeHeadersMsg(m.Header)
		if err != nil {
			return nil, fmt.Errorf("bus: read %s from the stream leader: %w", key, err)
		}
		if op := hdr.Get(kvOperationHeader); op == kvOpDelete || op == kvOpPurge {
			return nil, jetstream.ErrKeyNotFound
		}
		// A marker the server writes when a key expires or is removed, on a
		// bucket that asks for them, as nats.go reads it.
		if hdr.Get(jetstream.MarkerReasonHeader) != "" {
			return nil, jetstream.ErrKeyNotFound
		}
	}
	return leaderEntry{bucket: bucket, key: key, value: m.Data, rev: m.Seq, created: m.Time}, nil
}

// The header and values a key-value delete or purge marker carries.
const (
	kvOperationHeader = "KV-Operation"
	kvOpDelete        = "DEL"
	kvOpPurge         = "PURGE"
)

// leaderEntry is a [jetstream.KeyValueEntry] read by [leaderReadKV.Get]. It
// is always a put: a delete or purge reads as no entry.
type leaderEntry struct {
	bucket, key string
	value       []byte
	rev         uint64
	created     time.Time
}

func (e leaderEntry) Bucket() string                { return e.bucket }
func (e leaderEntry) Key() string                   { return e.key }
func (e leaderEntry) Value() []byte                 { return e.value }
func (e leaderEntry) Revision() uint64              { return e.rev }
func (e leaderEntry) Created() time.Time            { return e.created }
func (leaderEntry) Delta() uint64                   { return 0 }
func (leaderEntry) Operation() jetstream.KeyValueOp { return jetstream.KeyValuePut }
