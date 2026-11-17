// Package pg stores and queries Bindle data in PostgreSQL.
//
// Access model:
//   - The migrating role (e.g. the RDS master user) owns the schema.
//   - The writer role (bindle-ingest) inserts steps and upserts rollups.
//   - The reader role (bindle-api) can only SELECT.
//
// Every app transaction sets bindle.tenant, and row-level security limits
// it to that tenant's rows, on top of the tenant_id filter in every query.
package pg

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Roles names the app roles and their passwords. An empty password creates
// or leaves the role without a password (NOLOGIN for new roles).
type Roles struct {
	Writer, Reader                 string
	WriterPassword, ReaderPassword string
}

// DefaultRoles are the role names used when none are configured.
var DefaultRoles = Roles{Writer: "bindle_writer", Reader: "bindle_reader"}

var roleNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// migrationLockKey serializes concurrent bindle-migrate runs.
const migrationLockKey = 7447_0001

// Migrate creates the app roles, applies pending migrations and creates the
// current step partitions, all in one transaction under an advisory lock.
// db must be connected as the schema owner.
func Migrate(ctx context.Context, db *sql.DB, roles Roles) (applied []string, err error) {
	for _, r := range []string{roles.Writer, roles.Reader} {
		if !roleNameRe.MatchString(r) {
			return nil, fmt.Errorf("pg: invalid role name %q", r)
		}
	}
	if roles.Writer == roles.Reader {
		return nil, fmt.Errorf("pg: writer and reader roles must differ")
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
	if err = ensureRole(ctx, tx, roles.Writer, roles.WriterPassword); err != nil {
		return nil, err
	}
	if err = ensureRole(ctx, tx, roles.Reader, roles.ReaderPassword); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS bindle;
		CREATE TABLE IF NOT EXISTS bindle.schema_migrations (
			version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}

	files, err := migrationFiles()
	if err != nil {
		return nil, err
	}
	for _, name := range files {
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM bindle.schema_migrations WHERE version = $1)`, name).Scan(&exists); err != nil {
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
			return nil, fmt.Errorf("pg: migration %s: %w", name, err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO bindle.schema_migrations (version) VALUES ($1)`, name); err != nil {
			return nil, err
		}
		applied = append(applied, name)
	}

	today := time.Now().UTC()
	for d := -1; d <= 2; d++ {
		if _, err = tx.ExecContext(ctx, `SELECT bindle.ensure_steps_partition($1::date)`, today.AddDate(0, 0, d).Format("2006-01-02")); err != nil {
			return nil, err
		}
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
		"{{writer}}", pq.QuoteIdentifier(roles.Writer),
		"{{reader}}", pq.QuoteIdentifier(roles.Reader),
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
