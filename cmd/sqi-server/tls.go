// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
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
)

func init() {
	tlsInitCmd.Flags().StringVar(&tlsInitOut, "out", "./certs",
		"directory to write certificate material into")
	tlsInitCmd.Flags().StringArrayVar(&tlsInitHosts, "host", nil,
		"hostname or IP the server certificate must cover (repeatable; defaults to this machine's hostname plus localhost, 127.0.0.1 and ::1)")
	tlsInitCmd.Flags().StringArrayVar(&tlsInitClients, "client", nil,
		"worker ID to issue a client certificate for, used only with nats.tls.client_ca_file (repeatable)")
	tlsCmd.AddCommand(tlsInitCmd)
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

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Wrote certificate material to %s\n", tlsInitOut)
	fmt.Fprintf(out, "  SANs: %v\n\n", hosts)
	fmt.Fprintf(out, "On sqi-server:\n")
	fmt.Fprintf(out, "  http.tls.enabled:    true\n")
	fmt.Fprintf(out, "  http.tls.cert_file:  %s/server.crt\n", tlsInitOut)
	fmt.Fprintf(out, "  http.tls.key_file:   %s/server.key\n", tlsInitOut)
	fmt.Fprintf(out, "  nats.tls.enabled:    true\n")
	fmt.Fprintf(out, "  nats.tls.cert_file:  %s/server.crt\n", tlsInitOut)
	fmt.Fprintf(out, "  nats.tls.key_file:   %s/server.key\n\n", tlsInitOut)
	fmt.Fprintf(out, "On each sqi-worker (copy ca.crt over first):\n")
	fmt.Fprintf(out, "  nats.tls_ca_file:         /path/to/ca.crt\n")
	fmt.Fprintf(out, "  nats.server_tls_ca_file:  /path/to/ca.crt\n")
	if len(tlsInitClients) > 0 {
		fmt.Fprintf(out, "\nFor client-certificate (mTLS) workers, also set on the server:\n")
		fmt.Fprintf(out, "  nats.tls.client_ca_file: %s/ca.crt\n", tlsInitOut)
		fmt.Fprintf(out, "and give each worker its own client-<id>.crt / .key as\n")
		fmt.Fprintf(out, "  nats.tls_cert_file / nats.tls_key_file\n")
	}
	fmt.Fprintf(out, "\nEnabling TLS is a coordinated restart, not a rolling one: distribute\n")
	fmt.Fprintf(out, "ca.crt and stage worker config BEFORE flipping the server. See docs/tls.md.\n")
	return nil
}
