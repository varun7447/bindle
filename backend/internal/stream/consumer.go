package stream

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/payload"
)

// Lease is a shard's ownership and progress.
type Lease struct {
	Checkpoint string // last sequence number fully written
	Finished   bool   // shard closed and fully read
}

// LeaseStore coordinates processors. Implemented by control.Leases.
type LeaseStore interface {
	// Acquire takes the lease if free, expired or already ours.
	Acquire(ctx context.Context, stream, shard, owner string, ttl time.Duration) (Lease, bool, error)
	// Renew extends our lease; false if we lost it.
	Renew(ctx context.Context, stream, shard, owner string, ttl time.Duration) (bool, error)
	// Checkpoint records progress; fails if we no longer own the lease.
	Checkpoint(ctx context.Context, stream, shard, owner, seq string, finished bool) error
	// Finished reports whether a shard is fully processed.
	Finished(ctx context.Context, stream, shard string) (bool, error)
}

// ErrLeaseLost means another processor took the shard.
var ErrLeaseLost = errors.New("stream: lease lost")

// Writer stores a batch of steps (search.Writer). Writes must be
// idempotent: after a crash, records since the last checkpoint are read
// again.
type Writer interface {
	Write(ctx context.Context, steps []model.Step) error
}

// Consumer reads every shard it holds a lease for, writes the steps and
// checkpoints after each successful write (at-least-once delivery).
type Consumer struct {
	Kinesis  *Kinesis
	Stream   string
	Leases   LeaseStore
	Writer   Writer
	Spool    payload.Store
	Owner    string // unique per processor instance
	Log      *slog.Logger
	LeaseTTL time.Duration // default 30s
	Poll     time.Duration // GetRecords interval when idle; default 1s
	Discover time.Duration // shard discovery interval; default 10s

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// Run consumes until ctx ends.
func (c *Consumer) Run(ctx context.Context) {
	c.running = map[string]context.CancelFunc{}
	var wg sync.WaitGroup
	t := time.NewTicker(orDur(c.Discover, 10*time.Second))
	defer t.Stop()
	for {
		c.discover(ctx, &wg)
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-t.C:
		}
	}
}

func (c *Consumer) discover(ctx context.Context, wg *sync.WaitGroup) {
	shards, err := c.Kinesis.ListShards(ctx, c.Stream)
	if err != nil {
		if ctx.Err() == nil {
			c.Log.Warn("list shards failed", "err", err)
		}
		return
	}
	present := map[string]bool{}
	for _, s := range shards {
		present[s.ShardID] = true
	}
	for _, s := range shards {
		c.mu.Lock()
		_, busy := c.running[s.ShardID]
		c.mu.Unlock()
		if busy || !c.parentsDone(ctx, s, present) {
			continue
		}
		lease, ok, err := c.Leases.Acquire(ctx, c.Stream, s.ShardID, c.Owner, c.ttl())
		if err != nil || !ok || lease.Finished {
			continue
		}
		sctx, cancel := context.WithCancel(ctx)
		c.mu.Lock()
		c.running[s.ShardID] = cancel
		c.mu.Unlock()
		wg.Add(1)
		go func(shard string, lease Lease) {
			defer wg.Done()
			defer func() {
				c.mu.Lock()
				delete(c.running, shard)
				c.mu.Unlock()
				cancel()
			}()
			if err := c.consume(sctx, shard, lease.Checkpoint); err != nil && sctx.Err() == nil {
				c.Log.Warn("shard consumer stopped", "shard", shard, "err", err)
			}
		}(s.ShardID, lease)
	}
}

// parentsDone: after resharding, read parents to the end before children,
// so a tenant's steps stay in order. Parents that already expired from the
// stream count as done.
func (c *Consumer) parentsDone(ctx context.Context, s Shard, present map[string]bool) bool {
	for _, p := range []string{s.ParentShardID, s.AdjacentParent} {
		if p == "" || !present[p] {
			continue
		}
		if done, err := c.Leases.Finished(ctx, c.Stream, p); err != nil || !done {
			return false
		}
	}
	return true
}

func (c *Consumer) consume(ctx context.Context, shard, checkpoint string) error {
	it, err := c.Kinesis.ShardIterator(ctx, c.Stream, shard, checkpoint)
	if err != nil {
		return err
	}
	lastRenew := time.Now()
	for {
		if time.Since(lastRenew) > c.ttl()/3 {
			ok, err := c.Leases.Renew(ctx, c.Stream, shard, c.Owner, c.ttl())
			if err != nil {
				return err
			}
			if !ok {
				return ErrLeaseLost
			}
			lastRenew = time.Now()
		}
		page, err := c.Kinesis.GetRecords(ctx, it, 1000)
		if err != nil {
			var e *APIError
			if errors.As(err, &e) && e.Type == "ExpiredIteratorException" {
				if it, err = c.Kinesis.ShardIterator(ctx, c.Stream, shard, checkpoint); err != nil {
					return err
				}
				continue
			}
			if !sleep(ctx, 2*time.Second) {
				return ctx.Err()
			}
			continue
		}
		if len(page.Records) > 0 {
			var steps []model.Step
			for _, r := range page.Records {
				s, err := Decode(ctx, r.Data, c.Spool)
				if err != nil {
					// A record we can't read would block the shard forever;
					// log and skip it (it is retained in Kinesis for replay).
					c.Log.Error("skipping undecodable record", "shard", shard, "seq", r.SequenceNumber, "err", err)
					continue
				}
				steps = append(steps, s...)
			}
			if err := c.writeWithRetry(ctx, steps); err != nil {
				return err
			}
			checkpoint = page.Records[len(page.Records)-1].SequenceNumber
			if err := c.Leases.Checkpoint(ctx, c.Stream, shard, c.Owner, checkpoint, false); err != nil {
				return err
			}
		}
		if page.Next == "" { // shard closed and fully read
			return c.Leases.Checkpoint(ctx, c.Stream, shard, c.Owner, checkpoint, true)
		}
		it = page.Next
		if len(page.Records) == 0 && !sleep(ctx, orDur(c.Poll, time.Second)) {
			return ctx.Err()
		}
	}
}

// writeWithRetry retries with backoff until the write succeeds or ctx ends:
// moving past a batch without writing it would lose data.
func (c *Consumer) writeWithRetry(ctx context.Context, steps []model.Step) error {
	if len(steps) == 0 {
		return nil
	}
	backoff := 500 * time.Millisecond
	for {
		err := c.Writer.Write(ctx, steps)
		if err == nil {
			return nil
		}
		c.Log.Warn("write failed; retrying", "steps", len(steps), "err", err)
		if !sleep(ctx, backoff) {
			return ctx.Err()
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Consumer) ttl() time.Duration { return orDur(c.LeaseTTL, 30*time.Second) }

func orDur(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
