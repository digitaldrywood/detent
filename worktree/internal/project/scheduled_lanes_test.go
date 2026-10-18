package project

import (
	"context"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/scheduleowner"
)

func TestScheduledLaneStoresPreserveOccurrences(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"intake", "lesson", "routine", "coordinated intake", "coordinated routine"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			tracker := memory.New(memory.Config{Stateful: true})
			issue, err := tracker.CreateIssue(t.Context(), connector.IssueDraft{Title: "Original", Body: "Original body"})
			if err != nil {
				t.Fatal(err)
			}
			if err := tracker.UpdateIssueState(t.Context(), issue.ID, "Todo"); err != nil {
				t.Fatal(err)
			}
			writes := 0
			write := func(ctx context.Context, id, target string) error {
				writes++
				return tracker.UpdateIssueState(ctx, id, target)
			}
			var store interface {
				SetIntakeIssueState(context.Context, string, string) error
			}
			switch kind {
			case "intake", "lesson":
				store = intakeLaneStore{IssueStore: tracker, write: write}
			case "routine":
				store = routineLaneStore{IssueStore: tracker, write: write}
			case "coordinated intake":
				store = coordinatedIntakeStore(tracker, &scheduleowner.IssueCoordinator{}, write)
			case "coordinated routine":
				store = coordinatedRoutineIssueStore(tracker, &scheduleowner.IssueCoordinator{}, write)
			}
			if err := intake.CommentOccurrence(t.Context(), store, issue.ID, "New occurrence"); err != nil {
				t.Fatal(err)
			}
			issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
			if err != nil || len(issues) != 1 {
				t.Fatalf("issues = %+v, error = %v", issues, err)
			}
			got := issues[0]
			if writes != 0 || got.State != "Todo" || got.Description != "Original body" || len(got.Comments) != 1 {
				t.Fatalf("occurrence changed issue or lane: issue = %+v, writes = %d", got, writes)
			}
			if err := store.SetIntakeIssueState(t.Context(), issue.ID, "Backlog"); err != nil || writes != 1 {
				t.Fatalf("lane delegation: error = %v, writes = %d", err, writes)
			}
			reader, ok := store.(interface {
				IntakeIssueClosed(context.Context, string) (bool, error)
			})
			if !ok {
				return
			}
			for _, closed := range []bool{false, true} {
				if closed {
					if err := tracker.CloseIssue(t.Context(), issue.ID); err != nil {
						t.Fatal(err)
					}
				}
				if got, err := reader.IntakeIssueClosed(t.Context(), issue.ID); err != nil || got != closed {
					t.Fatalf("closed = %v, error = %v, want %v", got, err, closed)
				}
			}
		})
	}
}
