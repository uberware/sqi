// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go"
)

func TestPermanentPublishError(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nats.ErrMaxPayload, true},
		{fmt.Errorf("logstreamer: publish a/1: %w", nats.ErrConnectionClosed), true},
		{nats.ErrConnectionReconnecting, false},
		{nats.ErrTimeout, false},
		{errors.New("nats: no response from stream"), false},
		{nil, false},
	}
	for _, tt := range tests {
		if got := PermanentPublishError(tt.err); got != tt.want {
			t.Errorf("PermanentPublishError(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
