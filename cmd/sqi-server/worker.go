// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/auth/jointoken"
	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// workerFlags holds the values bound to the "worker" parent command's
// persistent flags, inherited by every subcommand.
var workerFlags struct {
	DBPath string
}

// workerCmd groups worker broker-credential and enrollment subcommands.
//
// These commands open the SQLite database directly, exactly like backup and
// migrate, and never start an HTTP server or NATS broker. That is
// deliberate: broker authentication (nats.auth.enabled) is independent of
// the user-facing auth.enabled gate, and the REST enrollment endpoints only
// exist when auth.enabled is on (otherwise there would be no RBAC in front
// of them). This CLI is what makes credential minting available regardless
// — an operator with a shell on the server host is already trusted.
var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Manage worker broker credentials and enrollment",
	Long: `Manage worker broker credentials and enrollment for NATS broker
authentication (the nats.auth.* settings).

These subcommands operate directly on the SQLite database file and do not
require sqi-server to be running, and work regardless of whether
auth.enabled (the separate, user-facing auth gate) is on.

The database path defaults to store.sqlite_path from the resolved
configuration (the root -c/--config file and SQI_STORE_SQLITE_PATH), falling
back to the legacy SQI_SQLITE_PATH environment variable and then to "sqi.db".
Pass --db to override it explicitly. The database must already exist — these
subcommands never create one; run "sqi-server migrate up" first.

Subcommands:
  token issue   Issue a one-time join token for self-service enrollment.
  enroll        Directly register a worker's credential by ID and public key
                (offline — see "enroll --help").
  revoke        Revoke a worker's credential (offline — see "revoke --help").
  list          List active worker credentials.`,
}

// workerTokenCmd groups worker join-token subcommands.
var workerTokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Manage worker join tokens",
}

var workerTokenIssueFlags struct {
	TTL  time.Duration
	Name string
}

var workerTokenIssueCmd = &cobra.Command{
	Use:   "issue",
	Short: "Issue a one-time worker join token",
	Long: `Issue a new join token that lets a worker enroll itself and obtain a
broker credential (POST /api/v1/workers/enroll).

The raw token is printed to stdout exactly once, immediately after creation.
Only its SHA-256 hash is stored in the database — the raw value cannot be
recovered or displayed again. If it is lost before the worker uses it, issue
a new one; the old one still works until it expires or is used.`,
	RunE: runWorkerTokenIssue,
}

var workerEnrollFlags struct {
	WorkerID  string
	PublicKey string
	Name      string
}

var workerEnrollCmd = &cobra.Command{
	Use:   "enroll",
	Short: "Directly register a worker's broker credential",
	Long: `Register a worker's broker credential by worker ID and public key
directly, without a join token.

This is the manual path for a worker that cannot reach sqi-server's REST API
to self-enroll (an air-gapped host, or an operator who provisions credentials
by hand): run "sqi-worker keygen" on the worker host to generate a keypair,
then run this command on the server with the worker ID and public key it
prints.

A RUNNING sqi-server does not see the new credential — it reads the enrolled
set once, at startup, and this command writes the database from a separate
process with no broker handle. The worker's connection is refused until
sqi-server is restarted. To enroll against a running server instead, use the
REST API with a join token:
  POST /api/v1/workers/enroll`,
	RunE: runWorkerEnroll,
}

var workerRevokeCmd = &cobra.Command{
	Use:   "revoke WORKER_ID",
	Short: "Revoke a worker's broker credential",
	Long: `Revokes a worker credential in the database. A RUNNING sqi-server does not
see this immediately — it applies at next start. To revoke a worker on a
running server and disconnect it at once, use the REST API:
  DELETE /api/v1/workers/{id}/credential`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkerRevoke,
}

var workerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List active worker broker credentials",
	Long:  `List every worker credential that has not been revoked.`,
	RunE:  runWorkerList,
}

func init() {
	workerCmd.PersistentFlags().StringVar(
		&workerFlags.DBPath,
		"db", "sqi.db",
		"path to SQLite database file (defaults to store.sqlite_path from config, or SQI_SQLITE_PATH)",
	)

	workerTokenIssueCmd.Flags().DurationVar(
		&workerTokenIssueFlags.TTL,
		"ttl", config.DefaultConfig().NATS.Auth.JoinTokenTTL,
		fmt.Sprintf("how long the token remains valid (between %s and %s)",
			config.MinNATSAuthJoinTokenTTL, config.MaxNATSAuthJoinTokenTTL),
	)
	workerTokenIssueCmd.Flags().StringVar(
		&workerTokenIssueFlags.Name,
		"name", "",
		"optional human-readable label for this token",
	)
	workerTokenCmd.AddCommand(workerTokenIssueCmd)

	workerEnrollCmd.Flags().StringVar(&workerEnrollFlags.WorkerID, "worker-id", "", "the worker's stable ID (required)")
	workerEnrollCmd.Flags().StringVar(&workerEnrollFlags.PublicKey, "public-key", "", "the worker's nkey public key, starting with 'U' (required)")
	workerEnrollCmd.Flags().StringVar(&workerEnrollFlags.Name, "name", "", "optional human-readable label for this credential")
	for _, f := range []string{"worker-id", "public-key"} {
		if err := workerEnrollCmd.MarkFlagRequired(f); err != nil {
			panic(err)
		}
	}

	workerCmd.AddCommand(workerTokenCmd, workerEnrollCmd, workerRevokeCmd, workerListCmd)
}

// openWorkerStore resolves the database path (see [resolveDBPath]) and opens
// it without applying migrations. These commands write rows into an existing
// schema; they never create or migrate the database — a worker subcommand
// pointed at the wrong file must fail with an actionable error, not conjure
// an empty one. Run "sqi-server migrate up" first against a fresh database.
func openWorkerStore(ctx context.Context, cmd *cobra.Command) (*sqlite.Store, error) {
	dbPath, err := resolveDBPath(workerFlags.DBPath, cmd != nil && cmd.Flags().Changed("db"))
	if err != nil {
		return nil, err
	}
	if dbPath == "" {
		return nil, errors.New("database path is empty; use --db, set store.sqlite_path, or set SQI_STORE_SQLITE_PATH")
	}
	if err := requireExistingDB(dbPath); err != nil {
		return nil, err
	}

	st, err := sqlite.Open(ctx, dbPath, sqlite.Options{AutoMigrate: false})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return st, nil
}

func runWorkerTokenIssue(cmd *cobra.Command, _ []string) error {
	ttl := workerTokenIssueFlags.TTL
	if ttl < config.MinNATSAuthJoinTokenTTL || ttl > config.MaxNATSAuthJoinTokenTTL {
		return fmt.Errorf("--ttl must be between %s and %s, got %s",
			config.MinNATSAuthJoinTokenTTL, config.MaxNATSAuthJoinTokenTTL, ttl)
	}

	ctx := context.Background()
	st, err := openWorkerStore(ctx, cmd)
	if err != nil {
		return err
	}
	defer st.Close()

	token, hash, prefix, err := jointoken.Generate()
	if err != nil {
		return fmt.Errorf("generate join token: %w", err)
	}

	now := time.Now().UTC()
	_, err = st.CreateWorkerJoinToken(ctx, store.WorkerJoinToken{
		ID:        uuid.NewString(),
		TokenHash: hash,
		Prefix:    prefix,
		Name:      workerTokenIssueFlags.Name,
		ExpiresAt: now.Add(ttl),
		CreatedBy: "cli",
		CreatedAt: now,
	})
	if err != nil {
		return fmt.Errorf("store join token: %w", err)
	}

	// The warning goes to stderr and the token alone to stdout, so
	// `TOKEN=$(sqi-server worker token issue)` captures exactly the token.
	fmt.Fprintln(os.Stderr, "This token will not be shown again — store it securely now.")
	fmt.Fprintln(os.Stdout, token)
	return nil
}

func runWorkerEnroll(cmd *cobra.Command, _ []string) error {
	if err := brokerauth.ValidatePublicKey(workerEnrollFlags.PublicKey); err != nil {
		return err
	}
	// The recorded worker ID is what this credential's broker grants are
	// built from (brokerauth.WorkerPermissions), and those grants are NATS
	// subject PATTERNS — so "*" would record a credential allowed to publish
	// concrete subjects belonging to any worker on the farm, and ">" would
	// put a malformed subject into the broker's key set. MarkFlagRequired
	// does not cover this: --worker-id "" counts as supplied.
	if !brokerauth.ValidWorkerIDToken(workerEnrollFlags.WorkerID) {
		return fmt.Errorf(
			"--worker-id %q is not a valid NATS subject token: it must be non-empty and must not contain '.', whitespace, '*' or '>'",
			workerEnrollFlags.WorkerID,
		)
	}

	ctx := context.Background()
	st, err := openWorkerStore(ctx, cmd)
	if err != nil {
		return err
	}
	defer st.Close()

	_, err = st.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID:         uuid.NewString(),
		WorkerID:   workerEnrollFlags.WorkerID,
		PublicKey:  workerEnrollFlags.PublicKey,
		Name:       workerEnrollFlags.Name,
		EnrolledAt: time.Now().UTC(),
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return fmt.Errorf(
				"cannot enroll worker %q: it already has a credential, or this public key is already enrolled to another worker",
				workerEnrollFlags.WorkerID,
			)
		}
		return fmt.Errorf("store credential: %w", err)
	}

	// Symmetric with runWorkerRevoke's warning, and for the same reason: this
	// command writes the database from a process with no broker handle, and
	// the broker's authorized-key set is built once at Broker.Start. Without
	// this line an operator who enrolls against a running server sees
	// "Enrolled worker" and then a worker that exits fatally on
	// nats.ErrAuthorization, with nothing connecting the two.
	fmt.Fprintf(os.Stdout, "Enrolled worker %q. A RUNNING sqi-server will not accept this credential until it restarts;"+
		" to enroll against a running server, use POST /api/v1/workers/enroll with a join token instead.\n",
		workerEnrollFlags.WorkerID)
	return nil
}

func runWorkerRevoke(cmd *cobra.Command, args []string) error {
	workerID := args[0]

	ctx := context.Background()
	st, err := openWorkerStore(ctx, cmd)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.RevokeWorkerCredential(ctx, workerID, time.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// RevokeWorkerCredential's SQL matches only an active credential
			// (revoked_at IS NULL), so this same error covers "never
			// enrolled" and "already revoked" — say so rather than claiming
			// the worker does not exist, which may be false.
			return fmt.Errorf(
				"no active credential for worker %q — it may never have been enrolled, or its credential may already be revoked",
				workerID,
			)
		}
		return fmt.Errorf("revoke credential: %w", err)
	}

	fmt.Fprintf(os.Stdout, "Revoked credential for worker %q. This takes effect the next time sqi-server starts;"+
		" to disconnect it immediately, use DELETE /api/v1/workers/%s/credential instead.\n", workerID, workerID)
	return nil
}

func runWorkerList(cmd *cobra.Command, _ []string) error {
	ctx := context.Background()
	st, err := openWorkerStore(ctx, cmd)
	if err != nil {
		return err
	}
	defer st.Close()

	creds, err := st.ListActiveWorkerCredentials(ctx)
	if err != nil {
		return fmt.Errorf("list credentials: %w", err)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "WORKER ID\tNAME\tPUBLIC KEY\tENROLLED\tLAST SEEN")
	for _, c := range creds {
		name := c.Name
		if name == "" {
			name = "-"
		}
		lastSeen := "never"
		if c.LastSeenAt != nil {
			lastSeen = c.LastSeenAt.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			c.WorkerID, name, c.PublicKey, c.EnrolledAt.Format(time.RFC3339), lastSeen)
	}
	return w.Flush()
}
