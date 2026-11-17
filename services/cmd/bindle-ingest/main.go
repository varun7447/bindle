// Command bindle-ingest is Bindle's OTLP/HTTP ingest edge.
//
// Environment:
//
//	BINDLE_LISTEN                  listen address (default 127.0.0.1:4318; the container image sets :4318)
//	BINDLE_CONTROL_DB_URL          control-plane PostgreSQL URL for the ingest role (or PGHOST, PGUSER, ... env vars);
//	                               ingest keys are resolved there
//	BINDLE_API_KEYS                "key=tenant,..." static keys: tests and development only
//	BINDLE_SINK                    stdout | opensearch (default stdout)
//	BINDLE_SEARCH_URL              OpenSearch endpoint (https; http only with BINDLE_SEARCH_ALLOW_HTTP=1)
//	BINDLE_SEARCH_USER / BINDLE_SEARCH_PASSWORD   the writer user (default user bindle_writer)
//	BINDLE_SEARCH_PREFIX           index name prefix (default bindle)
//	BINDLE_RETENTION_DAYS          retention (default 30; must match bindle-migrate)
//	BINDLE_PRICING_FILE            path to a JSON price table (optional)
//	BINDLE_RATE_LIMIT_SPANS_PER_SEC per-tenant limit (default 2000; 0 disables)
//	BINDLE_MAX_BODY_BYTES          default 8 MiB
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/bindle-ai/bindle/services/internal/auth"
	"github.com/bindle-ai/bindle/services/internal/control"
	"github.com/bindle-ai/bindle/services/internal/cost"
	"github.com/bindle-ai/bindle/services/internal/ingest"
	"github.com/bindle-ai/bindle/services/internal/normalize"
	"github.com/bindle-ai/bindle/services/internal/pipeline"
	"github.com/bindle-ai/bindle/services/internal/ratelimit"
	"github.com/bindle-ai/bindle/services/internal/redact"
	"github.com/bindle-ai/bindle/services/internal/search"
	"github.com/bindle-ai/bindle/services/internal/sink"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var keys auth.Resolver
	controlDB, err := control.OpenFromEnv("BINDLE_CONTROL_DB_URL")
	if err != nil {
		return err
	}
	switch static := os.Getenv("BINDLE_API_KEYS"); {
	case controlDB != nil && static != "":
		return errors.New("set either the control database or BINDLE_API_KEYS, not both")
	case controlDB != nil:
		defer controlDB.Close()
		r := &control.KeyResolver{DB: controlDB, Log: log}
		go r.RunUsageFlusher(ctx, time.Minute)
		keys = r
	default:
		k, err := auth.ParseEnv(static)
		if err != nil {
			return err
		}
		log.Warn("using static BINDLE_API_KEYS: for tests and development only")
		keys = k
	}
	prices, err := cost.Load(os.Getenv("BINDLE_PRICING_FILE"))
	if err != nil {
		return err
	}

	var out pipeline.Sink
	switch env("BINDLE_SINK", "stdout") {
	case "opensearch":
		client, err := search.FromEnv(os.Getenv, env("BINDLE_SEARCH_USER", search.DefaultUsers.Writer), os.Getenv("BINDLE_SEARCH_PASSWORD"))
		if err != nil {
			return err
		}
		w := &search.Writer{
			Client: client, Names: search.Names{Prefix: os.Getenv("BINDLE_SEARCH_PREFIX")}, Log: log,
			Retention: time.Duration(envFloat("BINDLE_RETENTION_DAYS", 30)*24) * time.Hour,
		}
		go w.Maintain(ctx, time.Hour)
		out = w
	case "stdout":
		out = &sink.Stdout{}
	default:
		return errors.New("BINDLE_SINK must be stdout or opensearch")
	}

	batcher := pipeline.NewBatcher(out, 50_000, 1_000, time.Second, log)
	go batcher.Run(ctx)

	rate := envFloat("BINDLE_RATE_LIMIT_SPANS_PER_SEC", 2000)
	h := &ingest.Handler{
		Keys:         keys,
		Normalizer:   &normalize.Normalizer{Prices: prices, Redactor: redact.Default()},
		Limiter:      ratelimit.New(rate, rate*5),
		Out:          batcher,
		MaxBodyBytes: int64(envFloat("BINDLE_MAX_BODY_BYTES", 8<<20)),
		Log:          log,
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/traces", h)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	srv := &http.Server{
		Addr:              env("BINDLE_LISTEN", "127.0.0.1:4318"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("bindle-ingest listening", "addr", srv.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	batcher.Wait() // drain buffered steps before exit
	log.Info("bindle-ingest stopped")
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
