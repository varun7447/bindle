package control

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/bindle-ai/bindle/services/internal/stream"
)

// Leases implements stream.LeaseStore in PostgreSQL. Connect it as the
// processor role.
type Leases struct{ DB *sql.DB }

var _ stream.LeaseStore = (*Leases)(nil)

// Acquire implements stream.LeaseStore.
func (l *Leases) Acquire(ctx context.Context, streamName, shard, owner string, ttl time.Duration) (stream.Lease, bool, error) {
	var lease stream.Lease
	err := l.DB.QueryRowContext(ctx, `INSERT INTO control.stream_leases (stream, shard_id, owner, lease_expires)
		VALUES ($1, $2, $3, now() + $4 * interval '1 millisecond')
		ON CONFLICT (stream, shard_id) DO UPDATE
		SET owner = excluded.owner, lease_expires = excluded.lease_expires, updated_at = now()
		WHERE control.stream_leases.lease_expires < now() OR control.stream_leases.owner = excluded.owner
		RETURNING checkpoint, finished`, streamName, shard, owner, ttl.Milliseconds()).Scan(&lease.Checkpoint, &lease.Finished)
	if errors.Is(err, sql.ErrNoRows) {
		return stream.Lease{}, false, nil
	}
	return lease, err == nil, err
}

// Renew implements stream.LeaseStore.
func (l *Leases) Renew(ctx context.Context, streamName, shard, owner string, ttl time.Duration) (bool, error) {
	res, err := l.DB.ExecContext(ctx, `UPDATE control.stream_leases SET lease_expires = now() + $4 * interval '1 millisecond', updated_at = now()
		WHERE stream = $1 AND shard_id = $2 AND owner = $3`, streamName, shard, owner, ttl.Milliseconds())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Checkpoint implements stream.LeaseStore; the owner check fences out a
// processor that lost its lease.
func (l *Leases) Checkpoint(ctx context.Context, streamName, shard, owner, seq string, finished bool) error {
	res, err := l.DB.ExecContext(ctx, `UPDATE control.stream_leases SET checkpoint = $4, finished = $5, updated_at = now()
		WHERE stream = $1 AND shard_id = $2 AND owner = $3`, streamName, shard, owner, seq, finished)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return stream.ErrLeaseLost
	}
	return nil
}

// Finished implements stream.LeaseStore.
func (l *Leases) Finished(ctx context.Context, streamName, shard string) (bool, error) {
	var done bool
	err := l.DB.QueryRowContext(ctx, `SELECT finished FROM control.stream_leases WHERE stream = $1 AND shard_id = $2`, streamName, shard).Scan(&done)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return done, err
}
