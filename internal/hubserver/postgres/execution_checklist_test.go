package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerEventQueuesScopedChecklistChanges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		project tracker.ProjectID
		attempt string
		wantErr error
	}{
		{name: "current attempt", project: "prj_1", attempt: "attempt_1"},
		{name: "different project", project: "prj_2", attempt: "attempt_1", wantErr: corestore.ErrTenantMismatch},
		{name: "different attempt", project: "prj_1", attempt: "attempt_2", wantErr: corestore.ErrTenantMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queue := new(pgx.Batch)
			var writer leasestore.ChecklistChangesWriter = &runnerEventSession{
				session: &session{organization: "org_1"},
				command: eventstore.RunCommand{Project: "prj_1", AttemptID: "attempt_1"},
				pending: queue,
			}
			err := writer.RecordExecutionCheckChanges(t.Context(), tc.project, tc.attempt, "revision", "evidence:event", time.Now(), []tracker.ExecutionCheckUpdate{
				{Ordinal: 1, Status: tracker.CheckPassed, Tree: "tree", Receipt: "receipt"},
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("checklist write error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if queue.Len() != 0 {
					t.Fatalf("out-of-scope write queued %d statements", queue.Len())
				}
				return
			}
			if queue.Len() != 1 {
				t.Fatalf("checklist write queued %d statements, want 1", queue.Len())
			}
		})
	}
}
