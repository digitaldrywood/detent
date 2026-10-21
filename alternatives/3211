package web

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestBudgetSpendRegression(t *testing.T) {
	t.Parallel()

	periodStart := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	previousDay := telemetry.BudgetDay{Date: "2026-08-07", SpendUSD: 100}
	tests := []struct {
		name      string
		now       time.Time
		budget    telemetry.Budget
		want      bool
		wantDrop  float64
		wantSpend float64
	}{
		{
			name: "waits for a meaningful portion of the day",
			now:  periodStart.Add(5 * time.Hour),
			budget: telemetry.Budget{
				PeriodStart:       periodStart,
				ProjectedSpendUSD: 1,
				Days:              []telemetry.BudgetDay{previousDay},
			},
		},
		{
			name: "requires previous day baseline",
			now:  periodStart.Add(12 * time.Hour),
			budget: telemetry.Budget{
				PeriodStart:       periodStart,
				ProjectedSpendUSD: 1,
			},
		},
		{
			name: "ignores drop below threshold",
			now:  periodStart.Add(12 * time.Hour),
			budget: telemetry.Budget{
				PeriodStart:       periodStart,
				ProjectedSpendUSD: 11,
				Days:              []telemetry.BudgetDay{previousDay},
			},
		},
		{
			name: "detects ninety percent projected drop",
			now:  periodStart.Add(12 * time.Hour),
			budget: telemetry.Budget{
				PeriodStart:       periodStart,
				ProjectedSpendUSD: 10,
				Days:              []telemetry.BudgetDay{previousDay},
			},
			want:      true,
			wantDrop:  90,
			wantSpend: 10,
		},
		{
			name: "detects complete same-day collapse",
			now:  periodStart.Add(12 * time.Hour),
			budget: telemetry.Budget{
				PeriodStart: periodStart,
				Days:        []telemetry.BudgetDay{previousDay},
			},
			want:     true,
			wantDrop: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := budgetSpendRegression(tt.budget, tt.now)
			if (got != nil) != tt.want {
				t.Fatalf("budgetSpendRegression() = %#v, want present %t", got, tt.want)
			}
			if got != nil && (got.DropPercent != tt.wantDrop || got.ProjectedSpendUSD != tt.wantSpend || got.Date != "2026-08-08") {
				t.Fatalf("budgetSpendRegression() = %#v, want drop %.0f spend %.2f", got, tt.wantDrop, tt.wantSpend)
			}
		})
	}
}

func TestLogSpendRegressionWarnsOncePerDay(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	server := &Server{
		logger:           slog.New(slog.NewTextHandler(&logs, nil)),
		spendRegressions: newSpendRegressionMonitor(),
	}
	regression := &telemetry.SpendRegression{
		Date:              "2026-08-08",
		PreviousSpendUSD:  363.50,
		ProjectedSpendUSD: 3.22,
		DropPercent:       99.1,
		ThresholdPercent:  90,
	}
	server.logSpendRegression(regression)
	server.logSpendRegression(regression)

	got := logs.String()
	if strings.Count(got, "fleet daily spend regression") != 1 {
		t.Fatalf("warning count = %d, want 1\n%s", strings.Count(got, "fleet daily spend regression"), got)
	}
	for _, want := range []string{"event=fleet_daily_spend_regression", "previous_spend_usd=363.5", "projected_spend_usd=3.22", "drop_percent=99.1", "threshold_percent=90"} {
		if !strings.Contains(got, want) {
			t.Fatalf("warning missing %q:\n%s", want, got)
		}
	}
}
