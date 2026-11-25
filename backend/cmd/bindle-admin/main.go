// Command bindle-admin is the operator's tool for workspace settings that no
// app role may change. It connects as the control-plane schema owner, the
// same connection bindle-migrate uses.
//
//	bindle-admin features list [--tenant <id>]
//	bindle-admin features enable --tenant <id> [--by <name>] <feature>
//	bindle-admin features disable --tenant <id> <feature>
//	bindle-admin secrets encrypt
//
// Features: ledger, passports, outcome_grading, learning_loop. All are off
// by default. Changes reach the services within their 30-second cache.
//
// `secrets encrypt` encrypts alert-channel targets and secrets and
// outcome-endpoint secrets that were stored before encryption, and clears
// the plaintext. It is safe to run more than once.
//
// Environment:
//
//	BINDLE_CONTROL_DB_ADMIN_URL   schema-owner URL (or PGHOST, PGUSER, ... env vars)
//	BINDLE_KMS_KEY_ID, AWS_REGION   KMS key for `secrets encrypt` (or BINDLE_SECRETS_KEY locally)
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"time"

	_ "github.com/lib/pq"

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/secretbox"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "bindle-admin:", err)
		os.Exit(2)
	}
}

func usage() error {
	return errors.New(`usage:
  bindle-admin features list [--tenant <id>]
  bindle-admin features enable --tenant <id> [--by <name>] <feature>
  bindle-admin features disable --tenant <id> <feature>
  bindle-admin secrets encrypt
features: ` + strings.Join(control.Features, ", "))
}

func openAdminDB() (*sql.DB, error) {
	db, err := control.OpenFromEnv("BINDLE_CONTROL_DB_ADMIN_URL")
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, errors.New("set BINDLE_CONTROL_DB_ADMIN_URL (or PGHOST) to the schema-owner connection")
	}
	return db, nil
}

func encryptSecrets(out io.Writer) error {
	box, err := secretbox.FromEnv(os.Getenv)
	if err != nil {
		return err
	}
	if _, ok := box.(secretbox.Disabled); ok {
		return secretbox.ErrNotConfigured
	}
	db, err := openAdminDB()
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	n, err := control.EncryptLegacySecrets(ctx, db, box)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "encrypted %d alert channels and %d outcome endpoints\n", n.AlertChannels, n.OutcomeEndpoints)
	return nil
}

func run(args []string, out io.Writer) error {
	if len(args) == 2 && args[0] == "secrets" && args[1] == "encrypt" {
		return encryptSecrets(out)
	}
	if len(args) < 2 || args[0] != "features" {
		return usage()
	}
	cmd := args[1]
	if cmd != "list" && cmd != "enable" && cmd != "disable" {
		return usage()
	}
	fs := flag.NewFlagSet("features "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tenant := fs.String("tenant", "", "workspace (tenant) ID")
	by := fs.String("by", "", "who is enabling it (default: the OS user)")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if cmd != "list" && (*tenant == "" || fs.NArg() != 1) {
		return usage()
	}
	if cmd != "list" && !control.IsFeature(fs.Arg(0)) {
		return fmt.Errorf("unknown feature %q (known: %s)", fs.Arg(0), strings.Join(control.Features, ", "))
	}
	db, err := openAdminDB()
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if cmd == "list" {
		flags, err := control.ListFeatures(ctx, db, *tenant)
		if err != nil {
			return err
		}
		if len(flags) == 0 {
			fmt.Fprintln(out, "no features enabled")
		}
		for _, f := range flags {
			fmt.Fprintf(out, "%s\t%s\tenabled by %s at %s\n", f.TenantID, f.Feature, f.EnabledBy, f.EnabledAt.UTC().Format(time.RFC3339))
		}
		return nil
	}
	who := *by
	if who == "" {
		if u, err := user.Current(); err == nil {
			who = u.Username
		}
	}
	if err := control.SetFeature(ctx, db, *tenant, fs.Arg(0), cmd == "enable", who); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s %sd for %s\n", fs.Arg(0), cmd, *tenant)
	return nil
}
