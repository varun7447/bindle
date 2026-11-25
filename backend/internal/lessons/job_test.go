package lessons

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestJobSkipsFailingWorkspace(t *testing.T) {
	stored := map[string][]Lesson{}
	j := &Job{
		Tenants: func(context.Context) ([]Tenant, error) {
			return []Tenant{{ID: "bad", MinRuns: 5}, {ID: "t1", MinRuns: 5, Capture: false}}, nil
		},
		Groups: func(_ context.Context, tenant string, from, to time.Time) ([]Group, error) {
			if tenant == "bad" {
				return nil, errors.New("search down")
			}
			if to.Sub(from) != Window {
				t.Errorf("window %v", to.Sub(from))
			}
			return []Group{group(6, "order <N> not found")}, nil
		},
		Tools: func(context.Context, string, time.Time, time.Time) (map[string][]string, error) {
			return map[string][]string{"refund-agent": {"lookup_order"}}, nil
		},
		Replace: func(_ context.Context, tenant string, ls []Lesson, _ map[string][]string) (int, error) {
			stored[tenant] = ls
			return len(ls), nil
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ws, n := j.RunOnce(context.Background())
	if ws != 1 || n != 1 || len(stored["t1"]) != 1 || stored["bad"] != nil {
		t.Fatalf("ws %d n %d stored %v", ws, n, stored)
	}
	if stored["t1"][0].Pack.Content {
		t.Fatal("capture off must give packs without content")
	}
}
