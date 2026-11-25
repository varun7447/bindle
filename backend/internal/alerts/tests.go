package alerts

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// ChannelTestJob is one "Send test" request claimed from the control plane.
type ChannelTestJob struct {
	TenantID  string
	ID        string
	ChannelID string
}

// TestStore is what the test runner needs from the control plane
// (implemented by *control.AlertStore, with the alerts role).
type TestStore interface {
	ClaimChannelTests(ctx context.Context, max int) ([]ChannelTestJob, error)
	// TestChannel returns the channel decrypted for delivery.
	TestChannel(ctx context.Context, tenant, channelID string) (Channel, error)
	FinishChannelTest(ctx context.Context, tenant, id string, ok bool, message string) error
}

// Sender delivers one alert to one channel (implemented by *Deliverer).
type Sender interface {
	Send(ctx context.Context, ch Channel, ev Event, r Rule) error
}

// TestRunner sends the console's test deliveries. It runs in the alerts
// worker, the only service allowed to decrypt channel targets and secrets,
// so the API never sees them.
type TestRunner struct {
	Store   TestStore
	Deliver Sender
	Log     *slog.Logger
	Now     func() time.Time
	Batch   int // tests claimed per tick; default 10
}

// TestSummary is the text of every test delivery.
const TestSummary = "Test alert from Bindle. If you can read this, the channel works."

// Run claims and sends tests every interval until ctx is done.
func (t *TestRunner) Run(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		t.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Once claims up to Batch open tests and sends each. It returns how many it
// finished.
func (t *TestRunner) Once(ctx context.Context) int {
	batch := t.Batch
	if batch <= 0 {
		batch = 10
	}
	jobs, err := t.Store.ClaimChannelTests(ctx, batch)
	if err != nil {
		t.log().Warn("claiming channel tests failed", "err", err)
		return 0
	}
	done := 0
	for _, j := range jobs {
		ok, msg := t.send(ctx, j)
		if err := t.Store.FinishChannelTest(ctx, j.TenantID, j.ID, ok, msg); err != nil {
			t.log().Warn("recording a channel test failed", "tenant", j.TenantID, "test", j.ID, "err", err)
			continue
		}
		done++
	}
	return done
}

func (t *TestRunner) send(ctx context.Context, j ChannelTestJob) (bool, string) {
	ch, err := t.Store.TestChannel(ctx, j.TenantID, j.ChannelID)
	if err != nil {
		t.log().Warn("test channel unavailable", "tenant", j.TenantID, "channel", j.ChannelID, "err", err)
		return false, "the channel could not be prepared for delivery"
	}
	now := time.Now()
	if t.Now != nil {
		now = t.Now()
	}
	ev := Event{ID: "evt_test", Kind: "test", Summary: TestSummary, FiredAt: now, TenantID: j.TenantID}
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := t.Deliver.Send(sctx, ch, ev, Rule{}); err != nil {
		t.log().Warn("test alert failed", "tenant", j.TenantID, "channel", j.ChannelID, "err", err)
		return false, DescribeFailure(err)
	}
	return true, ""
}

func (t *TestRunner) log() *slog.Logger {
	if t.Log == nil {
		return slog.Default()
	}
	return t.Log
}

var httpStatusRe = regexp.MustCompile(`^HTTP \d{3}$`)

// DescribeFailure reduces a delivery error to a message that is safe to show
// the user: it never echoes internal details such as resolved addresses or
// TLS internals.
func DescribeFailure(err error) string {
	msg := err.Error()
	switch {
	case httpStatusRe.MatchString(msg):
		return "the receiver answered " + msg
	case strings.Contains(msg, "non-public address"):
		return "the URL resolves to a private or local address"
	case strings.Contains(msg, "redirects"):
		return "the receiver answered with a redirect; use the final URL"
	case strings.Contains(msg, "not configured"):
		return "email delivery is not configured on this deployment"
	}
	return "the receiver could not be reached"
}
