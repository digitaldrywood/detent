package cli

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func TestDoctorMergeQueueRecordedHistory(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name               string
		strict, queue      bool
		unavailable        bool
		minutes            int
		heads              int
		branch             string
		wantRecommendation bool
		wantDetail         []string
	}{
		{name: "plan unavailable", strict: true, unavailable: true, minutes: 22, branch: "main"},
		{name: "strict busy branch", strict: true, minutes: 22, branch: "main", wantRecommendation: true, wantDetail: []string{"queue:", "31 distinct recorded merges", "30.0/day", "22.0 minutes", "0.46 merges per CI duration"}},
		{name: "non strict busy branch", minutes: 22, branch: "main"},
		{name: "existing queue", strict: true, queue: true, minutes: 22, branch: "main"},
		{name: "queue slower than merges", queue: true, minutes: 90, branch: "main", wantRecommendation: true, wantDetail: []string{"batching:", "1.25 per hour", "90.0 minutes", "1.88 merges per CI duration > 1"}},
		{name: "fast CI", strict: true, minutes: 2, branch: "main"},
		{name: "missing CI history", strict: true, branch: "main"},
		{name: "different branch", strict: true, minutes: 22, branch: "release"},
		{name: "repeated pushes per pull request", queue: true, minutes: 2, heads: 3, branch: "main", wantRecommendation: true, wantDetail: []string{"draft convention:", "93 distinct heads across 31 PRs", "3.00 CI runs per PR > 1.5"}},
		{name: "single push per pull request", queue: true, minutes: 2, heads: 1, branch: "main"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			path := filepath.Join(t.TempDir(), "history.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			_, err = db.ExecContext(t.Context(), `CREATE TABLE workflow_phase_events (id INTEGER PRIMARY KEY, project_id TEXT, phase_type TEXT, status TEXT, reason TEXT, started_at TEXT, metadata_json TEXT)`)
			if err != nil {
				t.Fatal(err)
			}
			for index := range 31 {
				metadata := fmt.Sprintf(`{"pull_request":{"repository":"example/repo","base_ref":%q,"number":%d,"ci_duration_seconds":%d}}`, tt.branch, index+1, tt.minutes*60)
				at := now.Add(-time.Duration(index) * 48 * time.Minute).Format(time.RFC3339Nano)
				_, err = db.ExecContext(t.Context(), `INSERT INTO workflow_phase_events(project_id,phase_type,status,reason,started_at,metadata_json) VALUES ('alpha','lane','entered','pull_request_merged',?,?)`, at, metadata)
				if err != nil {
					t.Fatal(err)
				}
				for head := range tt.heads {
					metadata := fmt.Sprintf(`{"pull_request":{"repository":"example/repo","number":%d,"head_sha":"head-%d-%d"}}`, index+1, index+1, head)
					_, err = db.ExecContext(t.Context(), `INSERT INTO workflow_phase_events(project_id,phase_type,status,reason,started_at,metadata_json) VALUES ('alpha','lane','entered','state_transition',?,?)`, at, metadata)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			cfg := workflowconfig.Config{}
			cfg.Tracker.Repository = "example/repo"
			got := checkDoctorMergeQueue(t.Context(), "alpha", globalconfig.Project{}, cfg, path, doctorDeps{
				now:                func() time.Time { return now },
				openSQLiteReadOnly: openDoctorSQLiteReadOnly,
				githubBranchPolicy: func(context.Context, workflowconfig.Config, string) (ghconnector.BranchMergePolicy, error) {
					return ghconnector.BranchMergePolicy{Branch: "main", Strict: tt.strict, MergeQueue: tt.queue, RulesUnavailableOnPlan: tt.unavailable}, nil
				},
			})
			if (got.Status == doctorWarn) != tt.wantRecommendation {
				t.Fatalf("check = %+v", got)
			}
			if tt.unavailable && !strings.Contains(got.Detail, "not available on this plan") {
				t.Fatalf("check = %+v", got)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(got.Detail, want) {
					t.Fatalf("detail %q missing %q", got.Detail, want)
				}
			}
		})
	}
}
