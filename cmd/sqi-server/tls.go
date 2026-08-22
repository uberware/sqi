// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/certgen"
)

const (
	// caValidity is the farm CA's lifetime. Long, because rotating a CA means
	// redistributing trust to every worker on the farm.
	caValidity = 10 * 365 * 24 * time.Hour

	// leafValidity is the lifetime of server and client certificates.
	leafValidity = 2 * 365 * 24 * time.Hour
)

var tlsCmd = &cobra.Command{
	Use:   "tls",
	Short: "Generate and manage TLS certificate material",
}

var tlsInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate a farm CA and a server certificate",
	Long: `Generate the certificate material a farm needs to run sqi over TLS.

Writes a farm certificate authority (ca.crt, ca.key) and a server certificate
(server.crt, server.key) into the output directory. Copy ca.crt to every
worker; the private keys never leave the machine that generated them.

Refuses to overwrite an existing CA: replacing one invalidates every
certificate already issued from it.`,
	RunE: runTLSInit,
}

var (
	tlsInitOut     string
	tlsInitHosts   []string
	tlsInitClients []string

	tlsIssueOut     string
	tlsIssueHosts   []string
	tlsIssueClients []string
	tlsIssueForce   bool
)

var tlsIssueCmd = &cobra.Command{
	Use:   "issue",
	Short: "Issue certificates from an existing farm CA",
	Long: `Issue additional certificates from a farm CA that already exists.

Use this to add a worker to a farm running mutual TLS, or to rotate the server
certificate. It never touches the CA itself.

` + "`tls init`" + ` deliberately refuses to overwrite a CA, so it cannot do either
job: a second invocation fails as a whole, taking --client with it.

Existing files are never replaced without --force, because a client key that is
already deployed belongs to a running worker, and replacing it takes that worker
offline at its next restart with nothing to indicate why.`,
	RunE: runTLSIssue,
}

func init() {
	tlsInitCmd.Flags().StringVar(&tlsInitOut, "out", "./certs",
		"directory to write certificate material into")
	tlsInitCmd.Flags().StringArrayVar(&tlsInitHosts, "host", nil,
		"hostname or IP the server certificate must cover (repeatable; defaults to this machine's hostname plus localhost, 127.0.0.1 and ::1)")
	tlsInitCmd.Flags().StringArrayVar(&tlsInitClients, "client", nil,
		"worker ID to issue a client certificate for, used only with nats.tls.client_ca_file (repeatable)")
	tlsCmd.AddCommand(tlsInitCmd)

	tlsIssueCmd.Flags().StringVar(&tlsIssueOut, "out", "./certs",
		"directory holding ca.crt and ca.key, and where new material is written")
	tlsIssueCmd.Flags().StringArrayVar(&tlsIssueClients, "client", nil,
		"worker ID to issue a client certificate for (repeatable)")
	tlsIssueCmd.Flags().StringArrayVar(&tlsIssueHosts, "host", nil,
		"reissue the server certificate covering these hosts (repeatable; loopback is always added)")
	tlsIssueCmd.Flags().BoolVar(&tlsIssueForce, "force", false,
		"replace certificate files that already exist")
	tlsCmd.AddCommand(tlsIssueCmd)
}

// loadFarmCA reads the CA written by `tls init` from dir.
func loadFarmCA(dir string) (*certgen.CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("no farm CA in %s (%w); create one with `sqi-server tls init`", dir, err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return nil, fmt.Errorf("no CA key in %s (%w); it must sit beside ca.crt — `sqi-server tls init` writes both", dir, err)
	}
	return certgen.LoadCA(certPEM, keyPEM)
}

// refuseExisting reports an error when base.crt or base.key already exists and
// --force was not given.
func refuseExisting(dir, base string) error {
	if tlsIssueForce {
		return nil
	}
	for _, ext := range []string{".crt", ".key"} {
		path := filepath.Join(dir, base+ext)
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; pass --force to replace it "+
				"(a deployed key belongs to a running worker, which stops connecting once it is replaced)", path)
		}
	}
	return nil
}

func runTLSIssue(cmd *cobra.Command, _ []string) error {
	if len(tlsIssueClients) == 0 && len(tlsIssueHosts) == 0 {
		return errors.New("nothing to issue: pass --client <worker-id> and/or --host <name>")
	}

	ca, err := loadFarmCA(tlsIssueOut)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if len(tlsIssueHosts) > 0 {
		hosts := resolveTLSHosts(tlsIssueHosts)
		if err := refuseExisting(tlsIssueOut, "server"); err != nil {
			return err
		}
		leaf, err := ca.NewServerCert(hosts, leafValidity)
		if err != nil {
			return err
		}
		if err := certgen.WriteLeaf(tlsIssueOut, "server", leaf); err != nil {
			return err
		}
		fmt.Fprintf(out, "Reissued %s/server.crt covering %v\n", tlsIssueOut, hosts)
		fmt.Fprintf(out, "  The server reads certificates once, at startup — restart it to pick this up.\n\n")
	}

	for _, id := range tlsIssueClients {
		base := "client-" + id
		if err := refuseExisting(tlsIssueOut, base); err != nil {
			return err
		}
		leaf, err := ca.NewClientCert(id, leafValidity)
		if err != nil {
			return err
		}
		if err := certgen.WriteLeaf(tlsIssueOut, base, leaf); err != nil {
			return err
		}
		fmt.Fprintf(out, "Issued %s/%s.crt for worker %q\n", tlsIssueOut, base, id)
	}

	if len(tlsIssueClients) > 0 {
		fmt.Fprintf(out, "\nOn each of those workers:\n")
		fmt.Fprintf(out, "  nats.tls_cert_file: /path/to/client-<worker-id>.crt\n")
		fmt.Fprintf(out, "  nats.tls_key_file:  /path/to/client-<worker-id>.key\n")
		fmt.Fprintf(out, "  nats.tls_ca_file:   /path/to/ca.crt\n")
		fmt.Fprintf(out, "\nThe CA is unchanged, so existing workers keep working.\n")
	}
	return nil
}

// loopbackSANs are always present on a generated server certificate.
var loopbackSANs = []string{"localhost", "127.0.0.1", "::1"}

// resolveTLSHosts returns the SAN list for the server certificate: whatever
// --host supplied, plus this machine's hostname when nothing was supplied,
// plus loopback in every case.
//
// Loopback is appended even when --host names specific hosts. An operator who
// names only the LAN host would otherwise get a certificate that cannot be
// verified from the machine itself — which breaks a local `curl https://
// localhost:8080/healthz`, any loopback health probe, and anything else that
// reaches the server by the name it actually runs under. Duplicates are
// dropped so an explicit --host localhost does not appear twice.
func resolveTLSHosts(flagHosts []string) []string {
	hosts := slices.Clone(flagHosts)
	if len(hosts) == 0 {
		if h, err := os.Hostname(); err == nil && h != "" {
			hosts = append(hosts, h)
		}
	}
	for _, l := range loopbackSANs {
		if !slices.Contains(hosts, l) {
			hosts = append(hosts, l)
		}
	}
	return hosts
}

// writeTLSMaterial generates and writes the CA, the server certificate, and
// any requested client certificates.
func writeTLSMaterial(dir string, hosts, clients []string) error {
	ca, err := certgen.NewCA("sqi farm CA", caValidity)
	if err != nil {
		return err
	}
	if err := certgen.WriteCA(dir, ca); err != nil {
		return err
	}
	server, err := ca.NewServerCert(hosts, leafValidity)
	if err != nil {
		return err
	}
	if err := certgen.WriteLeaf(dir, "server", server); err != nil {
		return err
	}
	for _, id := range clients {
		leaf, err := ca.NewClientCert(id, leafValidity)
		if err != nil {
			return err
		}
		if err := certgen.WriteLeaf(dir, "client-"+id, leaf); err != nil {
			return err
		}
	}
	return nil
}

func runTLSInit(cmd *cobra.Command, _ []string) error {
	hosts := resolveTLSHosts(tlsInitHosts)
	if err := writeTLSMaterial(tlsInitOut, hosts, tlsInitClients); err != nil {
		return err
	}

	// One template rather than a run of Fprintf calls, so the rendered layout
	// is visible in the source. TestTLSInit_PrintsConfigKeys only greps for
	// substrings, so a lost blank line would otherwise go unnoticed.
	fmt.Fprintf(cmd.OutOrStdout(), `Wrote certificate material to %[1]s
  SANs: %[2]v

On sqi-server:
  http.tls.enabled:    true
  http.tls.cert_file:  %[1]s/server.crt
  http.tls.key_file:   %[1]s/server.key
  nats.tls.enabled:    true
  nats.tls.cert_file:  %[1]s/server.crt
  nats.tls.key_file:   %[1]s/server.key

On each sqi-worker (copy ca.crt over first):
  nats.tls_ca_file:         /path/to/ca.crt
  nats.server_tls_ca_file:  /path/to/ca.crt
`, tlsInitOut, hosts)

	if len(tlsInitClients) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), `
For client-certificate (mTLS) workers, also set on the server:
  nats.tls.client_ca_file: %s/ca.crt
and give each worker its own client-<id>.crt / .key as
  nats.tls_cert_file / nats.tls_key_file
`, tlsInitOut)
	}

	fmt.Fprint(cmd.OutOrStdout(), `
To add a worker or rotate the server certificate later, use:
  sqi-server tls issue --client <worker-id>
This command will not run again in the same directory: it refuses to replace a
CA, because every certificate issued from the old one would stop verifying.

Enabling TLS is a coordinated restart, not a rolling one: distribute
ca.crt and stage worker config BEFORE flipping the server. See docs/tls.md.
`)
	return nil
}
