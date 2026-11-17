// Command bindle-migrate prepares Bindle's storage. Idempotent; run it on
// every deploy before ingest and the API. Each part runs when configured:
//
// OpenSearch (traces): the retention policy, index template, rollup index,
// and the least-privilege writer and reader users.
//
//	BINDLE_SEARCH_URL               OpenSearch endpoint (https; http only with BINDLE_SEARCH_ALLOW_HTTP=1)
//	BINDLE_SEARCH_ADMIN_USER        admin user (security plugin admin, or the AWS FGAC master user)
//	BINDLE_SEARCH_ADMIN_PASSWORD
//	BINDLE_SEARCH_WRITER_PASSWORD   sets the bindle_writer password (used by bindle-ingest)
//	BINDLE_SEARCH_READER_PASSWORD   sets the bindle_reader password (used by bindle-api)
//	BINDLE_SEARCH_PREFIX            index name prefix (default bindle)
//	BINDLE_SEARCH_REPLICAS          replicas per shard (default 1; 0 for a single-node dev cluster)
//	BINDLE_RETENTION_DAYS           default 30
//
// PostgreSQL (control plane): roles and schema for tenants, users and keys.
//
//	BINDLE_CONTROL_DB_ADMIN_URL     schema-owner URL (or PGHOST, PGUSER, ... env vars)
//	BINDLE_CONTROL_API_PASSWORD     sets the bindle_api role's password
//	BINDLE_CONTROL_INGEST_PASSWORD  sets the bindle_ingest role's password
//	BINDLE_CONTROL_PROCESSOR_PASSWORD  sets the bindle_processor role's password (shard leases)
//	BINDLE_DEV_SEED_TENANT / BINDLE_DEV_SEED_KEY
//	                                local development only: make this key valid for this tenant
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/bindle-ai/bindle/services/internal/control"
	"github.com/bindle-ai/bindle/services/internal/search"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("setup failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ran := false
	if os.Getenv("BINDLE_SEARCH_URL") != "" {
		if err := setupSearch(ctx, log); err != nil {
			return err
		}
		ran = true
	}
	db, err := control.OpenFromEnv("BINDLE_CONTROL_DB_ADMIN_URL")
	if err != nil {
		return err
	}
	if db != nil {
		defer db.Close()
		if err := setupControl(ctx, log, db); err != nil {
			return err
		}
		ran = true
	}
	if !ran {
		return errors.New("nothing to do: set BINDLE_SEARCH_URL and/or BINDLE_CONTROL_DB_ADMIN_URL (or PGHOST)")
	}
	return nil
}

func setupControl(ctx context.Context, log *slog.Logger, db *sql.DB) error {
	var err error
	for { // the database may still be starting
		if err = db.PingContext(ctx); err == nil || ctx.Err() != nil {
			break
		}
		log.Info("waiting for PostgreSQL", "err", err)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		return err
	}
	roles := control.DefaultRoles
	roles.APIPassword = os.Getenv("BINDLE_CONTROL_API_PASSWORD")
	roles.IngestPassword = os.Getenv("BINDLE_CONTROL_INGEST_PASSWORD")
	roles.ProcessorPassword = os.Getenv("BINDLE_CONTROL_PROCESSOR_PASSWORD")
	applied, err := control.Migrate(ctx, db, roles)
	if err != nil {
		return err
	}
	log.Info("control plane migrations complete", "applied", applied)
	tenant, key := os.Getenv("BINDLE_DEV_SEED_TENANT"), os.Getenv("BINDLE_DEV_SEED_KEY")
	if tenant != "" && key != "" {
		if err := control.SeedKey(ctx, db, tenant, "Local dev", key); err != nil {
			return err
		}
		log.Warn("seeded a development ingest key", "tenant", tenant)
	}
	return nil
}

func setupSearch(ctx context.Context, log *slog.Logger) error {
	client, err := search.FromEnv(os.Getenv, os.Getenv("BINDLE_SEARCH_ADMIN_USER"), os.Getenv("BINDLE_SEARCH_ADMIN_PASSWORD"))
	if err != nil {
		return err
	}
	// The cluster may still be starting.
	for {
		err = client.Do(ctx, http.MethodGet, "/_cluster/health", nil, nil, nil)
		if err == nil || ctx.Err() != nil {
			break
		}
		log.Info("waiting for OpenSearch", "err", err)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		return err
	}
	users := search.DefaultUsers
	users.WriterPassword = os.Getenv("BINDLE_SEARCH_WRITER_PASSWORD")
	users.ReaderPassword = os.Getenv("BINDLE_SEARCH_READER_PASSWORD")
	opts := search.SetupOptions{
		Names:         search.Names{Prefix: os.Getenv("BINDLE_SEARCH_PREFIX")},
		Users:         users,
		RetentionDays: envInt("BINDLE_RETENTION_DAYS", 30),
		Replicas:      envInt("BINDLE_SEARCH_REPLICAS", 1),
	}
	if err := search.Setup(ctx, client, opts); err != nil {
		return err
	}
	log.Info("OpenSearch setup complete", "writer", users.Writer, "reader", users.Reader, "replicas", opts.Replicas)
	return nil
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}
