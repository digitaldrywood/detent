package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

// Catch refresh queries that regress to scanning a fleet/project history, or
// sorting wide recent scheduler rows into temporary files. Result-only tests
// would still pass with either regression.
func TestRefreshHistoryQueriesUseIndexes(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	s, err := openSQLite(t.Context(), Config{Path: filepath.Join(t.TempDir(), "refresh.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	recorder := &refreshQueryRecorder{DB: s.db}
	s.queries = sqlc.New(recorder)
	identity := IssueIdentity{ProjectID: "p", IssueID: "i", Identifier: "owner/repo#1", IssueURL: "https://example.com/1"}
	tests := []struct {
		name    string
		read    func() error
		primary string
		noSort  bool
	}{
		{"recent project", func() error {
			_, err := s.ListRecentSchedulerDecisions(t.Context(), SchedulerDecisionQuery{ProjectID: "p"})
			return err
		}, "", true},
		{"recent fleet", func() error {
			_, err := s.ListRecentSchedulerDecisions(t.Context(), SchedulerDecisionQuery{})
			return err
		}, "", true},
		{"issue scheduler", func() error {
			_, err := s.ListIssueSchedulerDecisions(t.Context(), IssueSchedulerDecisionQuery{Identity: identity})
			return err
		}, "scheduler_decisions", false},
		{"tokens", func() error { _, err := s.IssueTokenSpend(t.Context(), identity); return err }, "codex_sessions", false},
		{"resume", func() error { _, err := s.LatestIssueAgentResumeState(t.Context(), identity); return err }, "codex_sessions", false},
		{"latest session", func() error { _, err := s.LatestIssueAgentSession(t.Context(), identity); return err }, "codex_sessions", false},
		{"attempts", func() error { _, err := s.ListIssueAIDebugWorkAttempts(t.Context(), identity); return err }, "work_attempts", false},
		{"spend", func() error {
			_, err := s.IssueSpendSince(t.Context(), IssueSpendSinceQuery{ProjectID: "p", IssueID: "i", Since: time.Now()})
			return err
		}, "usage_events", false},
		{"card history", func() error { _, err := s.IssueCardHistory(t.Context(), identity, time.Now()); return err }, "work_attempts,workflow_phase_events", false},
		{"activity", func() error {
			_, err := s.ListIssueActivity(t.Context(), IssueActivityQuery{ProjectID: "p", IssueID: "i", Identifier: identity.Identifier, IssueURL: identity.IssueURL})
			return err
		}, "", false},
		{"unscoped activity", func() error {
			_, err := s.ListIssueActivity(t.Context(), IssueActivityQuery{IssueID: "i", Identifier: identity.Identifier, IssueURL: identity.IssueURL})
			return err
		}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder.reads = nil
			if err := tt.read(); err != nil && !errors.Is(err, ErrNotFound) {
				t.Fatal(err)
			}
			if len(recorder.reads) == 0 {
				t.Fatal("no database read recorded")
			}
			for readIndex, read := range recorder.reads {
				rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+read.query, read.args...)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var details []string
				for rows.Next() {
					var id, parent, unused int
					var detail string
					if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
						t.Fatal(err)
					}
					details = append(details, detail)
					// A fleet recent read walks the time index with LIMIT. All other
					// base-table reads must search matching identities/primary keys.
					if strings.HasPrefix(detail, "SCAN ") && !strings.Contains(detail, "scheduler_decisions USING INDEX scheduler_decisions_at_idx") {
						for _, table := range []string{"scheduler_decisions", "codex_sessions", "work_attempts", "workflow_phase_events", "usage_events", "session"} {
							if strings.HasPrefix(detail, "SCAN "+table) {
								t.Errorf("history scan: %s", detail)
							}
						}
					}
					if tt.noSort && strings.Contains(detail, "TEMP B-TREE") {
						t.Errorf("wide scheduler sort: %s", detail)
					}
				}
				if err := errors.Join(rows.Err(), rows.Close()); err != nil {
					t.Fatal(err)
				}
				primaryTables := strings.Split(tt.primary, ",")
				primary := primaryTables[min(readIndex, len(primaryTables)-1)]
				if primary != "" && !strings.Contains(strings.Join(details, "\n"), "SEARCH "+primary+" USING INTEGER PRIMARY KEY") {
					t.Errorf("expected primary-key fetch, got %v", details)
				}
			}
		})
	}
}

type refreshQueryRead struct {
	query string
	args  []any
}

type refreshQueryRecorder struct {
	*sql.DB
	reads []refreshQueryRead
}

func (r *refreshQueryRecorder) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	r.reads = append(r.reads, refreshQueryRead{query: query, args: args})
	return r.DB.QueryContext(ctx, query, args...)
}

func (r *refreshQueryRecorder) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	r.reads = append(r.reads, refreshQueryRead{query: query, args: args})
	return r.DB.QueryRowContext(ctx, query, args...)
}
