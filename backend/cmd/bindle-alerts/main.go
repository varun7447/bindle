// Command bindle-alerts evaluates every workspace's alert rules once a
// minute and delivers new alerts to Slack, signed webhooks or email (Amazon
// SES). It listens on no port. Several instances may run: an alert's
// fingerprint is unique in the control plane, so each fires (and is
// delivered) once.
//
// Environment:
//
//	BINDLE_CONTROL_DB_URL      control plane as the alerts role (or PG* env vars)
//	BINDLE_SEARCH_URL / BINDLE_SEARCH_USER / BINDLE_SEARCH_PASSWORD   the OpenSearch reader user
//	BINDLE_SEARCH_PREFIX       index name prefix (default bindle)
//	BINDLE_CONSOLE_URL         console base URL for links in alerts
//	BINDLE_ALERTS_EMAIL_FROM   verified SES identity; empty disables email (AWS_REGION, credentials from env or the task role)
//	BINDLE_ALERTS_INTERVAL     default 1m
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/alerts"
	"github.com/bindle-ai/bindle/backend/internal/awsv4"
	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/search"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	client, err := search.FromEnv(os.Getenv, envOr("BINDLE_SEARCH_USER", search.DefaultUsers.Reader), os.Getenv("BINDLE_SEARCH_PASSWORD"))
	if err != nil {
		return err
	}
	db, err := control.OpenFromEnv("BINDLE_CONTROL_DB_URL")
	if err != nil {
		return err
	}
	if db == nil {
		return errors.New("the control database is required")
	}
	defer db.Close()
	interval := time.Minute
	if v := os.Getenv("BINDLE_ALERTS_INTERVAL"); v != "" {
		if interval, err = time.ParseDuration(v); err != nil || interval < 10*time.Second {
			return errors.New("BINDLE_ALERTS_INTERVAL must be a duration of at least 10s")
		}
	}
	d := &alerts.Deliverer{HTTP: alerts.SafeClient(), ConsoleURL: os.Getenv("BINDLE_CONSOLE_URL")}
	if from := os.Getenv("BINDLE_ALERTS_EMAIL_FROM"); from != "" {
		region := os.Getenv("AWS_REGION")
		if region == "" {
			return errors.New("AWS_REGION is required for email alerts")
		}
		d.SES = &alerts.SES{Region: region, From: from, Creds: &awsv4.CredentialChain{Getenv: os.Getenv}}
	}
	cs := &control.AlertStore{DB: db}
	ss := &search.Store{Client: client, Names: search.Names{Prefix: os.Getenv("BINDLE_SEARCH_PREFIX")}}
	e := &alerts.Evaluator{
		Store: cs, Detect: ss, Deliver: d, Log: log,
		Budgets: &alerts.Budgets{Store: cs, Channels: cs, Spend: ss, Deliver: d, Log: log},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("bindle-alerts starting", "interval", interval.String(), "email", d.SES != nil)
	e.Run(ctx, interval)
	log.Info("bindle-alerts stopped")
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
