package alerts

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakeTests struct {
	jobs     []ChannelTestJob
	channels map[string]Channel
	finished map[string]string // test ID -> "ok" or the failure message
	claimErr error
}

func (f *fakeTests) ClaimChannelTests(_ context.Context, max int) ([]ChannelTestJob, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	n := len(f.jobs)
	if n > max {
		n = max
	}
	out := f.jobs[:n]
	f.jobs = f.jobs[n:]
	return out, nil
}

func (f *fakeTests) TestChannel(_ context.Context, tenant, id string) (Channel, error) {
	c, ok := f.channels[tenant+"/"+id]
	if !ok {
		return Channel{}, errors.New("decrypt failed: key ARN arn:aws:kms:us-west-2:111122223333:key/x")
	}
	return c, nil
}

func (f *fakeTests) FinishChannelTest(_ context.Context, _, id string, ok bool, msg string) error {
	if f.finished == nil {
		f.finished = map[string]string{}
	}
	if ok {
		msg = "ok"
	}
	f.finished[id] = msg
	return nil
}

type fakeSender struct {
	sent []Event
	err  map[string]error // channel ID -> error
}

func (s *fakeSender) Send(_ context.Context, ch Channel, ev Event, _ Rule) error {
	s.sent = append(s.sent, ev)
	return s.err[ch.ID]
}

func TestTestRunnerDeliversAndStoresSafeErrors(t *testing.T) {
	store := &fakeTests{
		jobs: []ChannelTestJob{
			{TenantID: "t1", ID: "ct_1", ChannelID: "ch_ok"},
			{TenantID: "t1", ID: "ct_2", ChannelID: "ch_private"},
			{TenantID: "t1", ID: "ct_3", ChannelID: "ch_gone"},
		},
		channels: map[string]Channel{
			"t1/ch_ok":      {ID: "ch_ok", TenantID: "t1", Kind: "webhook"},
			"t1/ch_private": {ID: "ch_private", TenantID: "t1", Kind: "webhook"},
		},
	}
	sender := &fakeSender{err: map[string]error{"ch_private": errors.New("dial tcp 10.0.0.5:443: non-public address")}}
	r := &TestRunner{Store: store, Deliver: sender, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	if n := r.Once(context.Background()); n != 3 {
		t.Fatalf("finished %d", n)
	}
	want := map[string]string{
		"ct_1": "ok",
		"ct_2": "the URL resolves to a private or local address",
		"ct_3": "the channel could not be prepared for delivery",
	}
	for id, w := range want {
		if store.finished[id] != w {
			t.Errorf("%s: %q, want %q", id, store.finished[id], w)
		}
	}
	if len(sender.sent) != 2 || sender.sent[0].Summary != TestSummary || sender.sent[0].TenantID != "t1" {
		t.Fatalf("sent %+v", sender.sent)
	}
}

func TestTestRunnerBatchesAndSurvivesClaimErrors(t *testing.T) {
	store := &fakeTests{channels: map[string]Channel{}}
	for i := 0; i < 15; i++ {
		store.jobs = append(store.jobs, ChannelTestJob{TenantID: "t", ID: "ct_" + string(rune('a'+i)), ChannelID: "ch"})
	}
	store.channels["t/ch"] = Channel{ID: "ch", TenantID: "t"}
	r := &TestRunner{Store: store, Deliver: &fakeSender{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if n := r.Once(context.Background()); n != 10 {
		t.Fatalf("first batch %d", n)
	}
	if n := r.Once(context.Background()); n != 5 {
		t.Fatalf("second batch %d", n)
	}
	store.claimErr = errors.New("db down")
	if n := r.Once(context.Background()); n != 0 {
		t.Fatalf("claim error finished %d", n)
	}
}

func TestDescribeFailure(t *testing.T) {
	cases := map[string]string{
		"HTTP 500":                     "the receiver answered HTTP 500",
		"stopped after 3 redirects":    "the receiver answered with a redirect; use the final URL",
		"email is not configured":      "email delivery is not configured on this deployment",
		"tls: handshake failure 1.2.3": "the receiver could not be reached",
	}
	for in, want := range cases {
		if got := DescribeFailure(errors.New(in)); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
