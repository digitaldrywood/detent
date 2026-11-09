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

type workspaceMaintenanceCursor struct {
	workspace string
	actionRun string
}

func (w *workspaceService) organization() tracker.OrganizationID {
	if w.server.config.Hosted != nil {
		return tracker.OrganizationID(w.server.config.Hosted.OrganizationID)
	}
	return tracker.OrganizationID(w.server.localOrganization)
}

func (w *workspaceService) activeWorkspaceRecords(ctx context.Context) ([]workspaceRecord, error) {
	organization, err := w.selectMaintenanceOrganization(ctx)
	if err != nil || organization == "" {
		return nil, err
	}
	cursor := w.maintenanceCursors[organization]
	var records []workspaceRecord
	err = w.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.WorkspaceLifecycleReader)
		if !ok {
			return workspaceUnavailable()
		}
		rows, err := owner.ActiveWorkspaceSessions(ctx, cursor.workspace, workspaceMaintenanceBatch)
		if err != nil {
			return err
		}
		for _, row := range rows {
			records = append(records, workspaceFromRecord(row))
		}
		if len(rows) < workspaceMaintenanceBatch {
			cursor.workspace = ""
		} else {
			cursor.workspace = rows[len(rows)-1].ID
		}
		return nil
	})
	if err == nil {
		w.storeMaintenanceCursor(organization, cursor)
	}
	return records, err
}

func (w *workspaceService) maintenanceOrganization() tracker.OrganizationID {
	if w.selectedOrganization != "" {
		return w.selectedOrganization
	}
	return w.organization()
}

func (w *workspaceService) selectMaintenanceOrganization(ctx context.Context) (tracker.OrganizationID, error) {
	organization := w.organization()
	if w.server.config.Hosted == nil {
		err := w.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
			owner, ok := r.(postgres.WorkspaceLifecycleReader)
			if !ok {
				return workspaceUnavailable()
			}
			var err error
			organization, err = owner.NextWorkspaceMaintenanceOrganization(ctx, string(w.afterOrganization))
			if err == nil && organization == "" && w.afterOrganization != "" {
				organization, err = owner.NextWorkspaceMaintenanceOrganization(ctx, "")
			}
			return err
		})
		if err != nil {
			return "", err
		}
		w.afterOrganization = organization
	}
	w.selectedOrganization = organization
	return organization, nil
}

func (w *workspaceService) storeMaintenanceCursor(organization tracker.OrganizationID, cursor workspaceMaintenanceCursor) {
	if cursor.workspace == "" && cursor.actionRun == "" {
		delete(w.maintenanceCursors, organization)
		return
	}
	if w.maintenanceCursors == nil {
		w.maintenanceCursors = make(map[tracker.OrganizationID]workspaceMaintenanceCursor)
	}
	w.maintenanceCursors[organization] = cursor
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
	defer func() { w.selectedOrganization = "" }()
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
