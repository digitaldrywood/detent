package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type retentionTestReaper struct {
	cleanupSweepReaper
	completed map[string]time.Time
}

func (r *retentionTestReaper) SweepRetention(ctx context.Context, request workspace.RetentionRequest) (workspace.RetentionTotals, error) {
	completed, err := request.Completed(ctx, []workspace.Issue{{ID: "issue"}})
	r.completed = completed
	return workspace.RetentionTotals{Workdir: "test", At: request.Now, HookLogs: workspace.RemovalTotal{Count: 2, Bytes: 32}}, err
}

func TestRetentionCompletionClock(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	done := now.Add(-8 * 24 * time.Hour)
	closed := now.Add(-7 * 24 * time.Hour)
	for _, test := range []struct {
		name, state              string
		closed, stage, closeTime bool
		want                     time.Time
	}{
		{name: "done", state: "Done", stage: true, want: done},
		{name: "cancelled", state: "Cancelled", stage: true, want: done},
		{name: "closed lane", state: "Closed", stage: true, want: done},
		{name: "duplicate lane", state: "Duplicate", stage: true, want: done},
		{name: "custom terminal", state: "Archived", stage: true, want: done},
		{name: "custom terminal unknown time", state: "Archived"},
		{name: "closed later comment", state: "Backlog", closed: true, closeTime: true, want: closed},
		{name: "done then closed", state: "Done", stage: true, closed: true, closeTime: true, want: done},
		{name: "unknown time", state: "Done"},
		{name: "active", state: "In Progress", stage: true},
		{name: "human review", state: "Human Review", stage: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := connector.Issue{ID: "issue", Identifier: "repo#1", State: test.state, Closed: test.closed, UpdatedAt: &now}
			if test.stage {
				issue.StageUpdatedAt = &done
			}
			if test.closeTime {
				issue.ClosedAt = &closed
			}
			reaper := &retentionTestReaper{}
			o := &Orchestrator{cfg: Config{TerminalStates: normalizedStates([]string{"Done", "Cancelled", "Canceled", "Closed", "Duplicate", "Archived"})}, reaper: reaper, connector: memory.New(memory.Config{Issues: []connector.Issue{issue}}), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			state := State{}
			o.sweepRetention(t.Context(), &state, now)
			if got := reaper.completed[issue.ID]; !got.Equal(test.want) {
				t.Fatalf("completed=%v want=%v", got, test.want)
			}
			if len(state.WorkspaceRetention) != 1 || state.WorkspaceRetention[0].HookLogs.Count != 2 {
				t.Fatalf("totals=%+v", state.WorkspaceRetention)
			}
		})
	}
}
