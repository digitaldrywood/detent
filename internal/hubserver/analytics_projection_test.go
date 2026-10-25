package hubserver

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestAnalyticsMaintenanceDeadline(t *testing.T) {
	for _, stop := range []string{"complete", "cancel", "deadline"} {
		t.Run(stop, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &Service{config: Config{Logger: slog.Default()}}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var calls []string
				drains := 0
				steps := []analyticsMaintenanceStep{
					{"drain", func(ctx context.Context, _ time.Time) (bool, error) {
						calls = append(calls, "drain")
						drains++
						deadline, ok := ctx.Deadline()
						if !ok || time.Until(deadline) > backgroundStorageBudget {
							t.Error("analytics maintenance lacks its small per-tick deadline")
						}
						switch stop {
						case "cancel":
							cancel()
						case "deadline":
							<-ctx.Done()
							if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
								t.Errorf("maintenance stopped without reaching its deadline: %v", ctx.Err())
							}
						}
						return drains < 2, ctx.Err()
					}},
					{"backfill", func(context.Context, time.Time) (bool, error) {
						calls = append(calls, "backfill")
						return false, nil
					}},
				}
				s.runAnalyticsMaintenance(ctx, time.Now(), steps)
				want := "drain,backfill,drain"
				if stop != "complete" {
					want = "drain"
				}
				if got := strings.Join(calls, ","); got != want {
					t.Fatalf("maintenance order=%s want=%s", got, want)
				}
			})
		})
	}
}
