// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/brokerauth"
)

// layoutKeys is a Layout plus the data user's seed, so a test can connect as
// it.
type layoutKeys struct {
	layout   Layout
	dataSeed []byte
}

func newLayoutKeys(t *testing.T, authEnabled bool) layoutKeys {
	t.Helper()
	_, issuerPub, err := brokerauth.GenerateAccountSeed()
	if err != nil {
		t.Fatalf("issuer key: %v", err)
	}
	dataSeed, dataPub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("data key: %v", err)
	}
	_, calloutPub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("callout key: %v", err)
	}
	l := Layout{Account: DefaultAccount, DataUserPub: dataPub, CalloutUserPub: calloutPub}
	if authEnabled {
		l.AuthEnabled = true
		l.IssuerPub = issuerPub
	}
	return layoutKeys{layout: l, dataSeed: dataSeed}
}

// startLayoutServer boots a server whose accounts come from Apply and
// enables JetStream on the layout's account.
func startLayoutServer(t *testing.T, l Layout, storeDir string, tweaks ...func(*natsserver.Options)) *natsserver.Server {
	t.Helper()
	o := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  storeDir,
		NoSigs:    true,
		NoLog:     true,
	}
	if err := l.Apply(o); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, tweak := range tweaks {
		tweak(o)
	}
	ns, err := natsserver.NewServer(o)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ns.Start()
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("server not ready")
	}
	if err := l.EnableAccountJetStream(ns); err != nil {
		t.Fatalf("EnableAccountJetStream: %v", err)
	}
	return ns
}

type nkeyView struct {
	Account string
	Pub     []string
	Sub     []string
}

func nkeyViews(o *natsserver.Options) map[string]nkeyView {
	out := map[string]nkeyView{}
	for _, u := range o.Nkeys {
		v := nkeyView{}
		if u.Account != nil {
			v.Account = u.Account.Name
		}
		if p := u.Permissions; p != nil {
			if p.Publish != nil {
				v.Pub = append([]string(nil), p.Publish.Allow...)
			}
			if p.Subscribe != nil {
				v.Sub = append([]string(nil), p.Subscribe.Allow...)
			}
		}
		out[u.Nkey] = v
	}
	return out
}

func accountNames(o *natsserver.Options) []string {
	var names []string
	for _, a := range o.Accounts {
		names = append(names, a.Name)
	}
	slices.Sort(names)
	return names
}

type layoutCase struct {
	name    string
	auth    bool
	account string
}

// layoutMatrix is auth on and off, each with the default account name and a
// custom one such as an external cluster would use.
func layoutMatrix() []layoutCase {
	var out []layoutCase
	for _, auth := range []bool{true, false} {
		for _, account := range []string{DefaultAccount, "sqi_prod-2"} {
			out = append(out, layoutCase{fmt.Sprintf("auth=%v/account=%s", auth, account), auth, account})
		}
	}
	return out
}

func TestLayout_FragmentAgreesWithOptions(t *testing.T) {
	for _, tc := range layoutMatrix() {
		authEnabled := tc.auth
		t.Run(tc.name, func(t *testing.T) {
			k := newLayoutKeys(t, authEnabled)
			l := k.layout
			l.Account = tc.account

			want := &natsserver.Options{}
			if err := l.Apply(want); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			conf := filepath.Join(t.TempDir(), "nats.conf")
			frag, err := l.ConfigFragment()
			if err != nil {
				t.Fatalf("ConfigFragment: %v", err)
			}
			text := frag + "\nport: -1\njetstream {store_dir: " + fmt.Sprintf("%q", t.TempDir()) + "}\n"
			if err := os.WriteFile(conf, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := natsserver.ProcessConfigFile(conf)
			if err != nil {
				t.Fatalf("fragment does not parse: %v\n%s", err, text)
			}

			// The fragment also declares the system account, which Apply
			// leaves to nats-server's default.
			gotNames := accountNames(got)
			if !slices.Contains(gotNames, "SYS") || got.SystemAccount != "SYS" {
				t.Errorf("fragment must declare SYS as the system account; accounts=%v system=%q",
					gotNames, got.SystemAccount)
			}
			gotNames = slices.DeleteFunc(gotNames, func(n string) bool { return n == "SYS" })
			if wantNames := accountNames(want); !reflect.DeepEqual(gotNames, wantNames) {
				t.Errorf("accounts: fragment %v, options %v", gotNames, wantNames)
			}

			if g, w := nkeyViews(got), nkeyViews(want); !reflect.DeepEqual(g, w) {
				t.Errorf("nkeys differ:\n fragment %+v\n options  %+v", g, w)
			}
			if len(want.Nkeys) < 1 {
				t.Fatal("Apply produced no nkeys")
			}

			if got.NoAuthUser != want.NoAuthUser {
				t.Errorf("no_auth_user: fragment %q, options %q", got.NoAuthUser, want.NoAuthUser)
			}

			switch {
			case !authEnabled:
				if got.AuthCallout != nil || want.AuthCallout != nil {
					t.Errorf("auth callout present with auth off: fragment %+v options %+v",
						got.AuthCallout, want.AuthCallout)
				}
				if want.NoAuthUser == "" {
					t.Error("auth off must map anonymous connections with no_auth_user")
				}
			case got.AuthCallout == nil || want.AuthCallout == nil:
				t.Fatalf("auth callout missing: fragment %+v options %+v", got.AuthCallout, want.AuthCallout)
			default:
				g, w := *got.AuthCallout, *want.AuthCallout
				slices.Sort(g.AuthUsers)
				slices.Sort(w.AuthUsers)
				if g.Issuer != w.Issuer || g.Account != w.Account || !reflect.DeepEqual(g.AuthUsers, w.AuthUsers) {
					t.Errorf("auth callout: fragment %+v, options %+v", g, w)
				}
				if w.Account != AccountAuth || w.Issuer != l.IssuerPub {
					t.Errorf("callout not wired to AUTH and the sqi issuer: %+v", w)
				}
				if want.NoAuthUser != "" {
					t.Errorf("no_auth_user must be unset with auth on, got %q", want.NoAuthUser)
				}
			}
		})
	}
}

func TestLayout_FragmentBootsWithJetStreamInSQI(t *testing.T) {
	k := newLayoutKeys(t, true)
	l := k.layout
	l.JetStreamMaxStore = 64 << 20

	// A harness declares its own system user; SYS must be extendable
	// without editing the accounts block sqi renders.
	sysSeed, sysPub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	frag, err := l.ConfigFragmentWithSystemUser(sysPub)
	if err != nil {
		t.Fatalf("ConfigFragmentWithSystemUser: %v", err)
	}
	text := frag + "\nlisten: 127.0.0.1:-1\njetstream {store_dir: " + fmt.Sprintf("%q", dir) + "}\n"
	conf := filepath.Join(dir, "nats.conf")
	if err := os.WriteFile(conf, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := natsserver.ProcessConfigFile(conf)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, text)
	}
	o.NoSigs, o.NoLog = true, true
	ns, err := natsserver.NewServer(o)
	if err != nil {
		t.Fatalf("NewServer: %v\n%s", err, text)
	}
	ns.Start()
	defer func() { ns.Shutdown(); ns.WaitForShutdown() }()
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("not ready")
	}

	nc, err := nats.Connect(ns.ClientURL(), nkeyOption(t, k.dataSeed, k.layout.DataUserPub))
	if err != nil {
		t.Fatalf("data user: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "S", Subjects: []string{"s.>"}}); err != nil {
		t.Fatalf("stream from the fragment's SQI account: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "jetstream", "SQI")); err != nil {
		t.Errorf("no jetstream/SQI directory: %v", err)
	}

	sys, err := nats.Connect(ns.ClientURL(), nkeyOption(t, sysSeed, sysPub))
	if err != nil {
		t.Fatalf("harness system user: %v", err)
	}
	sys.Close()
}

func TestLayout_BootsAndPlacesStreamsInSQI(t *testing.T) {
	for _, authEnabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("auth=%v", authEnabled), func(t *testing.T) {
			k := newLayoutKeys(t, authEnabled)
			l := k.layout
			l.JetStreamMaxStore = 32 << 20
			storeDir := t.TempDir()
			ns := startLayoutServer(t, l, storeDir)

			nc, err := nats.Connect("", nats.InProcessServer(ns), nkeyOption(t, k.dataSeed, l.DataUserPub))
			if err != nil {
				t.Fatalf("connect as the data user: %v", err)
			}
			defer nc.Close()
			js, err := jetstream.New(nc)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "S", Subjects: []string{"s.>"}}); err != nil {
				t.Fatalf("create stream: %v", err)
			}
			info, err := js.AccountInfo(ctx)
			if err != nil {
				t.Fatalf("AccountInfo: %v", err)
			}
			if info.Streams != 1 {
				t.Errorf("AccountInfo streams = %d, want 1", info.Streams)
			}
			if info.Limits.MaxStore != 32<<20 {
				t.Errorf("max store = %d, want %d", info.Limits.MaxStore, 32<<20)
			}

			acc, err := ns.LookupAccount(l.Account)
			if err != nil {
				t.Fatal(err)
			}
			if !acc.JetStreamEnabled() || acc.JetStreamUsage().Streams != 1 {
				t.Errorf("stream is not in %s: enabled=%v usage=%+v", l.Account, acc.JetStreamEnabled(), acc.JetStreamUsage())
			}

			if _, err := os.Stat(filepath.Join(storeDir, "jetstream", l.Account)); err != nil {
				t.Errorf("no jetstream/%s directory: %v", l.Account, err)
			}
			if _, err := os.Stat(filepath.Join(storeDir, "jetstream", "$G")); err == nil {
				t.Error("jetstream/$G exists: streams must not live in the global account")
			}

			if !authEnabled {
				// An anonymous connection is mapped into the same account.
				anon, err := nats.Connect("", nats.InProcessServer(ns))
				if err != nil {
					t.Fatalf("anonymous connect with auth off: %v", err)
				}
				defer anon.Close()
				ajs, err := jetstream.New(anon)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ajs.Stream(ctx, "S"); err != nil {
					t.Errorf("anonymous connection does not see SQI's stream: %v", err)
				}
			}
		})
	}
}

func TestLayout_NonceAlwaysIssued(t *testing.T) {
	for _, authEnabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("auth=%v", authEnabled), func(t *testing.T) {
			ns := startLayoutServer(t, newLayoutKeys(t, authEnabled).layout, t.TempDir())
			conn, err := ns.InProcessConn()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			line, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil {
				t.Fatalf("read INFO: %v", err)
			}
			var info struct {
				Nonce string `json:"nonce"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "INFO "))), &info); err != nil {
				t.Fatalf("parse INFO %q: %v", line, err)
			}
			if info.Nonce == "" {
				t.Error("INFO carries no nonce")
			}
		})
	}
}

func TestLayout_ApplyRejectsIncompleteLayouts(t *testing.T) {
	k := newLayoutKeys(t, true)
	tests := []struct {
		name string
		edit func(*Layout)
	}{
		{"no account", func(l *Layout) { l.Account = "" }},
		{"account is AUTH", func(l *Layout) { l.Account = AccountAuth }},
		{"no data user", func(l *Layout) { l.DataUserPub = "" }},
		{"bad data user", func(l *Layout) { l.DataUserPub = "nope" }},
		{"auth without issuer", func(l *Layout) { l.IssuerPub = "" }},
		{"auth without callout user", func(l *Layout) { l.CalloutUserPub = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := k.layout
			tc.edit(&l)
			if err := l.Apply(&natsserver.Options{}); err == nil {
				t.Error("Apply accepted an incomplete layout")
			}
		})
	}
}

// jetStreamLimitsOf reads the limits a running server applies to account.
func jetStreamLimitsOf(t *testing.T, ns *natsserver.Server, account string) natsserver.JetStreamAccountLimits {
	t.Helper()
	acc, err := ns.LookupAccount(account)
	if err != nil {
		t.Fatalf("look up %s: %v", account, err)
	}
	if !acc.JetStreamEnabled() {
		t.Fatalf("JetStream is not enabled on %s", account)
	}
	return acc.JetStreamUsage().Limits
}

func TestLayout_FragmentJetStreamLimitsMatchEnableAccountJetStream(t *testing.T) {
	for _, maxStore := range []int64{0, 64 << 20} {
		for _, tc := range layoutMatrix() {
			t.Run(fmt.Sprintf("%s/max_store=%d", tc.name, maxStore), func(t *testing.T) {
				k := newLayoutKeys(t, tc.auth)
				l := k.layout
				l.Account, l.JetStreamMaxStore = tc.account, maxStore

				fromOptions := jetStreamLimitsOf(t, startLayoutServer(t, l, t.TempDir()), l.Account)

				frag, err := l.ConfigFragment()
				if err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				conf := filepath.Join(dir, "nats.conf")
				text := frag + "\nlisten: 127.0.0.1:-1\njetstream {store_dir: " + fmt.Sprintf("%q", dir) + "}\n"
				if err := os.WriteFile(conf, []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
				o, err := natsserver.ProcessConfigFile(conf)
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				o.NoSigs, o.NoLog = true, true
				ns, err := natsserver.NewServer(o)
				if err != nil {
					t.Fatal(err)
				}
				ns.Start()
				defer func() { ns.Shutdown(); ns.WaitForShutdown() }()
				if !ns.ReadyForConnections(10 * time.Second) {
					t.Fatal("not ready")
				}
				fromFragment := jetStreamLimitsOf(t, ns, l.Account)

				if fromFragment != fromOptions {
					t.Errorf("JetStream limits differ: fragment %+v, options %+v", fromFragment, fromOptions)
				}
				if want := l.jetStreamLimits()[""]; fromOptions != want {
					t.Errorf("applied limits %+v, want %+v", fromOptions, want)
				}
			})
		}
	}
}

func TestLayout_ConfigFragmentRejectsBadLayouts(t *testing.T) {
	k := newLayoutKeys(t, true)
	_, sysPub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("account", func(t *testing.T) {
		for _, name := range []string{"", AccountAuth, "SYS", "$G", "SQI prod", `SQI"x`, "a.b", "a\nb", "a*", "{x}"} {
			l := k.layout
			l.Account = name
			if frag, err := l.ConfigFragment(); err == nil {
				t.Errorf("account %q rendered:\n%s", name, frag)
			}
			if err := l.Apply(&natsserver.Options{}); err == nil {
				t.Errorf("Apply accepted account %q", name)
			}
		}
	})
	t.Run("missing keys", func(t *testing.T) {
		for _, edit := range []func(*Layout){
			func(l *Layout) { l.DataUserPub = "" },
			func(l *Layout) { l.IssuerPub = "" },
			func(l *Layout) { l.CalloutUserPub = "" },
		} {
			l := k.layout
			edit(&l)
			if frag, err := l.ConfigFragment(); err == nil {
				t.Errorf("incomplete layout rendered:\n%s", frag)
			}
		}
	})
	t.Run("system user", func(t *testing.T) {
		for name, pub := range map[string]string{
			"invalid":  "not-a-key",
			"data key": k.layout.DataUserPub,
			"callout":  k.layout.CalloutUserPub,
		} {
			if frag, err := k.layout.ConfigFragmentWithSystemUser(pub); err == nil {
				t.Errorf("%s system user rendered:\n%s", name, frag)
			}
		}
		if _, err := k.layout.ConfigFragmentWithSystemUser(sysPub); err != nil {
			t.Errorf("valid system user rejected: %v", err)
		}
	})
}

func TestLayout_AuthOnRefusesAnonymousConnection(t *testing.T) {
	l := newLayoutKeys(t, true).layout
	ns := startLayoutServer(t, l, t.TempDir(), func(o *natsserver.Options) { o.AuthTimeout = 1 })

	conn, err := ns.InProcessConn()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	if _, err := r.ReadString('\n'); err != nil { // INFO
		t.Fatalf("read INFO: %v", err)
	}
	if _, err := conn.Write([]byte("CONNECT {\"verbose\":false,\"pedantic\":false}\r\nPING\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	// No responder is running: the connection must end in an error or a
	// close, never in a PONG.
	start := time.Now()
	for {
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		line, err := r.ReadString('\n')
		switch {
		case strings.HasPrefix(line, "PONG"), strings.HasPrefix(line, "+OK"):
			t.Fatalf("anonymous client authenticated: %q", line)
		case strings.HasPrefix(line, "-ERR"):
			t.Logf("refused after %s: %s", time.Since(start).Round(time.Millisecond), strings.TrimSpace(line))
			return
		case err != nil:
			if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
				t.Fatalf("connection neither refused nor closed within 5s")
			}
			t.Logf("closed after %s: %v", time.Since(start).Round(time.Millisecond), err)
			return
		}
	}
}
