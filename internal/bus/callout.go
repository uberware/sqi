// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
)

const (
	// calloutSubject is where nats-server sends authorization requests.
	calloutSubject = "$SYS.REQ.USER.AUTH"

	// calloutQueue is the queue group the responders share, so any sqi-server
	// able to reach the store can answer.
	calloutQueue = "sqi-auth"

	// calloutMaxTimeout caps one authorization, store lookup included. The
	// real deadline is the request's own expiry (the broker's auth timeout);
	// a slower answer is wasted.
	calloutMaxTimeout = 5 * time.Second

	// calloutFlushTimeout bounds the round trip that confirms the callout
	// subscription is registered on the server.
	calloutFlushTimeout = 5 * time.Second

	// calloutMinTimeout is the budget when the request carries no usable
	// expiry (missing, or already past because sqi-server's clock runs ahead
	// of the broker's).
	calloutMinTimeout = time.Second

	// calloutMaxInFlight bounds the authorizations Serve runs at once.
	calloutMaxInFlight = 16

	// denyReason is the only text a denied connection's response carries.
	// nats-server shows the client a generic "Authorization Violation"
	// regardless, so the detail stays in sqi's log.
	denyReason = "not authorized"
)

// ErrUnknownCredential is wrapped by a [CredentialLookup] when no active
// credential has the key it was asked about.
var ErrUnknownCredential = errors.New("bus: no active credential for this key")

// CredentialLookup resolves a worker's public nkey to the worker ID of its
// active credential. It wraps [ErrUnknownCredential] when there is none (the
// key was never enrolled, or its credential was revoked). Any other error is
// a failure to answer, and denies the connection exactly as an unknown key
// does.
type CredentialLookup func(ctx context.Context, publicKey string) (workerID string, err error)

// ResponderConfig configures a [Responder].
type ResponderConfig struct {
	// Issuer signs every response and user JWT. It must hold an account
	// seed; nats-server is configured to trust its public key.
	Issuer nkeys.KeyPair
	// Account is the name of the NATS account workers are placed in.
	Account string
	// UserTTL is how long an issued user JWT stays valid. nats-server
	// disconnects the connection at that point and the worker reconnects
	// through the callout again.
	UserTTL time.Duration
	// Lookup resolves a worker's key to its worker ID.
	Lookup CredentialLookup
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
	// Logger receives every denial. Nil means slog.Default.
	Logger *slog.Logger
}

// Responder answers nats-server's auth-callout requests. It is the single
// place that decides whether a worker may connect and with which
// permissions, so nothing in it may answer "allow" except through a
// successful [Responder.Authorize].
type Responder struct {
	cfg ResponderConfig
	pub string

	// inflight counts the requests Serve has accepted and not yet answered.
	inflight sync.WaitGroup
	// sub is the subscription Serve made; nil before it succeeds.
	sub *nats.Subscription
}

// NewResponder validates cfg and returns a Responder.
func NewResponder(cfg ResponderConfig) (*Responder, error) {
	if cfg.Issuer == nil {
		return nil, errors.New("bus: callout responder needs an issuer key")
	}
	pub, err := cfg.Issuer.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("bus: callout issuer key: %w", err)
	}
	if !nkeys.IsValidPublicAccountKey(pub) {
		return nil, errors.New("bus: callout issuer must be an account nkey (starts with 'A')")
	}
	if _, err := cfg.Issuer.Seed(); err != nil {
		return nil, fmt.Errorf("bus: callout issuer key has no seed to sign with: %w", err)
	}
	if cfg.Account == "" {
		return nil, errors.New("bus: callout responder needs the account to place workers in")
	}
	if cfg.UserTTL <= 0 {
		return nil, fmt.Errorf("bus: callout user ttl must be positive, got %s", cfg.UserTTL)
	}
	if cfg.Lookup == nil {
		return nil, errors.New("bus: callout responder needs a credential lookup")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Responder{cfg: cfg, pub: pub}, nil
}

// Authorize decides one request. On success it returns the signed user JWT
// to place the connection with; on any failure it returns an error and no
// JWT, after logging why.
func (r *Responder) Authorize(ctx context.Context, req *jwt.AuthorizationRequestClaims) (string, error) {
	if req == nil {
		return "", r.deny(ctx, slog.LevelError, "bus: auth callout received no request", nil, errors.New("nil request"),
			"the connection is refused", "report this as a bug; nats-server sent a request sqi could not read")
	}
	workerKey := req.ConnectOptions.Nkey
	if err := brokerauth.ValidatePublicKey(workerKey); err != nil {
		return "", r.deny(ctx, slog.LevelWarn, "bus: auth callout refused a connection whose nkey is not a valid user key",
			[]any{nkeyAttr(workerKey)}, err,
			"this connection is refused",
			"connect with a user nkey, as generated by sqi-worker keygen")
	}
	if err := r.verifyNonce(ctx, req); err != nil {
		return "", err
	}

	workerID, err := r.cfg.Lookup(ctx, workerKey)
	if err != nil {
		return "", r.denyLookup(ctx, workerKey, err)
	}
	if !brokerauth.ValidWorkerIDToken(workerID) {
		return "", r.deny(ctx, slog.LevelError, "bus: auth callout refused a worker credential whose worker id is not a valid NATS subject token",
			[]any{slog.String("worker_id", workerID), nkeyAttr(workerKey)},
			errors.New("invalid worker id"),
			"this worker cannot connect; its grants would have been subject wildcards or a malformed subject",
			"revoke the credential and re-enroll the worker with an id containing no '.', whitespace, '*' or '>'")
	}

	return r.issueUserJWT(ctx, req.UserNkey, workerID)
}

// verifyNonce checks that the client signed the nonce nats-server issued,
// with the key it claims.
func (r *Responder) verifyNonce(ctx context.Context, req *jwt.AuthorizationRequestClaims) error {
	if req.ConnectOptions.SignedNonce == "" {
		// nats-server copies the nonce into the request only when the client
		// sent a signature, so an unsigned connect arrives with an empty
		// nonce too. That is the client's fault, and anyone can cause it.
		return r.deny(ctx, slog.LevelWarn, "bus: auth callout refused a connection that did not sign the server nonce",
			[]any{nkeyAttr(req.ConnectOptions.Nkey)}, errors.New("no signature"),
			"this connection is refused",
			"connect with a worker that signs the server nonce with its nkey seed")
	}
	nonce := req.ClientInformation.Nonce
	if nonce == "" {
		// The client signed, yet the request carries no nonce to check the
		// signature against: the broker's side is misconfigured. Verifying
		// the signature over the empty string would accept a replayable one.
		return r.deny(ctx, slog.LevelError,
			"bus: the cluster issued no nonce; define sqi's own users as nkey users",
			[]any{nkeyAttr(req.ConnectOptions.Nkey)},
			errors.New("empty nonce"),
			"every worker on the farm is being turned away",
			"give the cluster at least one nkey user (sqi's own two connections), as `sqi-server nats external init` generates, so that it issues a nonce")
	}
	sig, err := decodeSignature(req.ConnectOptions.SignedNonce)
	if err != nil {
		return r.deny(ctx, slog.LevelWarn, "bus: auth callout refused a connection with an unreadable signature",
			[]any{nkeyAttr(req.ConnectOptions.Nkey)}, err,
			"this connection is refused",
			"connect with a worker that signs the server nonce with its nkey seed")
	}
	pub, err := nkeys.FromPublicKey(req.ConnectOptions.Nkey)
	if err != nil {
		return r.deny(ctx, slog.LevelWarn, "bus: auth callout refused a connection whose nkey cannot be loaded",
			[]any{nkeyAttr(req.ConnectOptions.Nkey)}, err,
			"this connection is refused",
			"connect with a user nkey, as generated by sqi-worker keygen")
	}
	if err := pub.Verify([]byte(nonce), sig); err != nil {
		return r.deny(ctx, slog.LevelWarn, "bus: auth callout refused a connection whose signature does not verify",
			[]any{nkeyAttr(req.ConnectOptions.Nkey)}, err,
			"this connection is refused; whoever it was does not hold the private key it presented",
			"check the worker's key file matches the credential it was enrolled with")
	}
	return nil
}

// decodeSignature reads the signature as nats-server does: raw URL-safe
// base64, falling back to standard base64.
func decodeSignature(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("no signature")
	}
	sig, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		sig, err = base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("signature is not base64: %w", err)
		}
	}
	return sig, nil
}

// nkeyLogPrefix is how many characters of a connecting client's nkey a log
// line keeps.
const nkeyLogPrefix = 8

// nkeyAttr is the log attribute for the nkey a connecting client presented,
// cut to its first [nkeyLogPrefix] characters. The client chooses that
// string, unauthenticated, and can make one denial per connect: a whole key
// could be any size and fill the diagnostics buffer and every WebSocket
// subscriber. A user nkey's first eight characters ("U" and seven of base32)
// still single it out among a farm's enrolled keys.
func nkeyAttr(nkey string) slog.Attr {
	runes := 0
	for i := range nkey {
		if runes == nkeyLogPrefix {
			return slog.String("nkey", strings.ToValidUTF8(nkey[:i], "?")+"…")
		}
		runes++
	}
	return slog.String("nkey", strings.ToValidUTF8(nkey, "?"))
}

func (r *Responder) denyLookup(ctx context.Context, workerKey string, err error) error {
	if errors.Is(err, ErrUnknownCredential) {
		return r.deny(ctx, slog.LevelWarn, "bus: auth callout refused a key with no active credential",
			[]any{nkeyAttr(workerKey)}, err,
			"this connection is refused",
			"enroll the worker (sqi-worker enroll), or re-enroll it if its credential was revoked")
	}
	return r.deny(ctx, slog.LevelError, "bus: auth callout could not look the credential up",
		[]any{nkeyAttr(workerKey)}, err,
		"every worker connection is refused while the credential store is unreachable; workers retry on their own",
		"restore the database connection of sqi-server")
}

// issueUserJWT signs the user JWT. subject must be the ephemeral user key in
// the request, not the worker's own key: nats-server rejects a response
// addressed to anything else.
func (r *Responder) issueUserJWT(ctx context.Context, subject, workerID string) (string, error) {
	uc := jwt.NewUserClaims(subject)
	if uc == nil {
		return "", r.deny(ctx, slog.LevelError, "bus: auth callout request carries no user nkey to address the response to",
			[]any{slog.String("worker_id", workerID)}, errors.New("empty user nkey"),
			"this connection is refused", "report this as a bug; nats-server sent a request sqi could not read")
	}
	// The placement: in non-operator mode nats-server takes the account from
	// the audience.
	uc.Audience = r.cfg.Account
	uc.Name = workerID
	uc.Expires = r.cfg.Now().Add(r.cfg.UserTTL).Unix()
	uc.Permissions = brokerauth.WorkerUserPermissions(workerID)
	// nats-server enforces this for callout users from version 2.15.2.
	uc.AllowedConnectionTypes.Add(jwt.ConnectionTypeStandard)
	// No NotBefore: a worker whose clock trails the broker's would be refused
	// for the length of the skew.

	token, err := uc.Encode(r.cfg.Issuer)
	if err != nil {
		return "", r.deny(ctx, slog.LevelError, "bus: auth callout could not sign a user JWT",
			[]any{slog.String("worker_id", workerID)}, err,
			"this connection is refused", "check the account issuer key sqi-server was started with")
	}
	return token, nil
}

// deny logs one refusal and returns the error that ends the authorization.
// The impact and remediation attributes follow the style of the broker's
// other operator-facing warnings.
func (r *Responder) deny(ctx context.Context, level slog.Level, msg string, attrs []any, cause error, impact, remediation string) error {
	attrs = append(
		attrs,
		slog.Any("error", cause),
		slog.String("impact", impact),
		slog.String("remediation", remediation),
	)
	r.cfg.Logger.Log(ctx, level, msg, attrs...)
	return fmt.Errorf("bus: auth callout denied: %w", cause)
}

// Handle answers one request as nats-server expects: a signed
// AuthorizationResponseClaims JWT, allowing with a user JWT or refusing with
// an error. It returns an empty reply, which nats-server reads as "not
// authorized", only when the request is so malformed that there is no user or
// server to address a response to.
func (r *Responder) Handle(ctx context.Context, requestJWT []byte) (reply []byte) {
	// A panic must refuse the connection, never take down the process or
	// leave the request unanswered with a half-built allow.
	defer func() {
		if p := recover(); p != nil {
			r.cfg.Logger.ErrorContext(
				ctx,
				"bus: auth callout panicked; refusing the connection",
				slog.Any("panic", p),
				slog.String("impact", "this connection is refused"),
				slog.String("remediation", "report this as a bug"),
			)
			reply = nil
		}
	}()

	req, timeIssue, err := decodeRequest(requestJWT)
	if err != nil {
		r.cfg.Logger.ErrorContext(
			ctx,
			"bus: auth callout could not read a request; answering with an empty refusal",
			slog.Any("error", err),
			slog.String("impact", "this connection is refused"),
			slog.String("remediation", "check that the auth callout is not configured with an xkey, which sqi does not support, and that the cluster and sqi-server are compatible versions"),
		)
		return nil
	}

	if timeIssue != nil {
		r.cfg.Logger.WarnContext(
			ctx,
			"bus: auth callout request is outside its validity window; processing it anyway because the clocks of sqi-server and the broker probably disagree (clock skew)",
			slog.Any("error", timeIssue),
			slog.String("impact", "none while the skew is small; a large one makes every request look stale"),
			slog.String("remediation", "synchronize the clocks of sqi-server and the NATS cluster (NTP)"),
		)
	}
	ctx, cancel := context.WithTimeout(ctx, r.requestBudget(req))
	defer cancel()

	resp := jwt.NewAuthorizationResponseClaims(req.UserNkey)
	resp.Audience = req.Server.ID
	userJWT, err := r.Authorize(ctx, req)
	if err != nil {
		resp.Error = denyReason
	} else {
		resp.Jwt = userJWT
	}
	token, err := resp.Encode(r.cfg.Issuer)
	if err != nil {
		r.cfg.Logger.ErrorContext(
			ctx,
			"bus: auth callout could not sign its response",
			slog.Any("error", err),
			slog.String("impact", "this connection is refused"),
			slog.String("remediation", "check the account issuer key sqi-server was started with"),
		)
		return nil
	}
	return []byte(token)
}

// requestBudget is how long one authorization may take: the time left until
// the request's own expiry (the broker's auth deadline), capped at
// [calloutMaxTimeout] and never under [calloutMinTimeout].
func (r *Responder) requestBudget(req *jwt.AuthorizationRequestClaims) time.Duration {
	if req.Expires <= 0 {
		return calloutMinTimeout
	}
	left := time.Unix(req.Expires, 0).Sub(r.cfg.Now())
	return min(max(left, calloutMinTimeout), calloutMaxTimeout)
}

// decodeRequest parses the request and checks it is authentic and carries
// what the response must be addressed to. A time-based validation issue (the
// request looks expired or not yet valid) is returned separately and does not
// fail the decode: it means the clocks of sqi-server and the broker disagree,
// and the request's expiry is only a couple of seconds long.
func decodeRequest(requestJWT []byte) (req *jwt.AuthorizationRequestClaims, timeIssue, err error) {
	req, err = jwt.DecodeAuthorizationRequestClaims(string(requestJWT))
	if err != nil {
		return nil, nil, err
	}
	vr := jwt.CreateValidationResults()
	req.Validate(vr)
	for _, issue := range vr.Issues {
		switch {
		case issue.Blocking:
			return nil, nil, fmt.Errorf("request fails validation: %w", issue)
		case issue.TimeCheck && timeIssue == nil:
			timeIssue = issue
		}
	}
	if !nkeys.IsValidPublicServerKey(req.Server.ID) {
		return nil, nil, errors.New("request carries no valid server id")
	}
	// Decode already proved the signature matches req.Issuer; the issuer must
	// also be the server that is asking.
	if req.Issuer != req.Server.ID {
		return nil, nil, errors.New("request is not signed by the server it names")
	}
	if req.Audience != natsserver.AuthRequestSubject {
		return nil, nil, fmt.Errorf("request audience %q is not an authorization request", req.Audience)
	}
	return req, timeIssue, nil
}

// Serve subscribes the responder to the callout subject on nc as part of the
// shared queue group and replies to every request with [Responder.Handle].
// Requests are handled concurrently, at most [calloutMaxInFlight] at a time.
// Serve returns once the subscription is registered on the server; ctx may
// carry no deadline, so that round trip is bounded by [calloutFlushTimeout].
// Call it once, and [Responder.Stop] when the connection is about to close.
func (r *Responder) Serve(ctx context.Context, nc *nats.Conn) error {
	slots := make(chan struct{}, calloutMaxInFlight)
	sub, err := nc.QueueSubscribe(calloutSubject, calloutQueue, func(m *nats.Msg) {
		slots <- struct{}{}
		r.inflight.Go(func() {
			defer func() { <-slots }()
			ctx := context.Background()
			if err := m.Respond(r.Handle(ctx, m.Data)); err != nil {
				r.cfg.Logger.ErrorContext(
					ctx,
					"bus: auth callout could not send its reply",
					slog.Any("error", err),
					slog.String("impact", "the connection that asked is refused when nats-server times out"),
					slog.String("remediation", "check the callout connection to the cluster"),
				)
			}
		})
	})
	if err != nil {
		return fmt.Errorf("bus: subscribe auth callout: %w", err)
	}
	flushCtx, cancel := context.WithTimeout(ctx, calloutFlushTimeout)
	defer cancel()
	if err := nc.FlushWithContext(flushCtx); err != nil {
		return fmt.Errorf("bus: subscribe auth callout: %w", err)
	}
	r.sub = sub
	return nil
}

// Stop stops the subscription [Responder.Serve] made from taking new
// requests and waits, for at most [calloutMaxTimeout], for the requests it
// already accepted to be answered, so a shutdown does not close the
// connection under them. It logs when some were not. Stop does nothing if
// Serve did not succeed, or on a second call.
func (r *Responder) Stop() {
	sub := r.sub
	r.sub = nil
	if sub == nil || r.drain(sub) {
		return
	}
	r.cfg.Logger.WarnContext(
		context.Background(), "bus: auth callout requests were still unanswered at shutdown",
		slog.String("impact", "the workers that were connecting are refused and retry"),
	)
}

// drain is [Responder.Stop] for sub, reporting whether every accepted request
// was answered in time.
func (r *Responder) drain(sub *nats.Subscription) bool {
	deadline := time.After(calloutMaxTimeout)

	// The subscription reports closed only after its last message handler
	// has returned, and each handler counts its request in inflight before
	// returning, so waiting for it first makes the count final.
	closed := sub.StatusChanged(nats.SubscriptionClosed)
	if err := sub.Drain(); err != nil {
		return false
	}
	select {
	case <-closed:
	case <-deadline:
		return false
	}

	answered := make(chan struct{})
	go func() {
		r.inflight.Wait()
		close(answered)
	}()
	select {
	case <-answered:
		return true
	case <-deadline:
		return false
	}
}
