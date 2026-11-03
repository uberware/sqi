// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
)

const (
	testAccount = "SQI"
	testTTL     = time.Hour
)

// calloutRig is a Responder wired to a lookup stub, with the keys a test needs
// to forge a request the way nats-server builds one.
type calloutRig struct {
	t         *testing.T
	issuer    nkeys.KeyPair
	issuerPub string
	serverKP  nkeys.KeyPair
	serverID  string
	now       time.Time
	logs      *bytes.Buffer
	resp      *Responder
}

func newCalloutRig(t *testing.T, lookup CredentialLookup) *calloutRig {
	t.Helper()
	issuer, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	issuerPub, err := issuer.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	serverKP, err := nkeys.CreateServer()
	if err != nil {
		t.Fatal(err)
	}
	serverID, err := serverKP.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	// Anchored on the wall clock, because the jwt library stamps "iat" from it.
	now := time.Now().Truncate(time.Second)
	resp, err := NewResponder(ResponderConfig{
		Issuer:  issuer,
		Account: testAccount,
		UserTTL: testTTL,
		Lookup:  lookup,
		Now:     func() time.Time { return now },
		Logger:  slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &calloutRig{t: t, issuer: issuer, issuerPub: issuerPub, serverKP: serverKP, serverID: serverID, now: now, logs: logs, resp: resp}
}

// callerKeys are the two distinct keys of a real request: the worker's own
// nkey (which signs the nonce) and the ephemeral user key nats-server mints
// per connection, which the response must be addressed to.
type callerKeys struct {
	worker    nkeys.KeyPair
	workerPub string
	userNkey  string
}

func newCallerKeys(t *testing.T) callerKeys {
	t.Helper()
	w, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	wp, err := w.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	e, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	ep, err := e.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	return callerKeys{worker: w, workerPub: wp, userNkey: ep}
}

// request builds a well-formed, correctly signed authorization request for k.
func (g *calloutRig) request(k callerKeys, mutate func(*jwt.AuthorizationRequestClaims)) *jwt.AuthorizationRequestClaims {
	g.t.Helper()
	const nonce = "nonce-0123456789"
	sig, err := k.worker.Sign([]byte(nonce))
	if err != nil {
		g.t.Fatal(err)
	}
	req := jwt.NewAuthorizationRequestClaims(g.issuerPub)
	req.Audience = "nats-authorization-request"
	req.Expires = time.Now().Add(time.Minute).Unix()
	req.UserNkey = k.userNkey
	req.Server = jwt.ServerID{Name: "n1", ID: g.serverID}
	req.ClientInformation.Nonce = nonce
	req.ConnectOptions.Nkey = k.workerPub
	req.ConnectOptions.SignedNonce = base64.RawURLEncoding.EncodeToString(sig)
	if mutate != nil {
		mutate(req)
	}
	return req
}

func (g *calloutRig) encode(req *jwt.AuthorizationRequestClaims) []byte {
	g.t.Helper()
	tok, err := req.Encode(g.serverKP)
	if err != nil {
		g.t.Fatal(err)
	}
	return []byte(tok)
}

// handle runs the request through Handle and decodes the response, which must
// verify against the issuer key.
func (g *calloutRig) handle(req *jwt.AuthorizationRequestClaims) *jwt.AuthorizationResponseClaims {
	g.t.Helper()
	out := g.resp.Handle(context.Background(), g.encode(req))
	if len(out) == 0 {
		g.t.Fatal("Handle returned an empty response")
	}
	cr, err := jwt.DecodeAuthorizationResponseClaims(string(out))
	if err != nil {
		g.t.Fatalf("response does not decode: %v", err)
	}
	if cr.Issuer != g.issuerPub {
		g.t.Fatalf("response issuer = %q, want %q", cr.Issuer, g.issuerPub)
	}
	vr := jwt.CreateValidationResults()
	cr.Validate(vr)
	if len(vr.Issues) > 0 {
		g.t.Fatalf("response fails the validation nats-server applies: %v", vr.Issues[0])
	}
	return cr
}

func staticLookup(workerID string) CredentialLookup {
	return func(context.Context, string) (string, error) { return workerID, nil }
}

func TestResponder_AllowsEnrolledKey(t *testing.T) {
	const workerID = "0b7f4c1e-aaaa-bbbb-cccc-1234567890ab"
	k := newCallerKeys(t)
	var asked string
	g := newCalloutRig(t, func(_ context.Context, pub string) (string, error) {
		asked = pub
		return workerID, nil
	})

	userJWT, err := g.resp.Authorize(context.Background(), g.request(k, nil))
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if asked != k.workerPub {
		t.Errorf("lookup was asked for %q, want the worker's key %q", asked, k.workerPub)
	}
	uc, err := jwt.DecodeUserClaims(userJWT)
	if err != nil {
		t.Fatal(err)
	}
	// nats-server accepts only a user JWT addressed to the ephemeral key it
	// minted for this connection ("Expected authorized user of ...").
	if uc.Subject != k.userNkey {
		t.Errorf("Subject = %q, want the request's user nkey %q", uc.Subject, k.userNkey)
	}
	if uc.Audience != testAccount {
		t.Errorf("Audience = %q, want %q", uc.Audience, testAccount)
	}
	if uc.Name != workerID {
		t.Errorf("Name = %q, want %q", uc.Name, workerID)
	}
	want := brokerauth.WorkerUserPermissions(workerID)
	if fmt.Sprint(uc.Permissions) != fmt.Sprint(want) {
		t.Errorf("Permissions = %+v, want %+v", uc.Permissions, want)
	}
	if uc.Expires != g.now.Add(testTTL).Unix() {
		t.Errorf("Expires = %d, want %d", uc.Expires, g.now.Add(testTTL).Unix())
	}
	if got := []string(uc.AllowedConnectionTypes); len(got) != 1 || got[0] != jwt.ConnectionTypeStandard {
		t.Errorf("AllowedConnectionTypes = %v, want [STANDARD]", got)
	}
	if uc.IssuerAccount != "" {
		t.Errorf("IssuerAccount = %q, want empty (non-operator mode rejects it)", uc.IssuerAccount)
	}
	if uc.Issuer != g.issuerPub {
		t.Errorf("Issuer = %q, want %q", uc.Issuer, g.issuerPub)
	}
	vr := jwt.CreateValidationResults()
	uc.Validate(vr)
	if len(vr.Issues) > 0 {
		t.Errorf("user JWT fails validation: %v", vr.Issues[0])
	}
}

func TestResponder_Denies(t *testing.T) {
	const goodID = "0b7f4c1e-aaaa-bbbb-cccc-1234567890ab"
	accountKP, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	accountPub, err := accountKP.PublicKey()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		lookup CredentialLookup
		mutate func(k callerKeys, req *jwt.AuthorizationRequestClaims)
	}{
		{
			name:   "empty nonce",
			lookup: staticLookup(goodID),
			mutate: func(_ callerKeys, req *jwt.AuthorizationRequestClaims) { req.ClientInformation.Nonce = "" },
		},
		{
			// What nats-server sends for a client that signed nothing: it
			// copies the nonce into the request only alongside a signature.
			name:   "no signature, so no nonce either",
			lookup: staticLookup(goodID),
			mutate: func(_ callerKeys, req *jwt.AuthorizationRequestClaims) {
				req.ConnectOptions.SignedNonce = ""
				req.ClientInformation.Nonce = ""
			},
		},
		{
			name:   "signature is not base64",
			lookup: staticLookup(goodID),
			mutate: func(_ callerKeys, req *jwt.AuthorizationRequestClaims) {
				req.ConnectOptions.SignedNonce = "!!not base64!!"
			},
		},
		{
			name:   "bad signature",
			lookup: staticLookup(goodID),
			mutate: func(_ callerKeys, req *jwt.AuthorizationRequestClaims) {
				req.ConnectOptions.SignedNonce = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 64))
			},
		},
		{
			name:   "signature over a different nonce",
			lookup: staticLookup(goodID),
			mutate: func(k callerKeys, req *jwt.AuthorizationRequestClaims) {
				sig, err := k.worker.Sign([]byte("some other nonce"))
				if err != nil {
					t.Fatal(err)
				}
				req.ConnectOptions.SignedNonce = base64.RawURLEncoding.EncodeToString(sig)
			},
		},
		{
			name:   "no nkey",
			lookup: staticLookup(goodID),
			mutate: func(_ callerKeys, req *jwt.AuthorizationRequestClaims) { req.ConnectOptions.Nkey = "" },
		},
		{
			name:   "invalid nkey",
			lookup: staticLookup(goodID),
			mutate: func(_ callerKeys, req *jwt.AuthorizationRequestClaims) { req.ConnectOptions.Nkey = "UNOTAKEY" },
		},
		{
			name:   "account key presented as the user key",
			lookup: staticLookup(goodID),
			mutate: func(_ callerKeys, req *jwt.AuthorizationRequestClaims) {
				sig, err := accountKP.Sign([]byte(req.ClientInformation.Nonce))
				if err != nil {
					t.Fatal(err)
				}
				req.ConnectOptions.Nkey = accountPub
				req.ConnectOptions.SignedNonce = base64.RawURLEncoding.EncodeToString(sig)
			},
		},
		{
			name: "unknown credential",
			lookup: func(context.Context, string) (string, error) {
				return "", fmt.Errorf("lookup: %w", ErrUnknownCredential)
			},
		},
		{
			name: "store error",
			lookup: func(context.Context, string) (string, error) {
				return goodID, errors.New("database is locked")
			},
		},
		{name: "worker id is a wildcard", lookup: staticLookup("*")},
		{name: "worker id is the full wildcard", lookup: staticLookup(">")},
		{name: "worker id is empty", lookup: staticLookup("")},
		{name: "worker id has a dot", lookup: staticLookup("a.b")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newCalloutRig(t, tc.lookup)
			k := newCallerKeys(t)
			req := g.request(k, func(r *jwt.AuthorizationRequestClaims) {
				if tc.mutate != nil {
					tc.mutate(k, r)
				}
			})

			if userJWT, err := g.resp.Authorize(context.Background(), req); err == nil || userJWT != "" {
				t.Fatalf("Authorize = (%q, %v), want an error and no JWT", userJWT, err)
			}

			cr := g.handle(req)
			if cr.Error == "" {
				t.Error("response carries no Error")
			}
			if cr.Jwt != "" {
				t.Error("a denied request still carries a user JWT")
			}
			if cr.Subject != k.userNkey {
				t.Errorf("response Subject = %q, want the request's user nkey", cr.Subject)
			}
			if !strings.Contains(g.logs.String(), `"impact"`) || !strings.Contains(g.logs.String(), `"remediation"`) {
				t.Errorf("denial did not log impact and remediation: %s", g.logs.String())
			}
		})
	}
}

// logRecords returns every record in the captured log.
func (g *calloutRig) logRecords() []map[string]any {
	g.t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(g.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			g.t.Fatalf("log line is not JSON: %q", line)
		}
		out = append(out, rec)
	}
	return out
}

func TestResponder_EmptyNonceLogsCause(t *testing.T) {
	t.Run("signed but no nonce is the cluster's fault", func(t *testing.T) {
		g := newCalloutRig(t, staticLookup("w1"))
		req := g.request(newCallerKeys(t), func(r *jwt.AuthorizationRequestClaims) { r.ClientInformation.Nonce = "" })

		if _, err := g.resp.Authorize(context.Background(), req); err == nil {
			t.Fatal("Authorize accepted an empty nonce")
		}

		var errorLines []map[string]any
		for _, rec := range g.logRecords() {
			if rec["level"] == "ERROR" {
				errorLines = append(errorLines, rec)
			}
		}
		if len(errorLines) != 1 {
			t.Fatalf("got %d ERROR records, want 1: %v", len(errorLines), errorLines)
		}
		if msg := fmt.Sprint(errorLines[0]["msg"]); !strings.Contains(msg, "define sqi's own users as nkey users") {
			t.Errorf("ERROR does not name the cause: %v", errorLines[0])
		}
	})

	t.Run("unsigned connect is logged as a client fault", func(t *testing.T) {
		g := newCalloutRig(t, staticLookup("w1"))
		req := g.request(newCallerKeys(t), func(r *jwt.AuthorizationRequestClaims) {
			r.ConnectOptions.SignedNonce = ""
			r.ClientInformation.Nonce = ""
		})

		if _, err := g.resp.Authorize(context.Background(), req); err == nil {
			t.Fatal("Authorize accepted an unsigned connect")
		}

		recs := g.logRecords()
		if len(recs) != 1 || recs[0]["level"] != "WARN" {
			t.Fatalf("want exactly one WARN, got %v", recs)
		}
		if msg := fmt.Sprint(recs[0]["msg"]); strings.Contains(msg, "nkey users") {
			t.Errorf("a client's missing signature was blamed on the cluster: %v", recs[0])
		}
	})
}

func TestResponder_HandleAudienceIsServerID(t *testing.T) {
	const workerID = "0b7f4c1e-aaaa-bbbb-cccc-1234567890ab"
	g := newCalloutRig(t, staticLookup(workerID))
	k := newCallerKeys(t)

	cr := g.handle(g.request(k, nil))

	if cr.Audience != g.serverID {
		t.Errorf("Audience = %q, want the request's server id %q", cr.Audience, g.serverID)
	}
	if cr.Subject != k.userNkey {
		t.Errorf("Subject = %q, want the request's user nkey %q", cr.Subject, k.userNkey)
	}
	if cr.Error != "" || cr.Jwt == "" {
		t.Fatalf("expected an allow response, got error=%q jwt=%q", cr.Error, cr.Jwt)
	}
	if cr.IssuerAccount != "" {
		t.Errorf("IssuerAccount = %q, want empty", cr.IssuerAccount)
	}
	// The embedded user JWT is what nats-server will register.
	uc, err := jwt.DecodeUserClaims(cr.Jwt)
	if err != nil {
		t.Fatal(err)
	}
	if uc.Subject != cr.Subject {
		t.Errorf("user JWT subject %q differs from the response subject %q", uc.Subject, cr.Subject)
	}
}

func TestResponder_IssuedJWTHasNoNotBefore(t *testing.T) {
	g := newCalloutRig(t, staticLookup("w1"))
	userJWT, err := g.resp.Authorize(context.Background(), g.request(newCallerKeys(t), nil))
	if err != nil {
		t.Fatal(err)
	}
	uc, err := jwt.DecodeUserClaims(userJWT)
	if err != nil {
		t.Fatal(err)
	}
	// A "nbf" would make a worker whose clock trails the broker's fail to
	// connect for the length of the skew.
	if uc.NotBefore != 0 {
		t.Errorf("NotBefore = %d, want 0", uc.NotBefore)
	}
	// The jwt library stamps "iat" from the wall clock; Now is anchored on it.
	if d := uc.IssuedAt - g.now.Unix(); d < 0 || d > 5 {
		t.Errorf("IssuedAt = %d, want within 5s after Now (%d)", uc.IssuedAt, g.now.Unix())
	}
}

// credentialRow is a row of the lookup stub: the state a worker credential is
// in after a re-enrolment, where the old key stays on file but revoked.
type credentialRow struct {
	workerID string
	revoked  bool
}

func TestResponder_ReenrolledWorker_OldKeyDeniedNewKeyAllowed(t *testing.T) {
	const workerID = "0b7f4c1e-aaaa-bbbb-cccc-1234567890ab"
	oldK, newK := newCallerKeys(t), newCallerKeys(t)
	rows := map[string]credentialRow{
		oldK.workerPub: {workerID: workerID, revoked: true},
		newK.workerPub: {workerID: workerID},
	}
	g := newCalloutRig(t, func(_ context.Context, pub string) (string, error) {
		row, ok := rows[pub]
		if !ok || row.revoked {
			return "", fmt.Errorf("key %s: %w", pub, ErrUnknownCredential)
		}
		return row.workerID, nil
	})

	if jwtStr, err := g.resp.Authorize(context.Background(), g.request(oldK, nil)); err == nil || jwtStr != "" {
		t.Errorf("revoked key: Authorize = (%q, %v), want a denial", jwtStr, err)
	}
	if cr := g.handle(g.request(oldK, nil)); cr.Error == "" || cr.Jwt != "" {
		t.Errorf("revoked key: response error=%q jwt=%q, want an error and no jwt", cr.Error, cr.Jwt)
	}

	userJWT, err := g.resp.Authorize(context.Background(), g.request(newK, nil))
	if err != nil {
		t.Fatalf("new key: %v", err)
	}
	uc, err := jwt.DecodeUserClaims(userJWT)
	if err != nil {
		t.Fatal(err)
	}
	if uc.Name != workerID {
		t.Errorf("new key: Name = %q, want %q", uc.Name, workerID)
	}
}

func TestResponder_HandleNeverAllowsUndecodableRequests(t *testing.T) {
	g := newCalloutRig(t, staticLookup("w1"))
	for name, in := range map[string][]byte{
		"nil":          nil,
		"garbage":      []byte("not a jwt"),
		"no user nkey": g.encode(g.request(newCallerKeys(t), func(r *jwt.AuthorizationRequestClaims) { r.UserNkey = "" })),
		"issuer is not the server it names": g.encode(g.request(newCallerKeys(t), func(r *jwt.AuthorizationRequestClaims) {
			other, err := nkeys.CreateServer()
			if err != nil {
				t.Fatal(err)
			}
			r.Server.ID, err = other.PublicKey()
			if err != nil {
				t.Fatal(err)
			}
		})),
		"audience is not an authorization request": g.encode(g.request(newCallerKeys(t), func(r *jwt.AuthorizationRequestClaims) {
			r.Audience = "something-else"
		})),
		"a response, not a request": func() []byte {
			cr := jwt.NewAuthorizationResponseClaims(newCallerKeys(t).userNkey)
			cr.Audience = g.serverID
			cr.Error = "x"
			tok, err := cr.Encode(g.issuer)
			if err != nil {
				t.Fatal(err)
			}
			return []byte(tok)
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			out := g.resp.Handle(context.Background(), in)
			// With no usable request there is nobody to address a response to;
			// an empty reply is what nats-server reads as "not authorized".
			if len(out) != 0 {
				t.Errorf("Handle answered %q for an undecodable request, want an empty reply", out)
			}
		})
	}
}

func TestNewResponder_RejectsBadConfig(t *testing.T) {
	issuer, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	userKP, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	pubOnly, err := issuer.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	pubOnlyKP, err := nkeys.FromPublicKey(pubOnly)
	if err != nil {
		t.Fatal(err)
	}
	ok := ResponderConfig{Issuer: issuer, Account: testAccount, UserTTL: time.Minute, Lookup: staticLookup("w")}

	tests := []struct {
		name string
		edit func(*ResponderConfig)
	}{
		{"nil issuer", func(c *ResponderConfig) { c.Issuer = nil }},
		{"user key as issuer", func(c *ResponderConfig) { c.Issuer = userKP }},
		{"public-only issuer", func(c *ResponderConfig) { c.Issuer = pubOnlyKP }},
		{"empty account", func(c *ResponderConfig) { c.Account = "" }},
		{"zero ttl", func(c *ResponderConfig) { c.UserTTL = 0 }},
		{"negative ttl", func(c *ResponderConfig) { c.UserTTL = -time.Second }},
		{"nil lookup", func(c *ResponderConfig) { c.Lookup = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ok
			tc.edit(&cfg)
			if _, err := NewResponder(cfg); err == nil {
				t.Error("NewResponder accepted the config")
			}
		})
	}
	if _, err := NewResponder(ok); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestResponder_ExpiredRequestStillAnsweredWithClockSkewWarning(t *testing.T) {
	const workerID = "0b7f4c1e-aaaa-bbbb-cccc-1234567890ab"
	g := newCalloutRig(t, staticLookup(workerID))
	k := newCallerKeys(t)

	cr := g.handle(g.request(k, func(r *jwt.AuthorizationRequestClaims) {
		r.Expires = time.Now().Add(-10 * time.Second).Unix()
	}))

	if cr.Error != "" || cr.Jwt == "" {
		t.Fatalf("an enrolled key was refused over a stale request expiry: error=%q jwt=%q", cr.Error, cr.Jwt)
	}
	var warns int
	for _, rec := range g.logRecords() {
		if rec["level"] != "WARN" {
			continue
		}
		warns++
		if msg := fmt.Sprint(rec["msg"]); !strings.Contains(msg, "clock skew") {
			t.Errorf("WARN does not name clock skew: %v", rec)
		}
	}
	if warns != 1 {
		t.Errorf("got %d WARN records, want 1", warns)
	}

	// An unenrolled key is still refused: skew tolerance is not a bypass.
	g2 := newCalloutRig(t, func(context.Context, string) (string, error) { return "", ErrUnknownCredential })
	cr = g2.handle(g2.request(k, func(r *jwt.AuthorizationRequestClaims) {
		r.Expires = time.Now().Add(-10 * time.Second).Unix()
	}))
	if cr.Jwt != "" || cr.Error == "" {
		t.Errorf("stale request for an unknown key: error=%q jwt=%q, want a refusal", cr.Error, cr.Jwt)
	}
}

func TestResponder_RequestBudget(t *testing.T) {
	g := newCalloutRig(t, staticLookup("w1"))
	at := func(d time.Duration) *jwt.AuthorizationRequestClaims {
		return &jwt.AuthorizationRequestClaims{ClaimsData: jwt.ClaimsData{Expires: g.now.Add(d).Unix()}}
	}
	tests := []struct {
		name string
		req  *jwt.AuthorizationRequestClaims
		want time.Duration
	}{
		{"missing expiry gets the floor", &jwt.AuthorizationRequestClaims{}, calloutMinTimeout},
		{"past expiry gets the floor", at(-time.Minute), calloutMinTimeout},
		{"near expiry gets the floor", at(300 * time.Millisecond), calloutMinTimeout},
		{"expiry inside the window is used", at(3 * time.Second), 3 * time.Second},
		{"far expiry is capped", at(time.Hour), calloutMaxTimeout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.resp.requestBudget(tc.req); got != tc.want {
				t.Errorf("requestBudget = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestResponder_AcceptsStdBase64Signature(t *testing.T) {
	g := newCalloutRig(t, staticLookup("w1"))
	k := newCallerKeys(t)
	req := g.request(k, nil)
	sig, err := k.worker.Sign([]byte(req.ClientInformation.Nonce))
	if err != nil {
		t.Fatal(err)
	}
	std := base64.StdEncoding.EncodeToString(sig)
	if !strings.HasSuffix(std, "=") {
		t.Fatalf("test needs a padded encoding to exercise the fallback, got %q", std)
	}
	req.ConnectOptions.SignedNonce = std

	if _, err := g.resp.Authorize(context.Background(), req); err != nil {
		t.Errorf("a standard-base64 signature was refused: %v", err)
	}
}

func TestResponder_HandleRecoversFromPanic(t *testing.T) {
	g := newCalloutRig(t, func(context.Context, string) (string, error) { panic("store exploded") })

	out := g.resp.Handle(context.Background(), g.encode(g.request(newCallerKeys(t), nil)))

	if len(out) != 0 {
		t.Errorf("Handle answered %q after a panic, want an empty (refusing) reply", out)
	}
	found := false
	for _, rec := range g.logRecords() {
		if rec["level"] == "ERROR" && strings.Contains(fmt.Sprint(rec["msg"]), "panicked") {
			found = true
		}
	}
	if !found {
		t.Errorf("panic was not logged: %s", g.logs.String())
	}
}

func TestResponder_ServeHandlesRequestsConcurrently(t *testing.T) {
	const (
		requests = 50
		lookup   = 100 * time.Millisecond
	)
	srv, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("test broker did not start")
	}
	t.Cleanup(srv.Shutdown)

	g := newCalloutRig(t, func(context.Context, string) (string, error) {
		time.Sleep(lookup)
		return "w1", nil
	})
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	if err := g.resp.Serve(context.Background(), nc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.resp.Stop)

	var wg sync.WaitGroup
	errs := make(chan error, requests)
	start := time.Now()
	for range requests {
		payload := g.encode(g.request(newCallerKeys(t), nil))
		wg.Go(func() {
			msg, err := nc.Request(calloutSubject, payload, 10*time.Second)
			if err != nil {
				errs <- err
				return
			}
			cr, err := jwt.DecodeAuthorizationResponseClaims(string(msg.Data))
			if err != nil {
				errs <- fmt.Errorf("undecodable response: %w", err)
				return
			}
			if cr.Jwt == "" {
				errs <- fmt.Errorf("not an allow response: error=%q", cr.Error)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if elapsed, serial := time.Since(start), requests*lookup; elapsed > serial/2 {
		t.Errorf("%d requests took %s; handled serially they would take %s", requests, elapsed, serial)
	}
}

// TestResponder_DenialTruncatesTheClientNkey checks that a denial logs only a
// short prefix of the nkey the connecting client sent. Any unauthenticated
// client can trigger one denial per connect, and the line reaches the
// diagnostics buffer and its WebSocket subscribers, so the client must not
// control its size.
func TestResponder_DenialTruncatesTheClientNkey(t *testing.T) {
	huge := "UATTACKER" + strings.Repeat("x", 64*1024)
	tests := []struct {
		name   string
		lookup CredentialLookup
		nkey   string
		want   string
	}{
		{"invalid key of any length", staticLookup("w1"), huge, "UATTACKE…"},
		{"valid key with no credential", func(context.Context, string) (string, error) { return "", ErrUnknownCredential }, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newCalloutRig(t, tt.lookup)
			k := newCallerKeys(t)
			req := g.request(k, func(r *jwt.AuthorizationRequestClaims) {
				if tt.nkey != "" {
					r.ConnectOptions.Nkey = tt.nkey
				}
			})
			want := tt.want
			if want == "" {
				want = req.ConnectOptions.Nkey[:8] + "…"
			}

			if _, err := g.resp.Authorize(context.Background(), req); err == nil {
				t.Fatal("Authorize allowed the connection")
			}
			recs := g.logRecords()
			if len(recs) != 1 {
				t.Fatalf("got %d records, want 1: %v", len(recs), recs)
			}
			if got := fmt.Sprint(recs[0]["nkey"]); got != want {
				t.Fatalf("logged nkey = %q (%d bytes), want %q", got, len(got), want)
			}
		})
	}
}
