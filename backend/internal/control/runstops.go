package control

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// RunStop is a run stopped from the console.
type RunStop struct {
	TraceID   string    `json:"trace_id"`
	StoppedBy string    `json:"stopped_by,omitempty"`
	StoppedAt time.Time `json:"stopped_at"`
}

// StopRun records that a run should stop. The SDK learns about it from its
// polled config and bindle.check_limits() raises in that run. Stopping a
// run twice keeps the first time. Stops older than a day are removed.
func (s *Store) StopRun(ctx context.Context, tenant, userID, traceID string) (RunStop, error) {
	var by any
	if userID != "" {
		by = userID
	}
	var out RunStop
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM control.run_stops WHERE tenant_id = $1 AND stopped_at < now() - interval '1 day'`, tenant); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO control.run_stops (tenant_id, trace_id, stopped_by) VALUES ($1, $2, $3)
			ON CONFLICT (tenant_id, trace_id) DO NOTHING`, tenant, traceID, by); err != nil {
			return err
		}
		return scanStop(tx.QueryRowContext(ctx, `SELECT trace_id, COALESCE(stopped_by, ''), stopped_at FROM control.run_stops
			WHERE tenant_id = $1 AND trace_id = $2`, tenant, traceID), &out)
	})
	return out, err
}

// GetRunStop returns a run's stop, or ok=false if it was never stopped (or
// the stop expired).
func (s *Store) GetRunStop(ctx context.Context, tenant, traceID string) (RunStop, bool, error) {
	var out RunStop
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		return scanStop(tx.QueryRowContext(ctx, `SELECT trace_id, COALESCE(stopped_by, ''), stopped_at FROM control.run_stops
			WHERE tenant_id = $1 AND trace_id = $2 AND stopped_at > now() - interval '1 day'`, tenant, traceID), &out)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return RunStop{}, false, nil
	}
	return out, err == nil, err
}

func scanStop(row *sql.Row, out *RunStop) error {
	if err := row.Scan(&out.TraceID, &out.StoppedBy, &out.StoppedAt); err != nil {
		return err
	}
	out.StoppedAt = out.StoppedAt.UTC()
	return nil
}
