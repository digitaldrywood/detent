package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestIssueCardHistory(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		count      int
		result     string
		wantReason string
	}{
		{name: "no history"}, {name: "beyond runtime cap", count: 75, result: "applied", wantReason: "merge_conflict"}, {name: "failed transition", count: 1, result: "failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := openParkTestStore(t, filepath.Join(t.TempDir(), "cards.db"))
			day := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
			for i := range tt.count {
				_, err := db.db.ExecContext(t.Context(), `INSERT INTO work_attempts(project_id,issue_id,worker_type,status,started_at) VALUES ('p','i','codex','running',?)`, day.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano))
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, row := range []struct {
				project, issue string
				at             time.Time
			}{{"p", "i", day.Add(-time.Nanosecond)}, {"p", "i", day.Add(24 * time.Hour)}, {"other", "i", day}, {"p", "other", day}} {
				_, err := db.db.ExecContext(t.Context(), `INSERT INTO work_attempts(project_id,issue_id,worker_type,status,started_at) VALUES (?,?, 'codex','running',?)`, row.project, row.issue, row.at.Format(time.RFC3339Nano))
				if err != nil {
					t.Fatal(err)
				}
			}
			if tt.result != "" {
				_, err := db.db.ExecContext(t.Context(), `INSERT INTO lane_ledger(project_id,issue_id,from_state,to_state,reason,written_at,result) VALUES ('p','i','In Progress','Rework','merge_conflict',?,?)`, day.Format(time.RFC3339Nano), tt.result)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.db.ExecContext(t.Context(), `INSERT INTO lane_ledger(project_id,issue_id,from_state,to_state,reason,written_at,result) VALUES ('p','i','Rework','Merging','later_failed_write',?,'failed')`, day.Add(time.Hour).Format(time.RFC3339Nano))
				if err != nil {
					t.Fatal(err)
				}
			}
			got, err := db.IssueCardHistory(t.Context(), IssueIdentity{ProjectID: "p", IssueID: "i"}, day.Add(12*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if got.AttemptsToday != int64(tt.count) || got.LaneReason != tt.wantReason {
				t.Fatalf("history = %+v", got)
			}
			if tt.wantReason != "" && (got.LaneReasonAt == nil || !got.LaneReasonAt.Equal(day)) {
				t.Fatalf("recorded time = %v", got.LaneReasonAt)
			}
		})
	}
}

func TestIssueCardHistoryLatestTransition(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, phase, status, project, issue string
		offset                              time.Duration
		want                                string
	}{
		{name: "later operator move", phase: "lane", status: "entered", project: "p", issue: "i", offset: time.Hour, want: "operator_move"},
		{name: "older observation", phase: "lane", status: "entered", project: "p", issue: "i", offset: -time.Hour, want: "merge_conflict"},
		{name: "applied write wins tie", phase: "lane", status: "entered", project: "p", issue: "i", want: "merge_conflict"},
		{name: "failed event", phase: "lane", status: "failed", project: "p", issue: "i", offset: time.Hour, want: "merge_conflict"},
		{name: "other phase", phase: "session", status: "entered", project: "p", issue: "i", offset: time.Hour, want: "merge_conflict"},
		{name: "other project", phase: "lane", status: "entered", project: "other", issue: "i", offset: time.Hour, want: "merge_conflict"},
		{name: "other issue", phase: "lane", status: "entered", project: "p", issue: "other", offset: time.Hour, want: "merge_conflict"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := openParkTestStore(t, filepath.Join(t.TempDir(), "cards.db"))
			at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			_, err := db.db.ExecContext(t.Context(), `INSERT INTO lane_ledger(project_id,issue_id,from_state,to_state,reason,written_at,result) VALUES ('p','i','In Progress','Rework','merge_conflict',?,'applied')`, at.Format(time.RFC3339Nano))
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.db.ExecContext(t.Context(), `INSERT INTO workflow_phase_events(project_id,issue_id,phase_type,phase_name,reason,status,started_at,event_day) VALUES (?,?,?,'Todo','operator_move',?,?, '2026-09-14')`, tt.project, tt.issue, tt.phase, tt.status, at.Add(tt.offset).Format(time.RFC3339Nano))
			if err != nil {
				t.Fatal(err)
			}
			got, err := db.IssueCardHistory(t.Context(), IssueIdentity{ProjectID: "p", IssueID: "i"}, at)
			if err != nil {
				t.Fatal(err)
			}
			wantAt := at
			if tt.want == "operator_move" {
				wantAt = at.Add(tt.offset)
			}
			if got.LaneReason != tt.want || got.LaneReasonAt == nil || !got.LaneReasonAt.Equal(wantAt) {
				t.Fatalf("history = %+v, want %s at %s", got, tt.want, wantAt)
			}
		})
	}
}
