package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type workspaceRecord workspacehub.Record

func (r workspaceRecord) owner() workspacesession.Owner { return workspacehub.Record(r).Owner() }
func (r workspaceRecord) resource() workspacesession.Session {
	return workspacehub.Record(r).Resource()
}
func workspaceTenantRecord(row postgres.WorkspaceSession) workspaceRecord {
	return workspaceRecord(workspacehub.TenantRecord(workspacehub.Row(row)))
}
func workspaceTenantUpdate(row postgres.WorkspaceSession, state, reason string, now time.Time) postgres.WorkspaceUpdate {
	return postgres.WorkspaceUpdate(workspacehub.TenantUpdate(workspacehub.Row(row), state, reason, now))
}
func workspaceSameTime(left, right *time.Time) bool { return workspacehub.SameTime(left, right) }

type workspaceLifecycleReader struct{ postgres.WorkspaceReader }

func (r workspaceLifecycleReader) WorkspaceOpenSessions(ctx context.Context, after time.Time, id string, limit int) ([]workspacehub.Row, error) {
	rows, err := r.WorkspaceReader.WorkspaceOpenSessions(ctx, after, id, limit)
	if err != nil {
		return nil, err
	}
	var result []workspacehub.Row
	for _, row := range rows {
		result = append(result, workspacehub.Row(row))
	}
	return result, nil
}
func (r workspaceLifecycleReader) WorkspaceSession(ctx context.Context, project tracker.ProjectID, id string) (workspacehub.Row, error) {
	row, err := r.WorkspaceReader.WorkspaceSession(ctx, project, id)
	return workspacehub.Row(row), err
}

type workspaceLifecycleStateWriter struct {
	owner postgres.WorkspaceWriter
}

func (w workspaceLifecycleStateWriter) UpdateWorkspaceSession(ctx context.Context, update workspacehub.Update) (workspacehub.Row, error) {
	row, err := w.owner.UpdateWorkspaceSession(ctx, postgres.WorkspaceUpdate(update))
	return workspacehub.Row(row), err
}

type workspaceLifecycleWriter struct {
	workspaceLifecycleReader
	workspaceLifecycleStateWriter
	owner postgres.WorkspaceWriter
}

func (w workspaceLifecycleWriter) CloseWorkspaceItem(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, at time.Time) error {
	return w.owner.CloseWorkspaceItem(ctx, project, item, at)
}
func (w workspaceLifecycleWriter) EndWorkspaceOccupancy(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, id, runner string, at time.Time) error {
	return w.owner.EndWorkspaceOccupancy(ctx, postgres.WorkspaceOccupancyEnd{OrganizationID: organization, ProjectID: project, WorkspaceID: id, RunnerID: runner, EndedAt: &at})
}

type workspaceLifecycleRepository struct{ server *Service }

func (r workspaceLifecycleRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.server.config.TenantRepository.ReadTenant(ctx, organization, workload, read)
}
func (r workspaceLifecycleRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.server.config.TenantRepository.WriteTenant(ctx, organization, workload, write)
}

func (w *workspaceService) newLifecycle() *workspacehub.Service {
	return workspacehub.New(workspacehub.Deps{
		Repository:   workspaceLifecycleRepository{w.server},
		Organization: w.server.accessOrganization,
		Now:          w.now,
		Reader: func(r tenantstore.Reader) workspacehub.Reader {
			return workspaceLifecycleReader{r.(postgres.WorkspaceReader)}
		},
		Writer: func(w tenantstore.Writer) workspacehub.Writer {
			owner := w.(postgres.WorkspaceWriter)
			return workspaceLifecycleWriter{workspaceLifecycleReader: workspaceLifecycleReader{owner}, workspaceLifecycleStateWriter: workspaceLifecycleStateWriter{owner}, owner: owner}
		},
		PrepareIssueClose: func(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, item string, now time.Time) (func(context.Context, tenantstore.Writer) error, error) {
			scope := nativeScope{organization: organization, project: project, sourceActor: &tracker.Actor{Kind: "system", PrincipalID: "workspace_service"}}
			plan, err := w.server.prepareWorkspaceIssueClose(ctx, scope, item, now)
			return plan.write, err
		},
		Committed: func(ctx context.Context, record workspacehub.Record) { w.committed(ctx, workspaceRecord(record)) },
	})
}
func (w *workspaceService) lifecycleService() *workspacehub.Service {
	if w.lifecycle != nil {
		return w.lifecycle
	}
	return w.newLifecycle()
}
func workspaceRecords(rows []workspacehub.Record) []workspaceRecord {
	var result []workspaceRecord
	for _, row := range rows {
		result = append(result, workspaceRecord(row))
	}
	return result
}
func (w *workspaceService) workspacePage(ctx context.Context, after time.Time, id string) ([]workspaceRecord, error) {
	rows, err := w.lifecycleService().Page(ctx, after, id)
	return workspaceRecords(rows), fromFeatureError(err)
}
func (w *workspaceService) dueWorkspaces(ctx context.Context, now time.Time) ([]workspaceRecord, error) {
	rows, err := w.lifecycleService().Due(ctx, now)
	return workspaceRecords(rows), fromFeatureError(err)
}
func (w *workspaceService) applyDeadline(ctx context.Context, record workspaceRecord, now time.Time) (workspaceRecord, bool, error) {
	row, changed, err := w.lifecycleService().ApplyDeadline(ctx, workspacehub.Record(record), now)
	return workspaceRecord(row), changed, fromFeatureError(err)
}
func (w *workspaceService) applyWorkspaceLifecycle(ctx context.Context, record workspaceRecord, now time.Time, recovery bool) (workspaceRecord, bool, error) {
	row, changed, err := w.lifecycleService().ApplyLifecycle(ctx, workspacehub.Record(record), now, recovery)
	return workspaceRecord(row), changed, fromFeatureError(err)
}
func (w *workspaceService) recoverOpenWorkspaces(ctx context.Context) error {
	return fromFeatureError(w.lifecycleService().RecoverOpen(ctx))
}
func updateWorkspaceState(ctx context.Context, owner postgres.WorkspaceWriter, row, expected postgres.WorkspaceSession, target, reason string, now time.Time) (postgres.WorkspaceSession, error) {
	updated, err := workspacehub.UpdateState(ctx, workspaceLifecycleStateWriter{owner}, workspacehub.Row(row), workspacehub.Row(expected), target, reason, now)
	return postgres.WorkspaceSession(updated), fromFeatureError(err)
}

func workspaceClaimable(record workspaceRecord, capabilities workspacesession.Capabilities, fresh bool, runnerID, retainedRunner, runnerIsolation, configuredIsolation string) (bool, string) {
	return workspacehub.Claimable(workspacehub.Record(record), capabilities, fresh, runnerID, retainedRunner, runnerIsolation, configuredIsolation)
}
func terminalIsolationAllowed(requires []string, configured, reported string) bool {
	return workspacehub.TerminalIsolationAllowed(requires, configured, reported)
}
