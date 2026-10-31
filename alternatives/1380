package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readInstanceStepSuccesses(ctx context.Context, scope nativeScope, item, runner string) ([]tracker.FailureStepSuccess, error) {
	rows, err := s.database.reader.QueryContext(ctx, `SELECT id, work_item_id FROM (SELECT id,work_item_id,status FROM native_attempts WHERE organization_id=? AND project_id=? AND runner_id=? ORDER BY fencing_token DESC,id DESC LIMIT 16) WHERE work_item_id<>? AND status='succeeded'`, scope.organization, scope.project, runner, item)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type identity struct{ id, item string }
	var ids []identity
	for rows.Next() {
		var a identity
		if err := rows.Scan(&a.id, &a.item); err != nil {
			return nil, err
		}
		ids = append(ids, a)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var result []tracker.FailureStepSuccess
	for _, id := range ids {
		a, err := readNativeAttemptRecord(s.runDetailContext(ctx), s.database.reader, scope, id.item, id.id, s.config.now())
		if err != nil {
			return nil, err
		}
		if a.UpdatedAt.Before(s.config.now().Add(-30*time.Minute)) || a.UpdatedAt.After(s.config.now()) {
			continue
		}
		add := func(step string, at time.Time) {
			if len(result) < 16 {
				result = append(result, tracker.FailureStepSuccess{Step: step, AttemptID: id.id, WorkItemID: tracker.NativeWorkItemID(id.item), RunnerID: runner, MachineID: a.MachineID, CompletedAt: at})
			}
		}
		if a.Identity != nil {
			add("workspace_preparation", a.StartedAt)
		}
		for _, receipt := range a.Steps {
			if receipt.Step == "validation" && receipt.Validation != nil && receipt.Validation.ExitCode == 0 {
				add("validation", receipt.CompletedAt)
			}
		}
		if a.Runtime != nil && a.Runtime.Validation != nil && a.Runtime.Validation.ExitCode == 0 {
			add("validation", a.UpdatedAt)
		}
		if len(result) == 16 {
			break
		}
	}
	return result, nil
}
