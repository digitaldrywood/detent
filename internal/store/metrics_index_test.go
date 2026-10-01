package store

import (
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestWorkflowIndexedCorrelationEquivalence(t *testing.T) {
	t.Parallel()
	for _, seed := range []uint64{1, 7, 42, 2326} {
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			random := rand.New(rand.NewPCG(seed, seed+1))
			values := []string{"", " ", "a", " a ", "b", "c"}
			rows := make([]workflowMetricRow, 0, 400)
			for i := range 400 {
				phase := []WorkflowPhaseType{WorkflowPhaseTypeLane, WorkflowPhaseTypeAgentSession, WorkflowPhaseTypeCI, WorkflowPhaseTypeLocalCheck, WorkflowPhaseTypeReview}[random.IntN(5)]
				event := WorkflowPhaseEvent{
					ID: int64(i % 11), ProjectID: values[random.IntN(len(values))],
					IssueID: values[random.IntN(len(values))], Identifier: values[random.IntN(len(values))], IssueURL: values[random.IntN(len(values))],
					PhaseType: phase, PhaseName: []string{"In Progress", "Merging", "Rework"}[random.IntN(3)],
					StartedAt:       workflowMetricTestBase.Add(time.Duration(random.IntN(10)) * time.Minute),
					FinishedAt:      workflowMetricTestBase.Add(time.Duration(random.IntN(15)) * time.Minute),
					DurationSeconds: int64(random.IntN(1000) - 1), RunID: int64(random.IntN(15)), SessionID: int64(random.IntN(15)),
				}
				if random.IntN(2) == 0 {
					event.PRNumber = new(int64(random.IntN(3)))
				}
				rows = append(rows, workflowMetricRow{event: event})
			}
			if got, want := workflowLaneFlowsIndexed(rows, newWorkflowEventIndex(rows)), referenceWorkflowLaneFlows(rows); !reflect.DeepEqual(got, want) {
				t.Fatalf("flows differ: got %#v want %#v", got, want)
			}
			for _, selected := range [][]workflowMetricRow{rows, rows[:100], nil} {
				if got, want := workflowLaneRepresentativeRunsIndexed(selected, newWorkflowEventIndex(rows)), referenceWorkflowLaneRepresentativeRuns(selected, rows); !reflect.DeepEqual(got, want) {
					t.Fatalf("representatives differ: got %#v want %#v", got, want)
				}
			}
			// SQL no longer sorts the historical input. Representative ordering,
			// trends, and aggregates must be independent of the database row order.
			from, to := workflowMetricTestBase, workflowMetricTestBase.Add(time.Hour)
			unordered := slices.Clone(rows)
			for i := range unordered {
				unordered[i].event.ID = int64(i + 1)
			}
			want := workflowMetricsReport(unordered, unordered, from, to)
			slices.Reverse(unordered)
			if got := workflowMetricsReport(unordered, unordered, from, to); !reflect.DeepEqual(got, want) {
				t.Fatal("workflow report changed with historical row order")
			}
		})
	}
}

func TestWorkflowHistoryRevision(t *testing.T) {
	t.Parallel()
	backend, err := openSQLite(t.Context(), Config{Path: filepath.Join(t.TempDir(), "history.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	event := workflowMetricTestEvent("project", "issue", WorkflowPhaseTypeLane, "In Progress", 0, time.Hour)
	var id int64
	tests := []struct {
		name   string
		mutate func() error
		want   int64
	}{
		{"initial", func() error { return nil }, 0},
		{"insert", func() error {
			var err error
			id, err = backend.RecordWorkflowPhaseEvent(t.Context(), event)
			return err
		}, 1},
		{"metadata correction", func() error { return backend.UpdateWorkflowPhaseEventMetadata(t.Context(), id, `{"corrected":true}`) }, 2},
		{"same count and timestamps correction", func() error {
			_, err := backend.db.ExecContext(t.Context(), "UPDATE workflow_phase_events SET duration_seconds = 42 WHERE id = ?", id)
			return err
		}, 3},
		{"rollback", func() error {
			tx, err := backend.db.BeginTx(t.Context(), nil)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(t.Context(), "DELETE FROM workflow_phase_events"); err != nil {
				t.Error(err)
			}
			return tx.Rollback()
		}, 3},
		{"delete", func() error {
			_, err := backend.db.ExecContext(t.Context(), "DELETE FROM workflow_phase_events WHERE id = ?", id)
			return err
		}, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.mutate(); err != nil {
				t.Fatal(err)
			}
			got, err := backend.WorkflowHistoryRevision(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("revision = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestIssueWorkflowTimelineIndexedIdentityUnion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	backend := openTestStore(t, ctx)
	writer := backend.(*sqliteStore)
	base := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	for i := range 250 {
		event := WorkflowPhaseEvent{ProjectID: "project-a", PhaseType: WorkflowPhaseTypeLane, PhaseName: "Todo", StartedAt: base.Add(time.Duration(i%7) * time.Minute)}
		switch i % 4 {
		case 0:
			event.IssueID = "issue-1"
		case 1:
			event.Identifier = "owner/repo#1"
		case 2:
			event.IssueURL = "https://github.com/owner/repo/issues/1"
		case 3:
			event.IssueID, event.Identifier, event.IssueURL = "issue-1", "owner/repo#1", "https://github.com/owner/repo/issues/1"
		}
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
		event.ProjectID = "project-b"
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	for _, identity := range []IssueIdentity{
		{ProjectID: "project-a", IssueID: "issue-1", Identifier: "owner/repo#1", IssueURL: "https://github.com/owner/repo/issues/1"},
		{ProjectID: "project-a", IssueID: "issue-1"},
		{ProjectID: "project-a", Identifier: "owner/repo#1"},
		{ProjectID: "project-a", IssueURL: "https://github.com/owner/repo/issues/1"},
		{ProjectID: "project-a", IssueID: "missing", Identifier: "owner/repo#1"},
		{ProjectID: "missing", IssueID: "issue-1"},
	} {
		timeline, err := backend.IssueWorkflowTimeline(ctx, identity)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := writer.db.QueryContext(ctx, `SELECT id FROM workflow_phase_events WHERE project_id=? AND (issue_id=? OR identifier=? OR issue_url=?) ORDER BY started_at,id`, identity.ProjectID, nullString(identity.IssueID), nullString(identity.Identifier), nullString(identity.IssueURL))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var expected []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			expected = append(expected, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		var actual []int64
		for _, event := range timeline.Events {
			actual = append(actual, event.ID)
			if event.ProjectID != identity.ProjectID {
				t.Fatalf("cross-project event: %+v", event)
			}
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("identity %+v IDs=%v want%v", identity, actual, expected)
		}
		if identity.IssueID == "issue-1" && identity.Identifier != "" && len(actual) != 250 {
			t.Fatalf("complete history truncated/duplicated: %d", len(actual))
		}
	}
}
