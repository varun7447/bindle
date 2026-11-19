package api

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/control"
)

type fakeStops struct {
	stops map[string]control.RunStop // tenant/trace
}

func (f *fakeStops) StopRun(_ context.Context, tenant, user, trace string) (control.RunStop, error) {
	k := tenant + "/" + trace
	if st, ok := f.stops[k]; ok {
		return st, nil
	}
	st := control.RunStop{TraceID: trace, StoppedBy: user, StoppedAt: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	f.stops[k] = st
	return st, nil
}

func (f *fakeStops) GetRunStop(_ context.Context, tenant, trace string) (control.RunStop, bool, error) {
	st, ok := f.stops[tenant+"/"+trace]
	return st, ok, nil
}

func TestRunStopEndpoints(t *testing.T) {
	f := &fakeStops{stops: map[string]control.RunStop{}}
	h := (&Server{Store: &fakeStore{}, Stops: f,
		Sessions: tokenResolver{"tok-a": "tenant-a", "member-tok": "tenant-a", "tok-b": "tenant-b"},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	path := "/api/traces/" + traceID + "/stop"

	w, body := send(t, h, "GET", path, "tok-a", "")
	if w.Code != 200 || body["stopped"] != false || body["poll_seconds"] != float64(60) {
		t.Fatalf("before stop: %d %v", w.Code, body)
	}
	if w, _ := send(t, h, "POST", path, "member-tok", ""); w.Code != 403 {
		t.Fatalf("member stop: %d", w.Code)
	}
	if w, _ := send(t, h, "POST", "/api/traces/NOT-HEX/stop", "tok-a", ""); w.Code != 400 {
		t.Fatalf("bad id: %d", w.Code)
	}
	w, body = send(t, h, "POST", path, "tok-a", "")
	if w.Code != 200 || body["stopped"] != true || body["stopped_at"] != "2026-11-16T12:00:00Z" {
		t.Fatalf("stop: %d %v", w.Code, body)
	}
	if _, body := send(t, h, "GET", path, "tok-a", ""); body["stopped"] != true {
		t.Fatalf("after stop: %v", body)
	}
	if _, body := send(t, h, "GET", path, "tok-b", ""); body["stopped"] != false {
		t.Fatalf("tenant B sees A's stop: %v", body)
	}
	// Without a store: reads say "not stopped", stops are unavailable.
	h2 := (&Server{Store: &fakeStore{}, Sessions: tokenResolver{"tok-a": "tenant-a"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	if w, _ := send(t, h2, "POST", path, "tok-a", ""); w.Code != 503 {
		t.Fatalf("no store: %d", w.Code)
	}
}
