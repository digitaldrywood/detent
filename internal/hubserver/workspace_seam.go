package hubserver

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacerelay"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type workspaceRecord workspacehub.Record

func (r workspaceRecord) owner() workspacesession.Owner { return workspacehub.Record(r).Owner() }
func workspaceTenantRecord(row postgres.WorkspaceSession) workspaceRecord {
	return workspaceRecord(workspacehub.TenantRecord(workspacehub.Row(row)))
}

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
		Reader: func(r tenantstore.Reader) (workspacehub.Reader, error) {
			reader, ok := r.(postgres.WorkspaceReader)
			if !ok {
				return nil, errors.New("tenant reader does not support workspace reads")
			}
			return workspaceLifecycleReader{reader}, nil
		},
		Writer: func(w tenantstore.Writer) (workspacehub.Writer, error) {
			owner, ok := w.(postgres.WorkspaceWriter)
			if !ok {
				return nil, errors.New("tenant writer does not support workspace writes")
			}
			return workspaceLifecycleWriter{workspaceLifecycleReader: workspaceLifecycleReader{owner}, workspaceLifecycleStateWriter: workspaceLifecycleStateWriter{owner}, owner: owner}, nil
		},
		PrepareIssueClose: func(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, item string, now time.Time) (func(context.Context, tenantstore.Writer) error, error) {
			scope := hubscope.NativeScope{Organization: organization, Project: project, SourceActor: &tracker.Actor{Kind: "system", PrincipalID: "workspace_service"}}
			plan, err := w.server.prepareWorkspaceIssueClose(ctx, scope, item, now)
			return plan.Write, err
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
func (w *workspaceService) dueWorkspaces(ctx context.Context, now time.Time) ([]workspaceRecord, error) {
	rows, err := w.lifecycleService().Due(ctx, now)
	return workspaceRecords(rows), hubapi.FromFeatureError(err)
}
func (w *workspaceService) applyDeadline(ctx context.Context, record workspaceRecord, now time.Time) (workspaceRecord, bool, error) {
	row, changed, err := w.lifecycleService().ApplyDeadline(ctx, workspacehub.Record(record), now)
	return workspaceRecord(row), changed, hubapi.FromFeatureError(err)
}
func (w *workspaceService) recoverOpenWorkspaces(ctx context.Context) error {
	return hubapi.FromFeatureError(w.lifecycleService().RecoverOpen(ctx))
}
func updateWorkspaceState(ctx context.Context, owner postgres.WorkspaceWriter, row, expected postgres.WorkspaceSession, target, reason string, now time.Time) (postgres.WorkspaceSession, error) {
	updated, err := workspacehub.UpdateState(ctx, workspaceLifecycleStateWriter{owner}, workspacehub.Row(row), workspacehub.Row(expected), target, reason, now)
	return postgres.WorkspaceSession(updated), hubapi.FromFeatureError(err)
}

func workspaceClaimable(record workspaceRecord, capabilities workspacesession.Capabilities, fresh bool, runnerID, retainedRunner, runnerIsolation, configuredIsolation string) (bool, string) {
	return workspacehub.Claimable(workspacehub.Record(record), capabilities, fresh, runnerID, retainedRunner, runnerIsolation, configuredIsolation)
}
func terminalIsolationAllowed(requires []string, configured, reported string) bool {
	return workspacehub.TerminalIsolationAllowed(requires, configured, reported)
}

type execRunState = workspacerelay.ExecRun
type terminalStreamState = workspacerelay.TerminalStream
type relayStream = workspacerelay.Stream
type relayOutbound = workspacerelay.Outbound
type relayConnection = workspacerelay.Connection

const (
	hubStreamOwner    = workspacerelay.HubStreamOwner
	relayWriteQueue   = workspacerelay.WriteQueue
	relayWriteTimeout = workspacerelay.WriteTimeout
	relayReadLimit    = workspacerelay.ReadLimit
)

type workspaceRelay struct {
	*workspacerelay.Relay
	mu             sync.Mutex
	actionSweep    postgres.MaintenanceCursor
	actionDispatch postgres.MaintenanceCursor
	ticketSweep    postgres.MaintenanceCursor
}

func newWorkspaceRelay(service *workspaceService) *workspaceRelay {
	return &workspaceRelay{Relay: workspacerelay.New(workspacerelay.Deps{MemoryBytes: service.config.RelayMemoryBytes, FailExecRuns: service.failExecRuns, FinishTerminalRecordings: service.finishTerminalRecordings, RunReason: service.workspaceRunReason})}
}

func (w *workspaceService) workspaceRunReason(ctx context.Context, workspaceID string, states []*execRunState) string {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), relayWriteTimeout)
	defer cancel()
	if len(states) == 0 {
		return workspacesession.RunReasonWorkspaceEnd
	}
	state := states[0]
	var record postgres.WorkspaceSession
	err := w.server.accessRead(readCtx, state.Organization, func(reader tenantstore.Reader) error {
		owner, ok := reader.(postgres.WorkspaceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		record, err = owner.WorkspaceSession(readCtx, state.Project, workspaceID)
		return err
	})
	if err != nil {
		return workspacesession.RunReasonWorkspaceEnd
	}
	return actionRunReasonFor(record.Reason)
}

func (r *workspaceRelay) stop(ctx context.Context) { r.Stop(ctx) }
func (r *workspaceRelay) closeWorkspace(ctx context.Context, workspaceID, reason string) {
	r.CloseWorkspace(ctx, workspaceID, reason)
}
func (r *workspaceRelay) sweep(ctx context.Context, now time.Time) { r.Sweep(ctx, now) }
func (r *workspaceRelay) attachPerson(connection *relayConnection) error {
	return r.AttachPerson(connection)
}
func (r *workspaceRelay) attachRunner(connection *relayConnection) (*relayConnection, error) {
	return r.AttachRunner(connection)
}
func (r *workspaceRelay) runnerFor(workspaceID string) *relayConnection {
	return r.RunnerFor(workspaceID)
}
func (r *workspaceRelay) openHubStream(workspaceID, channel string) (*relayStream, string) {
	return r.OpenHubStream(workspaceID, channel)
}
func (r *workspaceRelay) closeStream(ctx context.Context, workspaceID, id string) {
	r.CloseStream(ctx, workspaceID, id)
}
func (r *workspaceRelay) nextToRunner(stream *relayStream) int64 { return r.NextToRunner(stream) }
func (r *workspaceRelay) detachedCount(workspaceID string) int {
	return r.DetachedCount(workspaceID)
}
func (r *workspaceRelay) memoryUsed() int64 { return r.MemoryUsed() }
func (r *workspaceRelay) beginExecRun(workspaceID, streamID string, state *execRunState, runner *relayConnection) bool {
	return r.BeginExecRun(workspaceID, streamID, state, runner)
}

type TerminalRecording = workspacerelay.TerminalRecording
type terminalRecordingRecord = workspacerelay.RecordingRecord
