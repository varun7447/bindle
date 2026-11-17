// Command bindle-migrate prepares OpenSearch for Bindle: the retention
// policy, index template, rollup index, and the least-privilege writer and
// reader users. Idempotent; run it on every deploy before ingest and the API.
//
// Environment:
//
//	BINDLE_SEARCH_URL               OpenSearch endpoint (https; http only with BINDLE_SEARCH_ALLOW_HTTP=1)
//	BINDLE_SEARCH_ADMIN_USER        admin user (security plugin admin, or the AWS FGAC master user)
//	BINDLE_SEARCH_ADMIN_PASSWORD
//	BINDLE_SEARCH_WRITER_PASSWORD   sets the bindle_writer password (used by bindle-ingest)
//	BINDLE_SEARCH_READER_PASSWORD   sets the bindle_reader password (used by bindle-api)
//	BINDLE_SEARCH_PREFIX            index name prefix (default bindle)
//	BINDLE_SEARCH_REPLICAS          replicas per shard (default 1; 0 for a single-node dev cluster)
//	BINDLE_RETENTION_DAYS           default 30
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

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
	client, err := search.FromEnv(os.Getenv, os.Getenv("BINDLE_SEARCH_ADMIN_USER"), os.Getenv("BINDLE_SEARCH_ADMIN_PASSWORD"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
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
