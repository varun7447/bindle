// Command bindle-migrate creates Bindle's PostgreSQL roles and schema.
// Run it once per deploy before starting ingest and the API.
//
// Environment:
//
//	BINDLE_DB_ADMIN_URL         URL of the schema-owner role (e.g. the RDS master user)
//	BINDLE_DB_WRITER_ROLE       default bindle_writer (used by bindle-ingest)
//	BINDLE_DB_READER_ROLE       default bindle_reader (used by bindle-api)
//	BINDLE_DB_WRITER_PASSWORD   sets the writer's password (optional)
//	BINDLE_DB_READER_PASSWORD   sets the reader's password (optional)
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/bindle-ai/bindle/services/internal/pg"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("migration failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	dsn := os.Getenv("BINDLE_DB_ADMIN_URL")
	if dsn == "" {
		return errors.New("BINDLE_DB_ADMIN_URL is required")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	roles := pg.DefaultRoles
	if v := os.Getenv("BINDLE_DB_WRITER_ROLE"); v != "" {
		roles.Writer = v
	}
	if v := os.Getenv("BINDLE_DB_READER_ROLE"); v != "" {
		roles.Reader = v
	}
	roles.WriterPassword = os.Getenv("BINDLE_DB_WRITER_PASSWORD")
	roles.ReaderPassword = os.Getenv("BINDLE_DB_READER_PASSWORD")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	applied, err := pg.Migrate(ctx, db, roles)
	if err != nil {
		return err
	}
	log.Info("migrations complete", "applied", applied, "writer", roles.Writer, "reader", roles.Reader)
	return nil
}
