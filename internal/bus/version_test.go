// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"strings"
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

func TestCheckBrokerVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		wantErr bool
	}{
		{"floor constant", MinBrokerVersion, false},
		{"embedded", natsserver.VERSION, false},
		{"2.14.7", "2.14.7", true},
		{"2.14.8", "2.14.8", false},
		{"2.14.9", "2.14.9", false},
		{"2.15.0", "2.15.0", true},
		{"2.15.1", "2.15.1", false},
		{"2.16.0", "2.16.0", false},
		{"2.13.99", "2.13.99", true},
		{"1.99.99", "1.99.99", true},
		{"3.0.0", "3.0.0", false},
		{"release candidate above minimum", "2.15.3-RC.1", false},
		{"release candidate below minimum", "2.15.0-RC.1", true},
		{"leading v", "v2.15.2", false},
		{"empty", "", true},
		{"garbage", "garbage", true},
		{"two parts", "2.14", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckBrokerVersion(tt.version)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckBrokerVersion(%q) err = %v, wantErr %v", tt.version, err, tt.wantErr)
			}
		})
	}
}

func TestCheckBrokerVersion_ErrorNamesFoundAndSeriesMinimum(t *testing.T) {
	tests := []struct{ found, minimum string }{
		{"2.15.0", "2.15.1"},
		{"2.14.7", "2.14.8"},
		{"2.13.99", MinBrokerVersion},
		{"1.99.99", MinBrokerVersion},
	}
	for _, tt := range tests {
		t.Run(tt.found, func(t *testing.T) {
			err := CheckBrokerVersion(tt.found)
			if err == nil {
				t.Fatal("want error")
			}
			for _, want := range []string{tt.found, tt.minimum} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}
