// Package control is Bindle's control plane in PostgreSQL: tenants, users,
// memberships and ingest API keys.
//
// Access model:
//   - The migrating role (e.g. the RDS master user) owns the schema.
//   - The API role (bindle-api) manages users, tenants and keys. It cannot
//     delete anything, and sees only the keys of the tenant set in its
//     transaction (row-level security).
//   - The ingest role (bindle-ingest) can only call control.resolve_key and
//     control.touch_keys; it has no table access.
package control

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Roles names the app roles and their passwords. An empty password creates
// or leaves the role without a password (NOLOGIN for new roles).
type Roles struct {
	API, Ingest, Processor, Alerts                                 string
	APIPassword, IngestPassword, ProcessorPassword, AlertsPassword string
}

// DefaultRoles are the role names used when none are configured.
var DefaultRoles = Roles{API: "bindle_api", Ingest: "bindle_ingest", Processor: "bindle_processor", Alerts: "bindle_alerts"}

var roleNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// migrationLockKey serializes concurrent bindle-migrate runs.
const migrationLockKey = 7447_0002

// Migrate creates the app roles and applies pending migrations, in one
// transaction under an advisory lock.
// db must be connected as the schema owner.
func Migrate(ctx context.Context, db *sql.DB, roles Roles) (applied []string, err error) {
	for _, r := range []string{roles.API, roles.Ingest, roles.Processor, roles.Alerts} {
		if !roleNameRe.MatchString(r) {
			return nil, fmt.Errorf("control: invalid role name %q", r)
		}
	}
	seen := map[string]bool{}
	for _, r := range []string{roles.API, roles.Ingest, roles.Processor, roles.Alerts} {
		if seen[r] {
			return nil, fmt.Errorf("control: app roles must all differ")
		}
		seen[r] = true
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		return nil, err
	}
	if err = ensureRole(ctx, tx, roles.API, roles.APIPassword); err != nil {
		return nil, err
	}
	if err = ensureRole(ctx, tx, roles.Ingest, roles.IngestPassword); err != nil {
		return nil, err
	}
	if err = ensureRole(ctx, tx, roles.Processor, roles.ProcessorPassword); err != nil {
		return nil, err
	}
	if err = ensureRole(ctx, tx, roles.Alerts, roles.AlertsPassword); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS control;
		CREATE TABLE IF NOT EXISTS control.schema_migrations (
			version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}

	files, err := migrationFiles()
	if err != nil {
		return nil, err
	}
	for _, name := range files {
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM control.schema_migrations WHERE version = $1)`, name).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			continue
		}
		body, rerr := migrationFS.ReadFile("migrations/" + name)
		if rerr != nil {
			return nil, rerr
		}
		stmt := renderMigration(string(body), roles)
		if _, err = tx.ExecContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf("control: migration %s: %w", name, err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO control.schema_migrations (version) VALUES ($1)`, name); err != nil {
			return nil, err
		}
		applied = append(applied, name)
	}

	return applied, tx.Commit()
}

func migrationFiles() ([]string, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// renderMigration substitutes role placeholders with quoted identifiers.
func renderMigration(body string, roles Roles) string {
	return strings.NewReplacer(
		"{{api}}", pq.QuoteIdentifier(roles.API),
		"{{ingest}}", pq.QuoteIdentifier(roles.Ingest),
		"{{processor}}", pq.QuoteIdentifier(roles.Processor),
		"{{alerts}}", pq.QuoteIdentifier(roles.Alerts),
	).Replace(body)
}

// ensureRole creates the role if missing and sets its password if given.
// Statements are built server-side with format(%I, %L) so names and
// passwords are quoted by PostgreSQL itself.
func ensureRole(ctx context.Context, tx *sql.Tx, name, password string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, name).Scan(&exists); err != nil {
		return err
	}
	var stmt string
	var err error
	switch {
	case !exists && password != "":
		err = tx.QueryRowContext(ctx, `SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', $1::text, $2::text)`, name, password).Scan(&stmt)
	case !exists:
		err = tx.QueryRowContext(ctx, `SELECT format('CREATE ROLE %I NOLOGIN', $1::text)`, name).Scan(&stmt)
	case password != "":
		err = tx.QueryRowContext(ctx, `SELECT format('ALTER ROLE %I LOGIN PASSWORD %L', $1::text, $2::text)`, name, password).Scan(&stmt)
	default:
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, stmt)
	return err
}
