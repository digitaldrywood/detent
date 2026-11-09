package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

const workspaceMaintenanceBatch = 100

func (w *workspaceService) organization() tracker.OrganizationID {
	if w.server.config.Hosted != nil {
		return tracker.OrganizationID(w.server.config.Hosted.OrganizationID)
	}
	return tracker.OrganizationID(w.server.localOrganization)
}

func (w *workspaceService) activeWorkspaceRecords(ctx context.Context) ([]workspaceRecord, error) {
	var records []workspaceRecord
	err := w.server.config.TenantRepository.ReadTenant(ctx, w.organization(), corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.WorkspaceLifecycleReader)
		if !ok {
			return workspaceUnavailable()
		}
		rows, err := owner.ActiveWorkspaceSessions(ctx, w.afterWorkspace, workspaceMaintenanceBatch)
		if err != nil {
			return err
		}
		for _, row := range rows {
			records = append(records, workspaceFromRecord(row))
		}
		if len(rows) < workspaceMaintenanceBatch {
			w.afterWorkspace = ""
		} else {
			w.afterWorkspace = rows[len(rows)-1].ID
		}
		return nil
	})
	return records, err
}

func (w *workspaceService) applyDeadlineRecord(ctx context.Context, record workspaceRecord, now time.Time) (workspaceRecord, bool, error) {
	record, recovered, err := w.recoverWorkspaceRecord(ctx, record)
	if err != nil {
		return record, false, err
	}
	decision := decideDeadline(record, now)
	if !decision.apply {
		return record, recovered, nil
	}
	scope := nativeScope{organization: record.OrganizationID, project: record.ProjectID}
	var finish workspaceDispatchClose
	if workspacesession.Terminal(decision.state) {
		finish, err = w.server.prepareWorkspaceDispatchClose(ctx, scope, record.ID)
		if err != nil {
			return record, false, err
		}
	}
	var moved workspaceRecord
	err = w.server.workspaceWrite(ctx, scope, func(writer workspaceWriter, _ time.Time) error {
		current, err := writer.WorkspaceSession(ctx, scope.project, record.ID)
		if err != nil {
			return err
		}
		if current.Revision != record.Revision {
			return errWorkspaceUnchanged
		}
		if workspacesession.Terminal(decision.state) {
			if current.RunnerID != "" {
				if err := writer.EndWorkspaceOccupancy(ctx, postgres.WorkspaceOccupancyEnd{OrganizationID: scope.organization, ProjectID: scope.project, WorkspaceID: record.ID, RunnerID: current.RunnerID, EndedAt: &decision.endedAt}); err != nil {
					return err
				}
			}
			moved, err = w.endWorkspaceRecord(ctx, writer, workspaceFromRecord(current), decision.state, decision.reason, now)
			if err != nil {
				return err
			}
			if finish != nil {
				if err = finish(writer); err != nil {
					return err
				}
			}
			return nil
		}
		moved, err = w.transitionRecord(ctx, writer, workspaceFromRecord(current), decision.state, decision.reason, now)
		return err
	})
	if errors.Is(err, errWorkspaceUnchanged) {
		return record, false, nil
	}
	return moved, err == nil, err
}

func (w *workspaceService) recoverWorkspaceRecords(ctx context.Context) error {
	records, err := w.activeWorkspaceRecords(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		if _, _, err := w.recoverWorkspaceRecord(ctx, record); err != nil {
			return err
		}
	}
	return nil
}

func (w *workspaceService) recoverWorkspaceRecord(ctx context.Context, record workspaceRecord) (workspaceRecord, bool, error) {
	if w.restartAt.IsZero() || !record.UpdatedAt.Before(w.restartAt) || record.State == workspacesession.StateClosing || record.State == workspacesession.StateRequested && record.RebindDeadline == nil {
		return record, false, nil
	}
	scope := nativeScope{organization: record.OrganizationID, project: record.ProjectID}
	moved := record
	err := w.server.workspaceWrite(ctx, scope, func(writer workspaceWriter, now time.Time) error {
		row, err := writer.WorkspaceSession(ctx, scope.project, record.ID)
		if err != nil {
			return err
		}
		if row.Revision != record.Revision {
			return errWorkspaceUnchanged
		}
		current := workspaceFromRecord(row)
		deadline := w.restartAt.Add(workspacesession.RebindGrace)
		current.RebindDeadline = &deadline
		if current.State == workspacesession.StateRequested {
			current.Revision++
			current.UpdatedAt = now
			moved = current
			return writeWorkspaceRecord(ctx, writer, current, row.Revision)
		}
		moved, err = w.transitionRecord(ctx, writer, current, workspacesession.StateRequested, workspacesession.ReasonHubRestarted, now)
		return err
	})
	if errors.Is(err, errWorkspaceUnchanged) {
		return record, false, nil
	}
	return moved, err == nil, err
}
