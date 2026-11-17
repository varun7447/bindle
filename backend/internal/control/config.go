package control

import (
	"database/sql"
	"os"
	"time"
)

// OpenFromEnv opens the control-plane database from the URL in urlVar, or
// from libpq-style PGHOST/PGUSER/PGPASSWORD/PGDATABASE/PGSSLMODE variables
// (used on AWS). Returns nil, nil when neither is configured.
func OpenFromEnv(urlVar string) (*sql.DB, error) {
	dsn := os.Getenv(urlVar)
	if dsn == "" && os.Getenv("PGHOST") == "" {
		return nil, nil
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return db, nil
}
