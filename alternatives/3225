package web

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func TestBoardAttemptCostsLoadsMergedIssueHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	backend, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "costs.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	history := backend.(store.WorkAttemptStore)
	now := time.Now()
	pr := int64(3421)
	for _, project := range []string{"detent", "other"} {
		for number := 1; number <= 21; number++ {
			id, err := history.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: project, IssueID: "merged", Identifier: "owner/repo#3409", PRNumber: &pr, WorkerType: "code", WorkerHost: "sprite", AttemptNumber: number, StartedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			if err := history.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: id, CompletedAt: now.Add(time.Second), TerminalState: store.WorkAttemptTerminalSuccess, MetricsJSON: `{"total_tokens":41741,"token_usd":0.2255,"compute_usd":0.00012}`}); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := &Server{store: backend}
	for _, tt := range []struct {
		name, project, issue, identifier string
		want                             int
	}{
		{"by issue", "detent", "merged", "", 21},
		{"by identifier", "detent", "", "owner/repo#3409", 21},
		{"different issue", "detent", "absent", "", 0},
		{"missing scope", "", "merged", "", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var data templates.DashboardData
			s.loadBoardAttemptCosts(t.Context(), &data, tt.project, tt.issue, tt.identifier)
			if data.AttemptCostsError != "" || len(data.AttemptCosts) != tt.want {
				t.Fatalf("cost rows = %d / %q, want %d", len(data.AttemptCosts), data.AttemptCostsError, tt.want)
			}
			for _, row := range data.AttemptCosts {
				if row.TokenUSD != "$0.23" || row.ComputeUSD != "$0.000120" {
					t.Fatalf("row = %+v", row)
				}
			}
		})
	}
}
