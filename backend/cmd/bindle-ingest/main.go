// Command bindle-ingest is Bindle's OTLP/HTTP ingest edge.
//
// Environment:
//
//	BINDLE_LISTEN                  listen address (default 127.0.0.1:4318; the container image sets :4318)
//	BINDLE_CONTROL_DB_URL          control-plane PostgreSQL URL for the ingest role (or PGHOST, PGUSER, ... env vars);
//	                               ingest keys are resolved there
//	BINDLE_API_KEYS                "key=tenant,..." static keys: tests and development only
//	BINDLE_SINK                    stdout | opensearch | stream (default stdout); stream puts steps on
//	                               Kinesis (BINDLE_STREAM_NAME, AWS_REGION) for bindle-processor
//	BINDLE_SEARCH_URL              OpenSearch endpoint (https; http only with BINDLE_SEARCH_ALLOW_HTTP=1)
//	BINDLE_SEARCH_USER / BINDLE_SEARCH_PASSWORD   the writer user (default user bindle_writer)
//	BINDLE_SEARCH_PREFIX           index name prefix (default bindle)
//	BINDLE_RETENTION_DAYS          retention (default 30; must match bindle-migrate)
//	BINDLE_PAYLOAD_STORE           s3://bucket/prefix or file:///dir: where fields over
//	                               BINDLE_PAYLOAD_OFFLOAD_BYTES (default 32768) are stored;
//	                               fields are then capped at BINDLE_PAYLOAD_MAX_BYTES (default 1 MiB)
//	BINDLE_BUFFER_MAX_BYTES        bytes of steps buffered before 503 backpressure (default 256 MiB)
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

	"github.com/bindle-ai/bindle/backend/internal/auth"
	"github.com/bindle-ai/bindle/backend/internal/awsv4"
	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/cost"
	"github.com/bindle-ai/bindle/backend/internal/ingest"
	"github.com/bindle-ai/bindle/backend/internal/ledger"
	"github.com/bindle-ai/bindle/backend/internal/normalize"
	"github.com/bindle-ai/bindle/backend/internal/payload"
	"github.com/bindle-ai/bindle/backend/internal/pipeline"
	"github.com/bindle-ai/bindle/backend/internal/ratelimit"
	"github.com/bindle-ai/bindle/backend/internal/redact"
	"github.com/bindle-ai/bindle/backend/internal/search"
	"github.com/bindle-ai/bindle/backend/internal/secretbox"
	"github.com/bindle-ai/bindle/backend/internal/sink"
	"github.com/bindle-ai/bindle/backend/internal/stream"
	"github.com/bindle-ai/bindle/backend/internal/vault"
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
	var policies ingest.PolicySource // remote SDK config, from the control plane
	var features *control.FeatureResolver
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
		policies = &control.ConfigResolver{DB: controlDB, Log: log}
		features = &control.FeatureResolver{DB: controlDB, Log: log}
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
	maxPayload := 0 // normalizer default (32 KiB) unless a payload store takes large fields
	switch env("BINDLE_SINK", "stdout") {
	case "opensearch":
		client, err := search.FromEnv(os.Getenv, env("BINDLE_SEARCH_USER", search.DefaultUsers.Writer), os.Getenv("BINDLE_SEARCH_PASSWORD"))
		if err != nil {
			return err
		}
		blobs, err := payload.FromURL(os.Getenv("BINDLE_PAYLOAD_STORE"), os.Getenv)
		if err != nil {
			return err
		}
		w := &search.Writer{
			Client: client, Names: search.Names{Prefix: os.Getenv("BINDLE_SEARCH_PREFIX")}, Log: log,
			Retention:    time.Duration(envFloat("BINDLE_RETENTION_DAYS", 30)*24) * time.Hour,
			Payloads:     blobs,
			OffloadBytes: int(envFloat("BINDLE_PAYLOAD_OFFLOAD_BYTES", 32<<10)),
		}
		if blobs == nil {
			log.Warn("BINDLE_PAYLOAD_STORE not set: fields are capped at 32 KiB in the search index")
		} else {
			maxPayload = int(envFloat("BINDLE_PAYLOAD_MAX_BYTES", 1<<20))
		}
		go w.Maintain(ctx, time.Hour)
		out = w
		if controlDB != nil { // workspaces with the ledger feature also get ledger entries
			out = &ledger.Sink{Inner: w, Ledger: &ledger.Writer{DB: controlDB}, Features: features, Log: log}
		}
	case "stdout":
		out = &sink.Stdout{}
	case "stream":
		// Handled below: requests go straight to Kinesis, no local buffer.
	default:
		return errors.New("BINDLE_SINK must be stdout, opensearch or stream")
	}

	var submit ingest.Submitter
	drain := func() {} // waits for buffered steps to be written
	if env("BINDLE_SINK", "stdout") == "stream" {
		name, region := os.Getenv("BINDLE_STREAM_NAME"), os.Getenv("AWS_REGION")
		if name == "" || region == "" {
			return errors.New("BINDLE_SINK=stream needs BINDLE_STREAM_NAME and AWS_REGION")
		}
		spool, err := payload.FromURL(os.Getenv("BINDLE_PAYLOAD_STORE"), os.Getenv)
		if err != nil {
			return err
		}
		if spool != nil {
			maxPayload = int(envFloat("BINDLE_PAYLOAD_MAX_BYTES", 1<<20)) // the processor offloads
		}
		submit = &stream.Producer{Kinesis: &stream.Kinesis{Region: region, Creds: &awsv4.CredentialChain{Getenv: os.Getenv}}, Stream: name, Spool: spool}
	} else {
		batcher := pipeline.NewBatcher(out, 50_000, 1_000, time.Second, log)
		batcher.MaxBytes = int64(envFloat("BINDLE_BUFFER_MAX_BYTES", 256<<20))
		go batcher.Run(ctx)
		drain = batcher.Wait
		submit = batcher
	}

	rate := envFloat("BINDLE_RATE_LIMIT_SPANS_PER_SEC", 2000)
	limiter := ratelimit.New(rate, rate*5)
	h := &ingest.Handler{
		Keys:         keys,
		Policies:     policies,
		Normalizer:   &normalize.Normalizer{Prices: prices, Redactor: redact.Default(), MaxPayloadBytes: maxPayload},
		Limiter:      limiter,
		Out:          submit,
		MaxBodyBytes: int64(envFloat("BINDLE_MAX_BODY_BYTES", 8<<20)),
		Log:          log,
	}
	if controlDB != nil { // workspaces with the passports feature get drift detection
		h.Passports = &control.PassportResolver{DB: controlDB, Features: features, Log: log}
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/traces", h)
	// OTLP logs: only gen_ai.evaluation.result events are kept (as scores).
	logs := *h
	logs.Logs = true
	mux.Handle("/v1/logs", &logs)
	mux.Handle("/v1/config", &ingest.ConfigHandler{Keys: keys, Policies: policies, Limiter: limiter, Log: log})
	if controlDB != nil {
		// Passport pushes from the CLI or CI, with a management key.
		ph := &ingest.PassportHandler{Keys: &control.MgmtKeyResolver{DB: controlDB}, Store: &control.PassportWriter{DB: controlDB},
			Features: features, Policies: policies, Limiter: ratelimit.New(1, 10), Log: log}
		mux.Handle("/v1/passports", ph)
		mux.Handle("/v1/passports/validate", ph)
		// Business events reported after a run (feature outcome_grading).
		secrets, err := secretbox.FromEnv(os.Getenv)
		if err != nil {
			return err
		}
		oh := &ingest.OutcomeHandler{Keys: keys, Store: &control.OutcomeIngest{DB: controlDB, Secrets: secrets},
			Features: features, Limiter: ratelimit.New(20, 100), Log: log}
		mux.Handle("/v1/outcome-events", oh)
		mux.Handle("/v1/outcome-events/", oh)
		// Learning loop (feature learning_loop): lessons, lesson packs,
		// skill drafts and vault sync for the CLI, with a management key.
		li := &control.LearningIngest{DB: controlDB}
		lh := &ingest.LearningHandler{Keys: &control.MgmtKeyResolver{DB: controlDB}, IngestKeys: keys, Store: li, Ship: li, Features: features,
			Limiter: ratelimit.New(2, 20), Log: log, Saver: &vault.Saver{
				Store: li, Passport: li.Passport, Tools: li.AgentTools, Labels: ingest.CustomLabels(policies),
				Ledger:   &ledger.Writer{DB: controlDB},
				LedgerOn: func(ctx context.Context, t string) bool { return features.Enabled(ctx, t, ledger.Feature) },
			}}
		for _, p := range []string{"/v1/lessons", "/v1/lessons/", "/v1/skills", "/v1/skills/proof", "/v1/vault"} {
			mux.Handle(p, lh)
		}
	}
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
	stop()  // ends the batcher's run loop
	drain() // write buffered steps before exit
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
