package cli

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type slowLifetimeTotals struct{ delay time.Duration }

func (s slowLifetimeTotals) LifetimeTotals(ctx context.Context) (store.LifetimeTotals, error) {
	select {
	case <-time.After(s.delay):
		return store.LifetimeTotals{TotalTokens: 99, Sessions: 2}, nil
	case <-ctx.Done():
		return store.LifetimeTotals{}, ctx.Err()
	}
}

func TestLifetimeTotalsRefreshRetainsLastSuccess(t *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "initial"
		if cached {
			name = "cached"
		}
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			defer slog.SetDefault(previousLogger)
			synctest.Test(t, func(t *testing.T) {
				snapshots := hub.New[telemetry.Snapshot]()
				registry := project.NewRegistry()
				mustSetProject(t, registry, newTelemetryProject(t, "alpha", nil, nil))
				if cached {
					if err := snapshots.Publish(telemetry.Snapshot{LifetimeTotals: telemetry.LifetimeTotals{Available: true, TotalTokens: 42, Sessions: 1}}); err != nil {
						t.Fatal(err)
					}
				}
				var seq atomic.Uint64
				if err := publishSnapshotOnce(t.Context(), registry, nil, snapshots, &seq, nil, time.Now(), nil, slowLifetimeTotals{600 * time.Millisecond}, "", nil); err != nil {
					t.Fatal(err)
				}
				got, _ := snapshots.Latest()
				if cached && (!got.LifetimeTotals.Available || got.LifetimeTotals.TotalTokens != 42) {
					t.Errorf("lost last good totals: %+v", got.LifetimeTotals)
				}
				if got.LifetimeTotals.Stale != cached || got.LifetimeTotals.ReadFailures != 1 || !strings.Contains(got.LifetimeTotals.DegradedReason, "deadline") {
					t.Errorf("missing failure metadata: %+v", got.LifetimeTotals)
				}
				if !strings.Contains(logs.String(), "level=DEBUG") {
					t.Error("missing debug diagnostic")
				}
				if err := publishSnapshotOnce(t.Context(), registry, nil, snapshots, &seq, nil, time.Now(), nil, slowLifetimeTotals{}, "", nil); err != nil {
					t.Fatal(err)
				}
				recovered, _ := snapshots.Latest()
				if totals := recovered.LifetimeTotals; !totals.Available || totals.Stale || totals.ReadFailures != 1 || totals.TotalTokens != 99 || totals.DegradedReason != "" {
					t.Errorf("recovery totals: %+v", totals)
				}
				if strings.Contains(logs.String(), "level=WARN msg=\"telemetry source read failed\" source=lifetime_totals") {
					t.Errorf("timeout logged at WARN: %s", logs.String())
				}
			})
		})
	}
}
