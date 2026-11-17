// Command bindle-processor consumes the steps stream (Amazon Kinesis) and
// writes to OpenSearch: payload offload, step indexing, trace rollups and
// rollup retention. Shards are shared between instances with leases in the
// control plane; delivery is at-least-once and writes are idempotent.
//
// Environment:
//
//	BINDLE_STREAM_NAME            Kinesis stream (AWS_REGION, credentials from env or the task role)
//	BINDLE_SEARCH_URL / BINDLE_SEARCH_USER / BINDLE_SEARCH_PASSWORD   the OpenSearch writer user
//	BINDLE_SEARCH_PREFIX          index name prefix (default bindle)
//	BINDLE_PAYLOAD_STORE          s3://bucket/prefix: offloaded payloads and spooled batches
//	BINDLE_PAYLOAD_OFFLOAD_BYTES  default 32768
//	BINDLE_CONTROL_DB_URL         control plane as the processor role (or PG* env vars): shard leases
//	BINDLE_RETENTION_DAYS         default 30
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/awsv4"
	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/payload"
	"github.com/bindle-ai/bindle/backend/internal/search"
	"github.com/bindle-ai/bindle/backend/internal/stream"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	streamName := os.Getenv("BINDLE_STREAM_NAME")
	region := os.Getenv("AWS_REGION")
	if streamName == "" || region == "" {
		return errors.New("BINDLE_STREAM_NAME and AWS_REGION are required")
	}
	client, err := search.FromEnv(os.Getenv, envOr("BINDLE_SEARCH_USER", search.DefaultUsers.Writer), os.Getenv("BINDLE_SEARCH_PASSWORD"))
	if err != nil {
		return err
	}
	blobs, err := payload.FromURL(os.Getenv("BINDLE_PAYLOAD_STORE"), os.Getenv)
	if err != nil {
		return err
	}
	db, err := control.OpenFromEnv("BINDLE_CONTROL_DB_URL")
	if err != nil {
		return err
	}
	if db == nil {
		return errors.New("the control database is required for shard leases")
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	w := &search.Writer{
		Client: client, Names: search.Names{Prefix: os.Getenv("BINDLE_SEARCH_PREFIX")}, Log: log,
		Retention:    time.Duration(envInt("BINDLE_RETENTION_DAYS", 30)) * 24 * time.Hour,
		Payloads:     blobs,
		OffloadBytes: envInt("BINDLE_PAYLOAD_OFFLOAD_BYTES", 32<<10),
	}
	go w.Maintain(ctx, time.Hour)

	host, _ := os.Hostname()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	c := &stream.Consumer{
		Kinesis: &stream.Kinesis{Region: region, Creds: &awsv4.CredentialChain{Getenv: os.Getenv}},
		Stream:  streamName, Leases: &control.Leases{DB: db}, Writer: w, Spool: blobs,
		Owner: host + "-" + hex.EncodeToString(b), Log: log,
	}
	log.Info("bindle-processor starting", "stream", streamName, "owner", c.Owner)
	c.Run(ctx)
	log.Info("bindle-processor stopped")
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
