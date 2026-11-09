package web

import (
	"context"
	"math"
	"testing"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func TestBoardAttemptCostsLoadsMergedIssueHistory(t *testing.T) {
	t.Parallel()
	backend := &attemptCostsStore{}
	for number := 1; number <= 21; number++ {
		backend.attempts = append(backend.attempts, store.WorkAttempt{ID: int64(number), AttemptNumber: number, MetricsJSON: `{"total_tokens":41741,"token_usd":0.2255,"compute_usd":0.00012}`})
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
			backend.queries = nil
			var data templates.DashboardData
			s.loadBoardAttemptCosts(t.Context(), &data, tt.project, tt.issue, tt.identifier)
			if data.AttemptCostsError != "" || len(data.AttemptCosts) != tt.want {
				t.Fatalf("cost rows = %d / %q, want %d", len(data.AttemptCosts), data.AttemptCostsError, tt.want)
			}
			if tt.project == "" {
				if len(backend.queries) != 0 {
					t.Fatal("unscoped history request")
				}
			} else if len(backend.queries) != 1 || backend.queries[0] != (store.WorkAttemptHistoryQuery{ProjectID: tt.project, IssueID: tt.issue, Identifier: tt.identifier, Limit: math.MaxInt32}) {
				t.Fatalf("history selectors = %+v", backend.queries)
			}
			for _, row := range data.AttemptCosts {
				if row.TokenUSD != "$0.23" || row.ComputeUSD != "$0.000120" {
					t.Fatalf("row = %+v", row)
				}
			}
		})
	}
}

type attemptCostsStore struct {
	store.Store
	attempts []store.WorkAttempt
	queries  []store.WorkAttemptHistoryQuery
}

func (s *attemptCostsStore) ListRecentTerminalWorkAttempts(ctx context.Context, q store.WorkAttemptHistoryQuery) ([]store.WorkAttempt, error) {
	s.queries = append(s.queries, q)
	if q.ProjectID != "detent" || q.IssueID != "merged" && q.Identifier != "owner/repo#3409" {
		return nil, ctx.Err()
	}
	return append([]store.WorkAttempt(nil), s.attempts...), ctx.Err()
}
