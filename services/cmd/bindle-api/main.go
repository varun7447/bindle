// Command bindle-api is the console's read API over OpenSearch.
//
// Environment:
//
//	BINDLE_API_LISTEN                        listen address (default 127.0.0.1:8080; compose sets :8080)
//	BINDLE_SEARCH_URL                        OpenSearch endpoint (https; http only with BINDLE_SEARCH_ALLOW_HTTP=1)
//	BINDLE_SEARCH_USER / BINDLE_SEARCH_PASSWORD   the read-only user (default user bindle_reader)
//	BINDLE_SEARCH_PREFIX                     index name prefix (default bindle)
//	BINDLE_PAYLOAD_STORE                     s3://bucket/prefix or file:///dir (read-only use: offloaded payloads)
//	BINDLE_API_RATE_LIMIT_RPS                per-tenant requests/sec (default 20; 0 disables)
//	BINDLE_API_LOOP_MIN_REPEATS              runaway-loop threshold (default 5)
//	BINDLE_CONTROL_DB_URL                    control-plane PostgreSQL URL for the API role (or PGHOST, ... env vars)
//	BINDLE_COGNITO_ISSUER                    https://cognito-idp.<region>.amazonaws.com/<user pool id>
//	BINDLE_COGNITO_CLIENT_ID                 the console's app client ID
//	BINDLE_API_DEV_AUTH_BYPASS_TENANT        local dev only: map every request to this tenant
//	BINDLE_API_DEV_AUTH_BYPASS_IN_CONTAINER  set to 1 to allow the bypass on a container's
//	                                         wildcard bind (publish the port to 127.0.0.1 only)
//
// Without the bypass, every /api request returns 401 until a real session
// verifier is wired in (control plane, step 3).
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

	"github.com/bindle-ai/bindle/services/internal/api"
	"github.com/bindle-ai/bindle/services/internal/control"
	"github.com/bindle-ai/bindle/services/internal/payload"
	"github.com/bindle-ai/bindle/services/internal/ratelimit"
	"github.com/bindle-ai/bindle/services/internal/search"
	"github.com/bindle-ai/bindle/services/internal/session"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	addr := env("BINDLE_API_LISTEN", "127.0.0.1:8080")
	controlDB, err := control.OpenFromEnv("BINDLE_CONTROL_DB_URL")
	if err != nil {
		return err
	}
	var store *control.Store
	var dir session.Directory
	if controlDB != nil {
		defer controlDB.Close()
		store = &control.Store{DB: controlDB}
		dir = store
	}
	sessions, err := session.FromEnv(os.Getenv, addr, dir)
	if err != nil {
		return err
	}
	switch s := sessions.(type) {
	case session.FailClosed:
		log.Warn("no session verifier configured: every /api request will be rejected with 401")
	case *session.Cognito:
		log.Info("console sign-in: Amazon Cognito", "issuer", s.Verifier.Issuer)
	case session.DevBypass:
		log.Warn("DEV AUTH BYPASS enabled: all requests are served as one tenant; local use only",
			"tenant", s.TenantID, "addr", addr)
	}

	client, err := search.FromEnv(os.Getenv, env("BINDLE_SEARCH_USER", search.DefaultUsers.Reader), os.Getenv("BINDLE_SEARCH_PASSWORD"))
	if err != nil {
		return err
	}

	blobs, err := payload.FromURL(os.Getenv("BINDLE_PAYLOAD_STORE"), os.Getenv)
	if err != nil {
		return err
	}

	rps := envInt("BINDLE_API_RATE_LIMIT_RPS", 20)
	srvAPI := &api.Server{
		Store:          &search.Store{Client: client, Names: search.Names{Prefix: os.Getenv("BINDLE_SEARCH_PREFIX")}, Payloads: blobs, Log: log},
		Sessions:       sessions,
		Keys:           keyStore(store),
		Config:         configStore(store),
		Limiter:        ratelimit.New(float64(rps), float64(rps*2)),
		Log:            log,
		LoopMinRepeats: envInt("BINDLE_API_LOOP_MIN_REPEATS", 5),
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           srvAPI.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		log.Info("bindle-api listening", "addr", addr)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Info("bindle-api stopped")
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// keyStore avoids a typed-nil interface when the control plane is absent.
func keyStore(s *control.Store) api.KeyStore {
	if s == nil {
		return nil
	}
	return s
}

func configStore(s *control.Store) api.ConfigStore {
	if s == nil {
		return nil
	}
	return s
}
