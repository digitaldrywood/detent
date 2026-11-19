package hubserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacerelay"
	"github.com/digitaldrywood/detent/internal/workspacesession"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
	"github.com/digitaldrywood/detent/internal/localboard"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) localBoardDeps() coreimport.LocalBoardDeps {
	return coreimport.LocalBoardDeps{
		Repository:                    s.config.TenantRepository,
		Clock:                         s.config.Clock,
		Plans:                         s.database.plansService,
		InvalidAPIRequest:             s.invalidAPIRequest,
		NativeAPIError:                s.nativeAPIError,
		AuthorizeWorkItemRecordScope:  s.authorizeWorkItemRecordScope,
		HydrateWorkItemRecord:         s.hydrateWorkItemRecord,
		PlanWorkItemRecord:            s.planWorkItemRecord,
		PrepareWorkItemFields:         s.prepareWorkItemFields,
		RunDetailStore:                s.runDetailStore,
		RecheckProjectRecordMutation:  s.recheckProjectRecordMutation,
		BeginProjectRecordAuthority:   s.beginProjectRecordAuthority,
		RecordEntryAccessKeyAuditWith: s.recordEntryAccessKeyAuditWith,
		ValidateWorkItemCreate:        validateWorkItemRecordCreateInput,
		ValidateLinkedRepository: func(record projectstore.Record, repository string) error {
			return validateLinkedIssueRepository(projectIntegrationRecord(record), repository)
		},
		IsRunnerBudget: func(ctx context.Context) bool {
			return ctx.Value(runnerRequestBudgetContext{}) == true
		},
		NotifyBoard: func() {
			select {
			case s.boardRefresh <- struct{}{}:
			default:
			}
		},
	}
}

func (s *Service) importLocalBoard(c echo.Context) error {
	return s.localBoardDeps().ImportLocalBoard(c)
}

func (s *Service) importLocalBoardRecord(ctx context.Context, scope hubscope.NativeScope, request localboard.Request) (localboard.Result, error) {
	return s.localBoardDeps().ImportLocalBoardRecord(ctx, scope, request)
}

const backgroundStorageBudget = storageconversion.BackgroundBudget
const storageConversionRows = storageconversion.MaxRows

func (s *Service) convertPostgresStorage(ctx context.Context, tenant hubmaintenance.Tenant, limit int) (int, error) {
	return storageconversion.ConvertTenant(ctx, tenant, limit, func(organization tracker.OrganizationID) storageconversion.Storage {
		return postgres.NewConversionStorage(s.database.postgres, organization, s.runDetailStore(), func(ctx context.Context, _ storageconversion.Candidate, raw []byte) ([]byte, error) {
			return hubcontent.ResolveStorageJSON(s.freshRunDetailContext(ctx), raw)
		})
	})
}

type relayPrincipal struct {
	credential    hubapi.APICredential
	sessionID     string
	subject       string
	principalID   string
	supportReason string
}

func (p relayPrincipal) toRelay() workspacerelay.Principal {
	return workspacerelay.Principal{Credential: p.credential, SessionID: p.sessionID, Subject: p.subject, PrincipalID: p.principalID, SupportReason: p.supportReason}
}

type workspaceRelayTicketResponse = workspacerelay.TicketResponse

func relayRunnerDeps() workspacerelay.HTTPDeps {
	return workspacerelay.HTTPDeps{
		RunnerForLease: func(ctx context.Context, reader tenantstore.Reader, id string) (runnerstore.Identity, error) {
			return (&nativeLeaseCommandReader{Reader: reader, compact: true}).RunnerForLease(ctx, id)
		},
		RunnerMatchesLease: runnerIdentityMatchesHeldLease,
		RunnerExcluded:     runnerExcluded,
	}
}

func (s *Service) relayHTTP(w *workspaceService) *workspacerelay.HTTP {
	deps := relayRunnerDeps()
	deps.AccessRead = s.accessRead
	deps.AccessWrite = s.accessWrite
	deps.AuthInput = s.authInput
	deps.AuthorizeAccessScope = s.authorizeAccessScope
	deps.AuthorizeSubject = workspaceReadableSubject
	deps.RequireWorkspaces = func() error {
		_, err := s.requireWorkspaces()
		return err
	}
	deps.NativeAPIError = s.nativeAPIError
	deps.Repository = func() tenantstore.Repository { return s.config.TenantRepository }
	deps.Clock = func() time.Time { return s.config.Clock() }
	deps.ActorLogger = func() *slog.Logger { return s.config.Logger }
	deps.Hosted = func() bool { return s.config.Hosted != nil }
	deps.PublicURL = func() string { return s.config.Hosted.PublicURL }
	deps.CookieName = hostedCookie
	deps.Authenticate = func(ctx context.Context, value string) (auth.Session, error) {
		if s.hostedSessions == nil {
			return auth.Session{}, auth.ErrInvalidSession
		}
		return s.hostedSessions.Authenticate(ctx, value)
	}
	deps.HostedSessionCredential = s.hostedSessionCredential
	deps.AppOrganizationID = s.relayTicketOrganization
	deps.AuthorityTicker = newRelayAuthorityTicker
	deps.ContentContext = s.contentContext
	deps.RunDetailStore = s.runDetailStore
	deps.PrepareMeteredContent = prepareMeteredContent
	deps.ActionRunReason = actionRunReasonFor
	deps.ActionRunClaimed = errActionRunClaimed
	if w != nil {
		deps.TerminalConfig = func() workspacehub.TerminalConfig { return w.config.Terminal }
		deps.Logger = w.logger
		if w.relay != nil {
			deps.Relay = w.relay.Relay
			deps.TicketCursor = func() string {
				w.relay.mu.Lock()
				defer w.relay.mu.Unlock()
				return w.relay.ticketSweepAfter
			}
			deps.SetTicketCursor = func(cursor string) {
				w.relay.mu.Lock()
				w.relay.ticketSweepAfter = cursor
				w.relay.mu.Unlock()
			}
		}
	}
	return &workspacerelay.HTTP{HTTPDeps: deps}
}

func (w *workspaceService) relayHTTP() *workspacerelay.HTTP { return w.server.relayHTTP(w) }

func (s *Service) openWorkspaceRelay(c echo.Context) error {
	w, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return w.relayHTTP().OpenWorkspaceRelay(c)
}

func (s *Service) relayActor(ctx context.Context, organization tracker.OrganizationID, connection *relayConnection, principal relayPrincipal) workspacesession.Actor {
	return s.relayHTTP(nil).RelayActor(ctx, organization, connection, principal.toRelay())
}

func (w *workspaceService) revalidatePerson(ctx context.Context, c echo.Context, scope hubscope.NativeScope, record workspaceRecord, connection *relayConnection) error {
	return w.relayHTTP().RevalidatePerson(ctx, c, scope, workspacehub.Record(record), connection)
}

func (w *workspaceService) revalidatePersonLocally(ctx context.Context, record workspaceRecord, connection *relayConnection) error {
	return w.relayHTTP().RevalidatePersonLocally(ctx, workspacehub.Record(record), connection)
}

func (w *workspaceService) refuseTerminal(ctx context.Context, record workspaceRecord, connection *relayConnection, frame workspacesession.Frame) string {
	return w.relayHTTP().RefuseTerminal(ctx, workspacehub.Record(record), connection, frame)
}

func (w *workspaceService) refuseRelayWrite(ctx context.Context, record workspaceRecord, connection *relayConnection, frame workspacesession.Frame) string {
	return w.relayHTTP().RefuseRelayWrite(ctx, workspacehub.Record(record), connection, frame)
}

func (w *workspaceService) handlePersonFrame(ctx context.Context, connection *relayConnection, record workspaceRecord, frame workspacesession.Frame) {
	w.relayHTTP().HandlePersonFrame(ctx, connection, workspacehub.Record(record), frame)
}

func (w *workspaceService) channelPermitted(record workspaceRecord, channel string) bool {
	return w.relayHTTP().ChannelPermitted(workspacehub.Record(record), channel)
}

func (w *workspaceService) touchWorkspace(ctx context.Context, record workspaceRecord) {
	w.relayHTTP().TouchWorkspace(ctx, workspacehub.Record(record))
}

func (s *Service) openWorkspaceWorkerRelay(c echo.Context) error {
	w, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return w.relayHTTP().OpenWorkspaceWorkerRelay(c)
}

func (w *workspaceService) handleRunnerFrame(ctx context.Context, connection *relayConnection, frame workspacesession.Frame) {
	w.relayHTTP().HandleRunnerFrame(ctx, connection, frame)
}

func (w *workspaceService) startExecRun(ctx context.Context, connection, runner *relayConnection, record workspaceRecord, stream *relayStream, frame workspacesession.Frame) string {
	return w.relayHTTP().StartExecRun(ctx, connection, runner, workspacehub.Record(record), stream, frame)
}

func (w *workspaceService) recordExecFrame(ctx context.Context, connection *relayConnection, stream *relayStream, frame workspacesession.Frame) {
	w.relayHTTP().RecordExecFrame(ctx, connection, stream, frame)
}

func (w *workspaceService) failExecRuns(ctx context.Context, states []*execRunState, reason string) {
	w.relayHTTP().FailExecRuns(ctx, states, reason)
}

func (w *workspaceService) openRelayAudit(ctx context.Context, record workspaceRecord, connection *relayConnection, principal relayPrincipal) error {
	return w.relayHTTP().OpenRelayAudit(ctx, workspacehub.Record(record), connection, principal.toRelay())
}

func (w *workspaceService) closeRelayAudit(ctx context.Context, record workspaceRecord, connection *relayConnection, reason string) {
	w.relayHTTP().CloseRelayAudit(ctx, workspacehub.Record(record), connection, reason)
}

func relayWorkspaceRecord(row postgres.WorkspaceSession) workspaceRecord {
	return workspaceRecord(workspacerelay.WorkspaceRecord(row))
}

func readRelayRunnerAuthority(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, leaseID string, now time.Time) (runnerstore.Identity, error) {
	return (&workspacerelay.HTTP{HTTPDeps: relayRunnerDeps()}).ReadRelayRunnerAuthority(ctx, reader, scope, leaseID, now)
}

func (w *workspaceService) claimRelayExecRun(ctx context.Context, record workspaceRecord, runner *relayConnection, payload workspacesession.ExecRun, claimant string) (*execRunState, string, string) {
	return w.relayHTTP().ClaimRelayExecRun(ctx, workspacehub.Record(record), runner, payload, claimant)
}

func (w *workspaceService) readRelayWorkspaceForActor(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, id string) (workspaceRecord, error) {
	record, err := w.relayHTTP().ReadRelayWorkspaceForActor(ctx, reader, scope, id)
	return workspaceRecord(record), err
}

func (w *workspaceService) prepareRelayOutput(ctx context.Context, organization tracker.OrganizationID, data []byte) (attachment.ArchiveReference, error) {
	return w.relayHTTP().PrepareRelayOutput(ctx, organization, data)
}

func actionRunExitCode(value *int) (pgtype.Int4, error) {
	return workspacerelay.ActionRunExitCode(value)
}

func (s *Service) retainRelayArchive(ctx context.Context, writer tenantstore.Writer, organization tracker.OrganizationID, project tracker.ProjectID, id, kind string, ref attachment.ArchiveReference, at time.Time) error {
	return workspacerelay.RetainRelayArchive(ctx, writer, organization, project, id, kind, ref, at)
}

func (s *Service) relayArchiveReference(ctx context.Context, organization tracker.OrganizationID, expected attachment.ArchiveReference) (attachment.ArchiveReference, error) {
	return s.relayHTTP(nil).RelayArchiveReference(ctx, organization, expected)
}

func (s *Service) relayPrincipal(ctx context.Context, c echo.Context) (relayPrincipal, error) {
	principal, err := s.relayHTTP(nil).ResolvePrincipal(ctx, c)
	return relayPrincipal{credential: principal.Credential, sessionID: principal.SessionID, subject: principal.Subject, principalID: principal.PrincipalID, supportReason: principal.SupportReason}, err
}

func relayTicketRequest(c echo.Context) bool {
	return workspacerelay.RelayTicketRequest(c)
}

func (s *Service) mintWorkspaceRelayTicket(c echo.Context) error {
	w, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return w.relayHTTP().MintWorkspaceRelayTicket(c)
}

func (w *workspaceService) sweepRelayTickets(ctx context.Context, now time.Time) error {
	return w.relayHTTP().SweepRelayTickets(ctx, now)
}

func (w *workspaceService) finishTerminalRecordings(ctx context.Context, states []*terminalStreamState) {
	w.relayHTTP().FinishTerminalRecordings(ctx, states)
}

func (w *workspaceService) finishTerminalRecording(ctx context.Context, state *terminalStreamState) {
	w.relayHTTP().FinishTerminalRecording(ctx, state)
}

func (s *Service) listWorkspaceTerminalRecordings(c echo.Context) error {
	return s.relayHTTP(nil).ListWorkspaceTerminalRecordings(c)
}

func (s *Service) getWorkspaceTerminalRecording(c echo.Context) error {
	return s.relayHTTP(nil).GetWorkspaceTerminalRecording(c)
}

func (s *Service) readRecording(ctx context.Context, scope hubscope.NativeScope, workspaceID, id string) (terminalRecordingRecord, error) {
	return s.relayHTTP(nil).ReadRecording(ctx, scope, workspaceID, id)
}

func (s *Service) readRecordings(ctx context.Context, scope hubscope.NativeScope, workspaceID, relaySessionID string, limit int) ([]TerminalRecording, error) {
	return s.relayHTTP(nil).ReadRecordings(ctx, scope, workspaceID, relaySessionID, limit)
}

func (s *Service) requireWorkspaces() (*workspaceService, error) {
	if s.workspaces == nil {
		return nil, hubapi.WorkspaceUnavailable()
	}
	return s.workspaces, nil
}

type workspaceRequest = workspacehub.Request
type workspaceListResponse = workspacehub.ListResponse

const workspaceItemLabel = workspacehub.ItemLabel

func allowWorkspaceLabel(ctx context.Context) context.Context {
	return workspacehub.AllowLabel(ctx)
}

func workspaceLabelAllowed(ctx context.Context) bool {
	return workspacehub.LabelAllowed(ctx)
}

func requireUnreservedLabels(ctx context.Context, labels []string) error {
	return workspacehub.RequireUnreservedLabels(ctx, labels)
}

func firstTerminalState(project tracker.NativeProject) (string, bool) {
	return workspacehub.FirstTerminalState(project)
}

func workspaceReadableSubject(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, id string) error {
	return workspacehub.ReadableSubject(ctx, reader, scope, id)
}

func nativeOperation(scope hubscope.NativeScope, method, suffix string) string {
	return workspacehub.NativeOperation(scope, method, suffix)
}

func (s *Service) workspaceOperations(w *workspaceService) *workspacehub.Operations {
	deps := workspacehub.OperationDeps{
		RequireWorkspaces: func() error {
			_, err := s.requireWorkspaces()
			return err
		},
		Repository:                          func() tenantstore.Repository { return s.config.TenantRepository },
		AccessRepository:                    func() tenantstore.Repository { return s.config.AccessRepository },
		Clock:                               func() time.Time { return s.config.Clock() },
		Plans:                               s.database.plansService,
		InvalidAPIRequest:                   s.invalidAPIRequest,
		NativeAPIError:                      s.nativeAPIError,
		ReadNativeAttemptTenant:             s.readNativeAttemptTenant,
		HydrateActionRecord:                 s.hydrateActionRecord,
		RecheckProjectRecordMutation:        s.recheckProjectRecordMutation,
		AuthorizeWorkItemRecordScope:        s.authorizeWorkItemRecordScope,
		LoadWorkItemPlan:                    s.loadWorkItemPlan,
		PlanWorkItemRecord:                  s.planWorkItemRecord,
		ValidateWorkItemRecordFilingTenant:  s.validateWorkItemRecordFilingTenant,
		PrepareWorkItemAttachmentReferences: s.prepareWorkItemAttachmentReferences,
		PrepareWorkItemFields:               s.prepareWorkItemFields,
		RunDetailStore:                      s.runDetailStore,
		ReadProjectRecord:                   s.readProjectRecord,
		RecordEntryAccessKeyAuditWith:       s.recordEntryAccessKeyAuditWith,
		ChargeProjectCommand:                s.chargeProjectCommand,
		BeginProjectRecordAuthority:         s.beginProjectRecordAuthority,
		AccessRead:                          s.accessRead,
		AccessWrite:                         s.accessWrite,
		AuthorizeAccessScope:                s.authorizeAccessScope,
		FirstDispatchableState:              firstDispatchableState,
		ReadRunnerAuthority:                 readRelayRunnerAuthority,
		UpdateWorkspaceState:                updateWorkspaceState,
		WorkerIdentityFromHeaders: func(c echo.Context) (workspacehub.WorkerIdentity, error) {
			identity, err := workspacerelay.WorkerIdentityFromHeaders(c)
			return workspacehub.WorkerIdentity(identity), err
		},
		ValidActionRunReason:  validActionRunReason,
		ForwardRunStatus:      forwardRunStatus,
		ActionRunExitCode:     actionRunExitCode,
		RelayArchiveReference: s.relayArchiveReference,
		RetainRelayArchive:    s.retainRelayArchive,
		ActionRunResource: func(ctx context.Context, action postgres.ActionRun, output bool) (workspacesession.Run, error) {
			run, err := s.actionRunRecord(ctx, action, output)
			return run.resource(), err
		},
		PrepareCancellation: func(ctx context.Context, scope hubscope.NativeScope, id string, mutation tracker.NativeLeaseMutation) (workspacehub.PreparedCancellation, error) {
			prepared, err := s.prepareNativeLeaseCancellationTenant(ctx, scope, id, mutation)
			if err != nil || prepared == nil {
				return workspacehub.PreparedCancellation{}, err
			}
			return workspacehub.PreparedCancellation{HasAttempt: prepared.attempt.ID != "", Record: func(ctx context.Context, writer tenantstore.Writer, scope hubscope.NativeScope, lease leasestore.Lease) error {
				return s.recordPreparedLeaseCancellation(ctx, writer, scope, lease, prepared)
			}}, nil
		},
	}
	if w != nil {
		deps.Config = func() workspacehub.Config { return w.config }
		deps.Committed = func(ctx context.Context, record workspacehub.Record) { w.committed(ctx, workspaceRecord(record)) }
		deps.ReadRelayWorker = func(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, id string, identity workspacehub.WorkerIdentity) (workspacehub.Record, error) {
			return w.relayHTTP().ReadRelayWorker(ctx, reader, scope, id, workspacerelay.WorkerIdentity(identity))
		}
		deps.PrepareRelayOutput = w.prepareRelayOutput
	}
	return workspacehub.NewOperations(deps)
}

func (s *Service) commandCreateWorkspace(ctx context.Context, scope hubscope.NativeScope, request workspaceRequest) (json.RawMessage, error) {
	return s.workspaceOperations(s.workspaces).CreateRecord(ctx, scope, request)
}
func (s *Service) commandCloseWorkspace(ctx context.Context, scope hubscope.NativeScope, id string) (json.RawMessage, error) {
	return s.workspaceOperations(s.workspaces).CloseRecord(ctx, scope, id)
}
func (s *Service) readWorkspace(ctx context.Context, scope hubscope.NativeScope, id string) (json.RawMessage, error) {
	return s.workspaceOperations(s.workspaces).ReadRecord(ctx, scope, id)
}
func (s *Service) readWorkspaces(ctx context.Context, scope hubscope.NativeScope, item, state string, limit int) (json.RawMessage, error) {
	return s.workspaceOperations(s.workspaces).ReadRecords(ctx, scope, item, state, limit)
}
func (w *workspaceService) presentWorkspaceRecord(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, row postgres.WorkspaceSession) (workspacesession.Session, error) {
	return w.server.workspaceOperations(w).PresentRecord(ctx, r, scope, row)
}
func (s *Service) prepareWorkspaceIssueClose(ctx context.Context, scope hubscope.NativeScope, item string, now time.Time) (workspacehub.IssueClosePlan, error) {
	return s.workspaceOperations(nil).PrepareIssueClose(ctx, scope, item, now)
}
func (s *Service) requireOperatorRunners(ctx context.Context, query hubapi.NativeQueryer, scope hubscope.NativeScope) error {
	return s.workspaceOperations(nil).RequireOperatorRunners(ctx, query, scope)
}
func (s *Service) createWorkspace(c echo.Context) error {
	return s.workspaceOperations(s.workspaces).Create(c)
}
func (s *Service) listWorkspaces(c echo.Context) error {
	return s.workspaceOperations(s.workspaces).List(c)
}
func (s *Service) getWorkspace(c echo.Context) error {
	return s.workspaceOperations(s.workspaces).Get(c)
}
func (s *Service) deleteWorkspace(c echo.Context) error {
	return s.workspaceOperations(s.workspaces).Delete(c)
}
func (s *Service) bindWorkspaceWorker(c echo.Context) error {
	return s.workspaceOperations(s.workspaces).BindWorker(c)
}
func (s *Service) heartbeatWorkspaceWorker(c echo.Context) error {
	return s.workspaceOperations(s.workspaces).HeartbeatWorker(c)
}
func (s *Service) unbindWorkspaceWorker(c echo.Context) error {
	return s.workspaceOperations(s.workspaces).UnbindWorker(c)
}
func (s *Service) reportWorkspaceActionRun(c echo.Context) error {
	return s.workspaceOperations(s.workspaces).ReportActionRun(c)
}
func (s *Service) workspaceForWorkItem(c echo.Context) error {
	return s.workspaceOperations(s.workspaces).ForWorkItem(c)
}
