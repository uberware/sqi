// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"testing"
	"time"
)

func TestParity_BrokerIdentity(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			got, err := st.GetBrokerIdentity(ctx)
			if err != nil || got != "" {
				t.Fatalf("fresh store: GetBrokerIdentity = (%q, %v), want (\"\", nil)", got, err)
			}

			at := time.Now().UTC()
			for _, want := range []string{"embedded:SQI", "external:SQI@c1", "embedded:SQI"} {
				if err := st.SetBrokerIdentity(ctx, want, at); err != nil {
					t.Fatalf("SetBrokerIdentity(%q): %v", want, err)
				}
				got, err := st.GetBrokerIdentity(ctx)
				if err != nil || got != want {
					t.Fatalf("after Set(%q): GetBrokerIdentity = (%q, %v)", want, got, err)
				}
				at = at.Add(time.Second)
			}
		})
	}
}
