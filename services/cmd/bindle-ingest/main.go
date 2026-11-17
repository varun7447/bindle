// Command bindle-ingest is Bindle's OTLP/HTTP ingest edge.
//
// Environment:
//
//	BINDLE_LISTEN                  listen address (default 127.0.0.1:4318; the container image sets :4318)
//	BINDLE_API_KEYS                "key=tenant,key2=tenant2" (dev; prod reads Secrets Manager)
//	BINDLE_SINK                    stdout | postgres (default stdout)
//	BINDLE_DB_URL                  PostgreSQL URL for the writer role (postgres sink)
//	BINDLE_RETENTION_DAYS          step retention (default 30)
//	BINDLE_PRICING_FILE            path to a JSON price table (optional)
//	BINDLE_RATE_LIMIT_SPANS_PER_SEC per-tenant limit (default 2000; 0 disables)
//	BINDLE_MAX_BODY_BYTES          default 8 MiB
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/bindle-ai/bindle/services/internal/auth"
	"github.com/bindle-ai/bindle/services/internal/cost"
	"github.com/bindle-ai/bindle/services/internal/ingest"
	"github.com/bindle-ai/bindle/services/internal/normalize"
	"github.com/bindle-ai/bindle/services/internal/pg"
	"github.com/bindle-ai/bindle/services/internal/pipeline"
	"github.com/bindle-ai/bindle/services/internal/ratelimit"
	"github.com/bindle-ai/bindle/services/internal/redact"
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
	keys, err := auth.ParseEnv(os.Getenv("BINDLE_API_KEYS"))
	if err != nil {
		return err
	}
	prices, err := cost.Load(os.Getenv("BINDLE_PRICING_FILE"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var out pipeline.Sink
	switch env("BINDLE_SINK", "stdout") {
	case "postgres":
		dsn := os.Getenv("BINDLE_DB_URL")
		if dsn == "" {
			return errors.New("BINDLE_DB_URL is required for the postgres sink")
		}
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		db.SetMaxOpenConns(10)
		db.SetConnMaxIdleTime(5 * time.Minute)
		w := &pg.Writer{DB: db, Log: log, Retention: time.Duration(envFloat("BINDLE_RETENTION_DAYS", 30)*24) * time.Hour}
		go w.Maintain(ctx, time.Hour)
		out = w
	case "stdout":
		out = &sink.Stdout{}
	default:
		return errors.New("BINDLE_SINK must be stdout or postgres")
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
