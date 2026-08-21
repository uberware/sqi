// SPDX-License-Identifier: AGPL-3.0-or-later

package brokerauth_test

import (
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
)

func TestNkeyOption_SetsPublicKeyAndSignsNonce(t *testing.T) {
	seed, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}

	opt := brokerauth.NkeyOption(pub, seed)

	var o nats.Options
	if err := opt(&o); err != nil {
		t.Fatalf("apply option: %v", err)
	}
	if o.Nkey != pub {
		t.Errorf("Options.Nkey = %q, want %q", o.Nkey, pub)
	}
	if o.SignatureCB == nil {
		t.Fatal("Options.SignatureCB not set")
	}

	nonce := []byte("test-nonce")
	sig, err := o.SignatureCB(nonce)
	if err != nil {
		t.Fatalf("SignatureCB: %v", err)
	}

	kp, err := nkeys.FromPublicKey(pub)
	if err != nil {
		t.Fatalf("nkeys.FromPublicKey: %v", err)
	}
	if err := kp.Verify(nonce, sig); err != nil {
		t.Errorf("signature does not verify against the public key: %v", err)
	}
}
