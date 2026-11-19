package hubserver

import (
	"context"
	"net/url"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimapi"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventbroker"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/executionapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerfleet"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func (s *Service) runnerRoutingDeps() runnerfleet.RoutingDeps {
	return runnerfleet.RoutingDeps{
		LandingCapacityKey:  landingCapacityKey,
		Repository:          s.config.TenantRepository,
		Now:                 s.config.Clock,
		HTTPError:           s.nativeAPIError,
		InvalidRequest:      s.invalidAPIRequest,
		Details:             s.runDetailStore,
		NotifyDispatch:      s.runnerNotifyDispatch(),
		AuthorizeAccess:     s.authorizeAccessScope,
		ManagementProjects:  runnerManagementProjects,
		FleetRecord:         runnerFromFleetRecord,
		AllRunnerGrants:     s.appAllRunnerGrants,
		AllRunnerGrantsWith: s.appAllRunnerGrantsWith,
		AuthorizeProject:    s.authorizeProjectRecord,
	}
}

func (s *Service) getRunnerRouting(c echo.Context) error {
	return s.runnerRoutingDeps().GetRunnerRouting(c)
}

type runnerProjectRankReader = runnerfleet.RunnerProjectRankReader

func (s *Service) readRunnerRouting(ctx context.Context, scope hubscope.NativeScope, runnerID string) (runnerauth.Runner, error) {
	return s.runnerRoutingDeps().ReadRunnerRouting(ctx, scope, runnerID)
}

type runnerRoutingRequest runnerfleet.RunnerRoutingRequest

func (request runnerRoutingRequest) effective(current runnerauth.Routing) runnerauth.RoutingChange {
	return runnerfleet.RunnerRoutingRequest(request).Effective(current)
}

func (s *Service) updateRunnerRouting(c echo.Context) error {
	return s.runnerRoutingDeps().UpdateRunnerRouting(c)
}

func (s *Service) updateRunnerRoutingCommand(ctx context.Context, scope hubscope.NativeScope, resource string, request runnerRoutingRequest) (any, error) {
	return s.runnerRoutingDeps().UpdateRunnerRoutingCommand(ctx, scope, resource, runnerfleet.RunnerRoutingRequest(request))
}

func (s *Service) updateRunnerHost(c echo.Context) error {
	return s.runnerRoutingDeps().UpdateRunnerHost(c)
}

func (s *Service) updateRunnerHostCommand(ctx context.Context, scope hubscope.NativeScope, resource string, change runnerauth.HostChange) (any, error) {
	return s.runnerRoutingDeps().UpdateRunnerHostCommand(ctx, scope, resource, change)
}

func (s *Service) requireRunnerAdministrationTenant(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope) error {
	return s.runnerRoutingDeps().RequireRunnerAdministrationTenant(ctx, reader, scope)
}

func runnerFromRoutingRecord(record runnerstore.RoutingRecord, now time.Time, urgent runnerstore.UrgentUpdate) runnerauth.Runner {
	return (runnerfleet.RoutingDeps{FleetRecord: runnerFromFleetRecord, LandingCapacityKey: landingCapacityKey}).FromRoutingRecord(record, now, urgent)
}

func (s *Service) listRunnerRouting(c echo.Context) error {
	return s.runnerRoutingDeps().ListRunnerRouting(c)
}

func (s *Service) listRunnerRoutingData(ctx context.Context, organization tracker.OrganizationID, limit, offset int) ([]runnerauth.Runner, error) {
	return s.runnerRoutingDeps().ListRunnerRoutingData(ctx, organization, limit, offset)
}

type routingPolicyKey = runnerfleet.RoutingPolicyKey

func readRoutingLeasePolicies(ctx context.Context, reader policystore.Reader, runners ...runnerauth.Runner) (map[routingPolicyKey]policystore.Revision, error) {
	return runnerfleet.ReadRoutingLeasePolicies(ctx, reader, runners...)
}

func (s *Service) pinRoutingLeasePolicies(ctx context.Context, rows map[routingPolicyKey]policystore.Revision, runners ...runnerauth.Runner) error {
	return s.runnerRoutingDeps().PinRoutingLeasePolicies(ctx, rows, runners...)
}

func runnerExcluded(exclusions []runnerauth.Exclusion) error {
	return runnerfleet.RunnerExcluded(exclusions)
}

func (s *Service) validateRunnerLease(c echo.Context) error {
	return s.runnerRoutingDeps().ValidateRunnerLease(c)
}

func runnerLeaseMutationRoute(path string) bool {
	return runnerfleet.RunnerLeaseMutationRoute(path)
}

func (s *Service) authorizeRunnerLeaseProject(ctx context.Context, scope hubscope.NativeScope) error {
	return s.runnerRoutingDeps().AuthorizeRunnerLeaseProject(ctx, scope)
}

func runnerIdentityMatchesHeldLease(identity runnerstore.Identity, organization tracker.OrganizationID, runnerID, tokenID string, machine tracker.MachineID, now time.Time) bool {
	return runnerfleet.RunnerIdentityMatchesHeldLease(identity, organization, runnerID, tokenID, machine, now)
}

func (s *Service) validateRunnerLeaseTenant(ctx context.Context, scope hubscope.NativeScope, leaseID tracker.LeaseID, fencing tracker.FencingToken) (runnerauth.Runner, error) {
	return s.runnerRoutingDeps().ValidateRunnerLeaseTenant(ctx, scope, leaseID, fencing)
}

var errLeaseSelectorUnbound = runnerfleet.ErrLeaseSelectorUnbound

func checkLeaseSelector(pinned policystore.LeasePolicy, identity runnerstore.Identity, legacyChecked bool) error {
	return runnerfleet.CheckLeaseSelector(pinned, identity, legacyChecked)
}

func (s *Service) checkLegacyLeaseSelector(ctx context.Context, scope hubscope.NativeScope, leaseID tracker.LeaseID) error {
	return s.runnerRoutingDeps().CheckLegacyLeaseSelector(ctx, scope, leaseID)
}

func (s *Service) runnerNotifyDispatch() func(tracker.OrganizationID) {
	if s.notifications == nil {
		return nil
	}
	return func(org tracker.OrganizationID) { s.notifications.notify(dispatchNotificationKey(org)) }
}

func (s *Service) runnerCapacityDeps() runnerfleet.CapacityDeps {
	return runnerfleet.CapacityDeps{
		Repository:        s.config.TenantRepository,
		Now:               s.config.Clock,
		HTTPError:         s.nativeAPIError,
		InvalidRequest:    s.invalidAPIRequest,
		Details:           s.runDetailStore,
		ReadRouting:       s.readRunnerRouting,
		AuthorizeCapacity: s.authorizeRunnerCapacity,
		PlansEnabled:      func() bool { return s.database.plansService().Plans() != nil },
		Entitlement: func(ctx context.Context, query hubapi.NativeQueryer, now time.Time) (hostedplans.HostedEntitlement, error) {
			return hubapi.FeatureResult(s.database.plansService().Entitlement(ctx, query, now))
		},
	}
}

type runnerCapacityChange = runnerfleet.RunnerCapacityChange

func (s *Service) persistRunnerCapacityCommand(ctx context.Context, scope hubscope.NativeScope, resource string, request runnerCapacityChange) (any, error) {
	return s.runnerCapacityDeps().PersistRunnerCapacityCommand(ctx, scope, resource, request)
}

func (s *Service) updateRunnerCapacity(c echo.Context) error {
	return s.runnerCapacityDeps().UpdateRunnerCapacity(c)
}

func (s *Service) runnerCapacityView(ctx context.Context, query hubapi.NativeQueryer, r runnerauth.Runner, backend string, now time.Time) (runnerauth.CapacityView, error) {
	return s.runnerCapacityDeps().RunnerCapacityView(ctx, query, r, backend, now)
}

func (s *Service) runnerIdentityDeps() runnerfleet.IdentityDeps {
	return runnerfleet.IdentityDeps{
		Repository:            s.config.TenantRepository,
		Now:                   s.config.Clock,
		HTTPError:             s.nativeAPIError,
		InvalidRequest:        s.invalidAPIRequest,
		NotifyDispatch:        s.runnerNotifyDispatch(),
		ApplyUsage:            s.applyRunnerTenantUsage,
		Heartbeat:             s.heartbeatNativeMachineTenant,
		RequireAdministration: s.requireRunnerAdministrationTenant,
	}
}

func (s *Service) getRunnerIdentity(c echo.Context) error {
	return s.runnerIdentityDeps().GetRunnerIdentity(c)
}

func (s *Service) renewRunnerIdentity(c echo.Context) error {
	return s.runnerIdentityDeps().RenewRunnerIdentity(c)
}

func (s *Service) rotateRunnerIdentity(c echo.Context) error {
	return s.runnerIdentityDeps().RotateRunnerIdentity(c)
}

func (s *Service) changeRunnerCredentialTenant(ctx context.Context, credential hubapi.APICredential, replacement string) (runnerauth.Identity, error) {
	return s.runnerIdentityDeps().ChangeRunnerCredentialTenant(ctx, credential, replacement)
}

func (s *Service) revokeRunnerIdentity(c echo.Context) error {
	return s.runnerIdentityDeps().RevokeRunnerIdentity(c)
}

func (s *Service) revokeRunnerIdentityCommand(ctx context.Context, scope hubscope.NativeScope, resource string) (any, error) {
	return s.runnerIdentityDeps().RevokeRunnerIdentityCommand(ctx, scope, resource)
}

func (s *Service) revokeRunnerIdentityTenant(ctx context.Context, w tenantstore.Writer, organization tracker.OrganizationID, resource, actorID string, clock func() time.Time) error {
	return s.runnerIdentityDeps().RevokeRunnerIdentityTenant(ctx, w, organization, resource, actorID, clock)
}

type nativeMachineHeartbeatRequest = runnerfleet.NativeMachineHeartbeatRequest

func (s *Service) heartbeatNativeMachine(c echo.Context) error {
	return s.runnerIdentityDeps().HeartbeatNativeMachine(c)
}

func validRunnerPlatform(os, architecture string) bool {
	return runnerfleet.ValidRunnerPlatform(os, architecture)
}

func (s *Service) readRunnerIdentityTenant(ctx context.Context, organization tracker.OrganizationID, runnerID string) (runnerauth.Identity, error) {
	return s.runnerIdentityDeps().ReadRunnerIdentityTenant(ctx, organization, runnerID)
}

func (s *Service) runnerDiagnosticDeps() runnerfleet.DiagnosticDeps {
	return runnerfleet.DiagnosticDeps{
		Repository:      s.config.TenantRepository,
		Now:             s.config.Clock,
		HTTPError:       s.nativeAPIError,
		AuthorizeAccess: s.authorizeAccessScope,
		DetailsFor:      s.runDetailStoreFor,
	}
}

func (s *Service) getRunnerProjectDiagnostics(c echo.Context) error {
	return s.runnerDiagnosticDeps().GetRunnerProjectDiagnostics(c)
}

func (s *Service) readRunnerProjectDiagnostics(ctx context.Context, scope hubscope.NativeScope, args operatortool.LocalProjectArguments) (runnerauth.DiagnosticPage, error) {
	return s.runnerDiagnosticDeps().ReadRunnerProjectDiagnostics(ctx, scope, args)
}

type preparedRunnerAdmissionDecisions = runnerfleet.PreparedRunnerAdmissionDecisions

func (s *Service) prepareRunnerAdmissionDecisions(ctx context.Context, credential hubapi.APICredential, view *runnerauth.ProjectConfiguration) (preparedRunnerAdmissionDecisions, error) {
	return s.runnerDiagnosticDeps().PrepareRunnerAdmissionDecisions(ctx, credential, view)
}

func appendRunnerAdmissionDecisions(ctx context.Context, w tenantstore.Writer, identity runnerstore.Identity, prepared preparedRunnerAdmissionDecisions, receivedAt time.Time) error {
	return runnerfleet.AppendRunnerAdmissionDecisions(ctx, w, identity, prepared, receivedAt)
}

func storeRunnerProjectObservationTenant(ctx context.Context, scope hubscope.NativeScope, view runnerauth.ProjectConfiguration, receivedAt time.Time, writer runnerstore.Writer) error {
	return runnerfleet.StoreRunnerProjectObservationTenant(ctx, pruneExpiredProjectDiagnostics, projectConfigurationDigest, scope, view, receivedAt, writer)
}

func (s *Service) runnerDiffDeps() executionapi.DiffDeps {
	return executionapi.DiffDeps{
		Repository:     s.config.TenantRepository,
		Now:            s.config.Clock,
		HTTPError:      s.nativeAPIError,
		InvalidRequest: s.invalidAPIRequest,
		Details:        s.runDetailStore,
		Unavailable:    nativeUnavailable,
		ArchiveJSON:    nativeArchiveJSON,
		HostedShared:   s.hostedShared,
		QuotaLimit:     s.attachmentQuotaLimit,
		ServiceClaims:  hostedSharedClaims,
	}
}

func (s *Service) checkAttemptDiff(c echo.Context) error {
	return s.runnerDiffDeps().CheckAttemptDiff(c)
}

func (s *Service) postAttemptDiff(c echo.Context) error {
	return s.runnerDiffDeps().PostAttemptDiff(c)
}

func (s *Service) getAttemptDiff(c echo.Context) error {
	return s.runnerDiffDeps().GetAttemptDiff(c)
}

func (s *Service) readAttemptDiff(ctx context.Context, scope hubscope.NativeScope, item, attemptID, source string, seq int64) (tracker.AttemptDiff, error) {
	return s.runnerDiffDeps().ReadAttemptDiff(ctx, scope, item, attemptID, source, seq)
}

func (s *Service) getWorkItemDiff(c echo.Context) error {
	return s.runnerDiffDeps().GetWorkItemDiff(c)
}

func (s *Service) readWorkItemDiff(ctx context.Context, scope hubscope.NativeScope, item, source string) (tracker.WorkItemDiff, error) {
	return s.runnerDiffDeps().ReadWorkItemDiff(ctx, scope, item, source)
}

func (s *Service) postAttemptDiffTenant(ctx context.Context, scope hubscope.NativeScope, attemptID string, request tracker.AttemptDiffRequest, checkOnly bool) (tracker.AttemptDiffReceipt, error) {
	return s.runnerDiffDeps().PostAttemptDiffTenant(ctx, scope, attemptID, request, checkOnly)
}

func (s *Service) readAttemptDiffTenant(ctx context.Context, scope hubscope.NativeScope, item, attemptID, source string, seq int64) (tracker.AttemptDiff, error) {
	return s.runnerDiffDeps().ReadAttemptDiffTenant(ctx, scope, item, attemptID, source, seq)
}

func (s *Service) readWorkItemDiffTenant(ctx context.Context, scope hubscope.NativeScope, item, source string) (tracker.WorkItemDiff, error) {
	return s.runnerDiffDeps().ReadWorkItemDiffTenant(ctx, scope, item, source)
}

func (s *Service) attemptDiffBodyExists(c echo.Context) error {
	return s.runnerDiffDeps().AttemptDiffBodyExists(c)
}

func (s *Service) manifestDiffBodyExists(ctx context.Context, key string) (bool, error) {
	return s.runnerDiffDeps().ManifestDiffBodyExists(ctx, key)
}

func (s *Service) runnerEvidenceDeps() executionapi.EvidenceDeps {
	return executionapi.EvidenceDeps{
		Repository:    s.config.TenantRepository,
		Now:           s.config.Clock,
		PlansEnabled:  func() bool { return s.database.plansService().Plans() != nil },
		ValidateLease: s.validateRunnerLeaseTenant,
		StaleLease:    nativeStaleLease,
	}
}

func (s *Service) checkAttachmentEvidence(c echo.Context, input *attachment.EvidenceRequest) (resultErr error) {
	return s.runnerEvidenceDeps().CheckAttachmentEvidence(c, input)
}

func escapeEvidenceCaption(caption string) string {
	return executionapi.EscapeEvidenceCaption(caption)
}

func (s *Service) checkAttachmentEvidenceTenant(ctx context.Context, scope hubscope.NativeScope, input attachment.EvidenceRequest) error {
	return s.runnerEvidenceDeps().CheckAttachmentEvidenceTenant(ctx, scope, input)
}

func (s *Service) publishEvidenceCompletion(ctx context.Context, w tenantstore.Writer, publication hubrecords.AttachmentEvidencePublication) error {
	return executionapi.PublishEvidenceCompletion(ctx, w, publication)
}

func (s *Service) runnerCommandDeps() executionapi.CommandDeps {
	return executionapi.CommandDeps{
		Repository:    s.config.TenantRepository,
		WriteClaim:    s.writeNativeClaimCommand,
		OwnsRunEvents: s.runnerOwnsRunEvents,
	}
}

func (s *Service) readNativeRunCommand(ctx context.Context, scope hubscope.NativeScope, command eventstore.RunCommand, read func(context.Context, tenantstore.Reader) error) error {
	return s.runnerCommandDeps().ReadNativeRunCommand(ctx, scope, command, read)
}

func (s *Service) writeNativeRunCommand(ctx context.Context, scope hubscope.NativeScope, command eventstore.RunCommand, write func(context.Context, tenantstore.Writer) error) error {
	return s.runnerCommandDeps().WriteNativeRunCommand(ctx, scope, command, write)
}

func lockRunnerCredential(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope) error {
	return executionapi.LockRunnerCredential(ctx, w, scope)
}

func runnerEventCommandRoute(c echo.Context) bool {
	return executionapi.RunnerEventCommandRoute(c)
}

func (s *Service) runnerEventCommandScope(c echo.Context, scope hubscope.NativeScope) bool {
	return s.runnerCommandDeps().RunnerEventCommandScope(c, scope)
}

func (s *Service) runnerSessionDeps() executionapi.SessionDeps {
	return executionapi.SessionDeps{
		ValidateDetailReference: validateNativeDetailReference,
		RegisterWorkerProcess:   registerSessionWorkerProcess,
		Repository:              s.config.TenantRepository,
		HTTPError:               s.nativeAPIError,
		InvalidRequest:          s.invalidAPIRequest,
		Details:                 s.runDetailStore,
		AuthorizeAccess:         s.authorizeAccessScope,
		ReadCommand:             s.readNativeRunCommand,
		WriteCommand:            s.writeNativeRunCommand,
	}
}

func (s *Service) readRunnerSessionPayload(ctx context.Context, record runnersession.Record) (runnersession.Snapshot, error) {
	return s.runnerSessionDeps().ReadRunnerSessionPayload(ctx, record)
}

func (s *Service) getRunnerSession(c echo.Context) error {
	return s.runnerSessionDeps().GetRunnerSession(c)
}

func (s *Service) findRunnerSession(c echo.Context) error {
	return s.runnerSessionDeps().FindRunnerSession(c)
}

func (s *Service) runnerSessionFence(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, request runnersession.Mutation, issue string, lock bool) (leasestore.Attempt, policystore.Revision, error) {
	return s.runnerSessionDeps().RunnerSessionFence(ctx, r, scope, request, issue, lock)
}

type heldRunnerSessionAuthority struct{ owner runnerstore.Identity }

func (s *Service) runnerSessionFenceHeld(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, request runnersession.Mutation, issue string, lock bool, held *heldRunnerSessionAuthority) (leasestore.Attempt, corestore.WorkItem, policystore.Revision, error) {
	var current *executionapi.HeldRunnerSessionAuthority
	if held != nil {
		current = &executionapi.HeldRunnerSessionAuthority{Owner: held.owner}
	}
	return s.runnerSessionDeps().RunnerSessionFenceHeld(ctx, r, scope, request, issue, lock, current)
}

func (s *Service) mutateRunnerSession(c echo.Context) error {
	return s.runnerSessionDeps().MutateRunnerSession(c)
}

func (s *Service) getRunnerSessionHistory(c echo.Context) error {
	return s.runnerSessionDeps().GetRunnerSessionHistory(c)
}

func (s *Service) getRunnerSessionProcesses(c echo.Context) error {
	return s.runnerSessionDeps().GetRunnerSessionProcesses(c)
}

func (s *Service) verifyRunnerSessionFinalization(ctx context.Context, scope hubscope.NativeScope, record runnersession.Record, snapshot runnersession.Snapshot) error {
	return s.runnerSessionDeps().VerifyRunnerSessionFinalization(ctx, scope, record, snapshot)
}

func (s *Service) runnerOwnsRunEvents() bool {
	switch s.config.NativeRunEvents.(type) {
	case tenantNativeRunEventParticipant, *tenantNativeRunEventParticipant:
		return true
	default:
		return false
	}
}

func runnerSessionProcessAuthority(key string) string {
	return executionapi.RunnerSessionProcessAuthority(key)
}

func (s *Service) runnerDependencyDeps() executionapi.DependencyDeps {
	return executionapi.DependencyDeps{
		Repository:      s.config.TenantRepository,
		Details:         s.runDetailStore,
		AuthorizeAccess: s.authorizeAccessScope,
		WriteClaim:      s.writeNativeClaimCommand,
		LoadPlan:        s.loadWorkItemPlan,
		PlanWorkItem:    s.planWorkItemRecord,
		PrepareFields:   s.prepareWorkItemFields,
	}
}

type preparedRunnerDependencies struct {
	state   *executionapi.PreparedRunnerDependencies
	final   corestore.WorkItem
	version eventstore.CollaborationVersion
}

func normalizeRunnerDependencyDisposition(data *tracker.NativeRunData, project projectstore.Metadata) {
	executionapi.NormalizeRunnerDependencyDisposition(data, project)
}

func (s *Service) prepareRunnerDependencies(ctx context.Context, scope hubscope.NativeScope, source corestore.WorkItem, event tracker.NativeRunEvent) (*preparedRunnerDependencies, bool, error) {
	state, changed, err := s.runnerDependencyDeps().PrepareRunnerDependencies(ctx, scope, source, event)
	if state == nil {
		return nil, changed, err
	}
	return &preparedRunnerDependencies{state: state, final: state.Final, version: state.Version}, changed, err
}

func (s *Service) writeRunnerDependenciesCommand(ctx context.Context, scope hubscope.NativeScope, command eventstore.RunCommand, dependencies *preparedRunnerDependencies, write func(context.Context, tenantstore.Writer) error) error {
	return s.runnerDependencyDeps().WriteRunnerDependenciesCommand(ctx, scope, command, dependencies.state, write)
}

func applyRunnerDependencies(ctx context.Context, w tenantstore.Writer, dependencies *preparedRunnerDependencies) error {
	return executionapi.ApplyRunnerDependencies(ctx, w, dependencies.state)
}

func (s *Service) runnerSpendDeps() executionapi.SpendDeps {
	return executionapi.SpendDeps{
		Repository:      s.config.TenantRepository,
		HTTPError:       s.nativeAPIError,
		AuthorizeAccess: s.authorizeAccessScope,
	}
}

func (s *Service) getRunnerSpend(c echo.Context) error {
	return s.runnerSpendDeps().GetRunnerSpend(c)
}

func (s *Service) getRunnerModelQuantiles(c echo.Context) error {
	return s.runnerSpendDeps().GetRunnerModelQuantiles(c)
}

func (s *Service) runnerUsageDeps() executionapi.UsageDeps {
	return executionapi.UsageDeps{
		Repository:       s.config.TenantRepository,
		HTTPError:        s.nativeAPIError,
		AuthorizeAccess:  s.authorizeAccessScope,
		ReadAttempt:      s.readNativeAttemptTenant,
		Plans:            func() *hostedplans.HostedPlansConfig { return s.database.plansService().Plans() },
		Entitlement:      s.runnerUsageEntitlement,
		HasRequestBudget: func(ctx context.Context) bool { return ctx.Value(runnerRequestBudgetContext{}) == true },
	}
}

func (s *Service) accountRunnerUsage(c echo.Context) error {
	return s.runnerUsageDeps().AccountRunnerUsage(c)
}

func (s *Service) applyRunnerTenantUsage(ctx context.Context, writer billingstore.Writer, now time.Time, receiptID string, completion, heartbeat bool) error {
	return s.runnerUsageDeps().ApplyRunnerTenantUsage(ctx, writer, now, receiptID, completion, heartbeat)
}

func (s *Service) checkRunnerAdmission(ctx context.Context, writer billingstore.Writer, now time.Time, counted int64, metrics ...string) error {
	return s.runnerUsageDeps().CheckRunnerAdmission(ctx, writer, now, counted, metrics...)
}

func (s *Service) runnerUsageEntitlement(ctx context.Context, r billingstore.Reader, now time.Time) (hostedplans.HostedEntitlement, error) {
	return hubapi.FeatureResult(s.database.plansService().EntitlementRecord(ctx, r, now))
}

func nativeClaimAnswerRequired(plan hubrecords.NativeClaimPlan) bool {
	return claimapi.NativeClaimAnswerRequired(plan)
}

func nativeClaimDispatchSatisfied(plan hubrecords.NativeClaimPlan) bool {
	return claimapi.NativeClaimDispatchSatisfied(plan)
}

func readRunnerRecoveryEligibility(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, item tracker.NativeWorkItemID) (bool, bool, error) {
	return claimapi.ReadRunnerRecoveryEligibility(ctx, reader, scope, item)
}

func (s *Service) runnerAttemptFactsDeps() claimapi.AttemptFactsDeps {
	return claimapi.AttemptFactsDeps{
		ReadAttempt:    s.readNativeAttemptRunTenant,
		ReadCheckpoint: s.readNativeAttemptCheckpointTenant,
		AttemptStatus:  nativeRunAttemptStatus,
	}
}

func nativeAttemptClaimDisposition(data tracker.NativeRunData, checkpoint *tracker.NativeCheckpoint, project tracker.ProjectID) string {
	return claimapi.NativeAttemptClaimDisposition(data, checkpoint, project)
}

func attemptClaimFacts(attempt leasestore.Attempt, disposition string, unresolvedOnly bool) leasestore.AttemptClaimFacts {
	return claimapi.AttemptClaimFacts(attempt, disposition, unresolvedOnly)
}

func (s *Service) prepareAttemptClaimFacts(ctx context.Context, snapshot hubrecords.TenantAttemptSnapshot) (leasestore.AttemptClaimFacts, error) {
	return s.runnerAttemptFactsDeps().PrepareAttemptClaimFacts(ctx, snapshot)
}

func answeredClaimKind(data tracker.NativeRunData, checkpoint *tracker.NativeCheckpoint) string {
	return claimapi.AnsweredClaimKind(data, checkpoint)
}

const claimPreparationLimit = claimapi.ClaimPreparationLimit

func providerWait(code, message string) error {
	return claimapi.ProviderWait(code, message)
}

func isProviderWait(err error) bool {
	return claimapi.IsProviderWait(err)
}

func validateProviderCandidates(candidates []tracker.NativeCapacityCandidate) error {
	return claimapi.ValidateProviderCandidates(candidates)
}

func validateProviderReports(reports []providercapacity.Report) error {
	return runnerfleet.ValidateProviderReports(reports)
}

func mergeRunnerProviderReports(previous, reports []providercapacity.Report, now time.Time) []providercapacity.Report {
	return runnerfleet.MergeRunnerProviderReports(previous, reports, now)
}

func validateTenantProviderAttempt(ctx context.Context, reader tenantstore.Reader, runnerID string, data tracker.NativeRunData, now time.Time) error {
	return claimapi.ValidateTenantProviderAttempt(ctx, reader, runnerID, data, now)
}

func (s *Service) runnerClaimDeps() claimapi.Deps {
	return claimapi.Deps{
		Repository:                       s.config.TenantRepository,
		Now:                              s.config.Clock,
		Details:                          s.runDetailStore,
		AuthorizeAccess:                  s.authorizeAccessScope,
		Logger:                           s.config.Logger,
		RunEventsEnabled:                 func() bool { return s.config.NativeRunEvents != nil },
		DetailsFor:                       s.runDetailStoreFor,
		PrepareCancellation:              s.prepareRunnerClaimCancellation,
		EvaluateContract:                 evaluateRunnerClaimContract,
		ErrPlacementSnapshotRequired:     errClaimPlacementSnapshotRequired,
		ValidateRunnerVersion:            func(version string) error { return runnerVersionError(minimumRunnerVersion(s.config.Version), version) },
		PlansEnabled:                     func() bool { return s.database.plansService().Plans() != nil },
		Entitlement:                      s.runnerUsageEntitlement,
		WorkspaceOptions:                 s.runnerWorkspaceClaimOptions,
		WorkspaceClaimable:               runnerWorkspaceClaimable,
		AnsweredExclusion:                s.nativeClaimAnsweredExclusion,
		CurrentChangeDetail:              s.nativeClaimCurrentChangeDetail,
		ReadCheckpoint:                   s.readNativeAttemptCheckpointTenant,
		ReadPlacementSnapshot:            s.readPlacementSnapshot,
		LandingCapacityAvailable:         s.nativeClaimLandingCapacityAvailable,
		ProjectModel:                     s.projectRecordModel,
		PlacementAllowed:                 claimPlacementAllowed,
		CutoverSourceReference:           cutoverSourceReference,
		HydrateChangePolicy:              hydrateChangeSummaryPolicy,
		LandingCapacityKey:               landingCapacityKey,
		MonthlyBudgetRefusal:             monthlyBudgetRefusal,
		MonthlyCostExposure:              monthlyCostExposure,
		ChangeLandingClaimable:           nativeChangeLandingClaimable,
		ReadMonthlyBudget:                readBillingMonthlyBudgetInput,
		ReadCurrentChangePolicy:          readCurrentChangePolicyRows,
		ReadReleaseDeliveryFacts:         readNativeClaimReleaseDeliveryFacts,
		ReadReleaseDeliveryContractFacts: readReleaseDeliveryContractFacts,
		SpriteAllocationAllowed:          spriteAllocationRunnerAllowedFacts,
		RefusedAPIError:                  s.refusedAPIError,
	}
}

const nativeClaimCandidatePageSize = claimapi.NativeClaimCandidatePageSize

type nativeClaimCandidateScan struct {
	plans    []hubrecords.NativeClaimPlan
	replay   *hubrecords.NativeClaimPlan
	pageNext claimstore.CandidateCursor
	cursor   claimstore.SelectionCursor
}

func (s *Service) claimNativeIssueTenant(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeClaim, ttl time.Duration) (leasestore.Lease, error) {
	return s.runnerClaimDeps().ClaimNativeIssueTenant(ctx, scope, request, ttl)
}

func (s *Service) readNativeClaimCandidates(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeClaim, now time.Time) (nativeClaimCandidateScan, error) {
	scan, err := s.runnerClaimDeps().ReadNativeClaimCandidates(ctx, scope, request, now)
	return nativeClaimCandidateScan{plans: scan.Plans, replay: scan.Replay, pageNext: scan.PageNext, cursor: scan.Cursor}, err
}

func nativeClaimPlanFromBatch(shared hubrecords.NativeClaimPlan, candidate claimstore.Candidate, row tenantstore.ClaimPreviewPlanRow) hubrecords.NativeClaimPlan {
	return claimapi.NativeClaimPlanFromBatch(shared, candidate, row)
}

func (s *Service) claimNativeIssueTenantWithPlan(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeClaim, ttl time.Duration, now time.Time, plan hubrecords.NativeClaimPlan, cursor *claimstore.SelectionCursor) (leasestore.Lease, error) {
	return s.runnerClaimDeps().ClaimNativeIssueTenantWithPlan(ctx, scope, request, ttl, now, plan, cursor)
}

func (s *Service) writeNativeClaimCommand(ctx context.Context, scope hubscope.NativeScope, write func(context.Context, tenantstore.Writer) error) error {
	return s.runnerClaimDeps().WriteNativeClaimCommand(ctx, scope, write)
}

func (s *Service) persistNativeClaimCursor(ctx context.Context, scope hubscope.NativeScope, cursor claimstore.SelectionCursor) error {
	return s.runnerClaimDeps().PersistNativeClaimCursor(ctx, scope, cursor)
}

func claimCandidateCursor(candidate claimstore.Candidate) claimstore.CandidateCursor {
	return claimapi.ClaimCandidateCursor(candidate)
}

func selectTenantProviderReservation(ctx context.Context, w tenantstore.Writer, identity runnerstore.Identity, request tracker.NativeClaim, item tracker.NativeWorkItemID, joining string, now time.Time) (providercapacity.Reservation, error) {
	return claimapi.SelectTenantProviderReservation(ctx, w, identity, request, item, joining, now)
}

func readTenantProviderReservation(ctx context.Context, r claimstore.Reader, lease tracker.LeaseID) (claimstore.ProviderReservation, bool, error) {
	return claimapi.ReadTenantProviderReservation(ctx, r, lease)
}

func (s *Service) readNativeClaimPlan(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeClaim, now time.Time) (hubrecords.NativeClaimPlan, error) {
	return s.runnerClaimDeps().ReadNativeClaimPlan(ctx, scope, request, now)
}

func (s *Service) readNativeClaimPlanRows(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeClaim, now time.Time, reader tenantstore.Reader, candidate *claimstore.Candidate, shared hubrecords.NativeClaimPlan) (hubrecords.NativeClaimPlan, error) {
	return s.runnerClaimDeps().ReadNativeClaimPlanRows(ctx, scope, request, now, reader, candidate, shared)
}

func (s *Service) prepareNativeClaimApprovals(ctx context.Context, scope hubscope.NativeScope, plans []hubrecords.NativeClaimPlan) error {
	return s.runnerClaimDeps().PrepareNativeClaimApprovals(ctx, scope, plans)
}

func validateClaimSnapshotPolicy(scope hubscope.NativeScope, plan hubrecords.NativeClaimPlan) error {
	return claimapi.ValidateClaimSnapshotPolicy(scope, plan)
}

func (s *Service) hydrateNativeClaimPlan(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeClaim, now time.Time, plan hubrecords.NativeClaimPlan, descriptor *policy.Descriptor) (hubrecords.NativeClaimPlan, error) {
	return s.runnerClaimDeps().HydrateNativeClaimPlan(ctx, scope, request, now, plan, descriptor)
}

func nativeClaimLandingCapacityRequired(plan hubrecords.NativeClaimPlan) (bool, error) {
	return claimapi.NativeClaimLandingCapacityRequired(plan)
}

func nativeClaimPendingLanding(plan hubrecords.NativeClaimPlan) bool {
	return claimapi.NativeClaimPendingLanding(plan)
}

func sameNativeClaimChangeRecord(left, right eventstore.Change) bool {
	return claimapi.SameNativeClaimChangeRecord(left, right)
}

func sameNativeClaimReviewPolicy(left eventstore.ReviewPolicy, right eventstore.ReviewPolicy) bool {
	return claimapi.SameNativeClaimReviewPolicy(left, right)
}

func sameNativeClaimLinkedSource(left, right eventstore.LinkedSource) bool {
	return claimapi.SameNativeClaimLinkedSource(left, right)
}

func (s *Service) nativeWorkspaceClaimAllowed(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, item tracker.NativeWorkItemID, now time.Time) (bool, error) {
	return s.runnerClaimDeps().NativeWorkspaceClaimAllowed(ctx, r, scope, item, now)
}

func nativeTenantCandidateQuery(scope hubscope.NativeScope, request tracker.NativeClaim, item tracker.NativeWorkItemID, now time.Time) claimstore.CandidateQuery {
	return claimapi.NativeTenantCandidateQuery(scope, request, item, now)
}

func runnerIdentityClaimRefusal(identity runnerstore.Identity, scope hubscope.NativeScope, machine tracker.MachineID, now time.Time) error {
	return claimapi.RunnerIdentityClaimRefusal(identity, scope, machine, now)
}

func runnerIdentityCanClaim(identity runnerstore.Identity, scope hubscope.NativeScope, machine tracker.MachineID, now time.Time) bool {
	return claimapi.RunnerIdentityCanClaim(identity, scope, machine, now)
}

func nativeClaimProjectEnabled(project projectstore.Record) bool {
	return claimapi.NativeClaimProjectEnabled(project)
}

func nativeCompatibleClaimSource(project projectstore.Record, issue tracker.NativeIssue, candidate claimstore.Candidate) bool {
	return claimapi.NativeCompatibleClaimSource(cutoverSourceReference, project, issue, candidate)
}

func readNativeClaimChangeFacts(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, plans []hubrecords.NativeClaimPlan) error {
	return claimapi.ReadNativeClaimChangeFacts(ctx, r, scope, plans)
}

func (s *Service) prepareNativeClaimLegacyAuditFacts(ctx context.Context, scope hubscope.NativeScope, plans []hubrecords.NativeClaimPlan) error {
	return s.runnerClaimDeps().PrepareNativeClaimLegacyAuditFacts(ctx, scope, plans)
}

const claimSourceClassificationLimit = claimapi.ClaimSourceClassificationLimit

var ErrNoClaimableWork = claimapi.ErrNoClaimableWork

func (s *Service) trackerAPIError(c echo.Context, err error) error {
	return s.runnerClaimDeps().TrackerAPIError(c, err)
}

func (s *Service) prepareRunnerClaimCancellation(ctx context.Context, scope hubscope.NativeScope, item int64, includeReleased bool) (*leasestore.AttemptCancellation, error) {
	prepared, err := s.prepareExpiredNativeLeaseCancellation(ctx, scope, item, includeReleased)
	return prepared.attemptCancellation(), err
}

func evaluateRunnerClaimContract(plan hubrecords.NativeClaimPlan) (claimapi.ContractEvaluation, error) {
	evaluation, err := nativeRuntimeIssueContractEvaluation(plan)
	return claimapi.ContractEvaluation{Allowed: evaluation.Allowed, Reason: evaluation.Reason, ReasonCode: evaluation.ReasonCode}, err
}

func nativeClaimIssueContractAllowed(plan hubrecords.NativeClaimPlan) bool {
	evaluation, err := nativeRuntimeIssueContractEvaluation(plan)
	return err == nil && evaluation.Allowed
}

func (s *Service) runnerWorkspaceClaimOptions() (time.Duration, string, bool) {
	if s.workspaces == nil {
		return 0, "", false
	}
	return s.workspaces.config.RetainAfterRun, s.workspaces.config.Terminal.Isolation, true
}

func runnerWorkspaceClaimable(row postgres.WorkspaceSession, capabilities workspacesession.Capabilities, fresh bool, runnerID, retained, isolation, configuredIsolation string) (bool, string) {
	return workspaceClaimable(workspaceTenantRecord(row), capabilities, fresh, runnerID, retained, isolation, configuredIsolation)
}

func (s *Service) runnerAttemptDeps() executionapi.AttemptDeps {
	return executionapi.AttemptDeps{
		Repository:             s.config.TenantRepository,
		Now:                    s.config.Clock,
		HTTPError:              s.nativeAPIError,
		Details:                s.runDetailStore,
		AuthorizeAccess:        s.authorizeAccessScope,
		Logger:                 s.config.Logger,
		ReadQuery:              nativeReadQuery,
		ArchiveJSON:            nativeArchiveJSON,
		DetailContext:          s.runDetailContext,
		DetailsFor:             s.runDetailStoreFor,
		ReadPage:               s.readNativePage,
		PageOrder:              nativePageOrder,
		CleanupRequired:        nativeRunCleanupRequired,
		ReadLandingObservation: s.readNativeLandingObservation,
		FailureSummary:         attemptFailureSummary,
	}
}

func nativeExecutionConflict(message string) error {
	return executionapi.NativeExecutionConflict(message)
}

func nativeStaleLease(err error, message string) error {
	return executionapi.NativeStaleLease(err, message)
}

func validExecutionName(value string) bool {
	return executionapi.ValidExecutionName(value)
}

func validCheckpointRef(ref string) bool {
	return executionapi.ValidCheckpointRef(ref)
}

func validateNativeExecution(data tracker.NativeRunData, eventType string) error {
	return executionapi.ValidateNativeExecution(nativeRejection, nativeTerminationData, data, eventType)
}

func nativeRuntimeHistoryChanged(previous, observation *tracker.NativeRuntimeObservation) bool {
	return executionapi.NativeRuntimeHistoryChanged(nativeRuntimeSession, previous, observation)
}

func (s *Service) listNativeAttempts(c echo.Context) error {
	return s.runnerAttemptDeps().ListNativeAttempts(c)
}

type nativeAttemptPage struct {
	tracker.Page[tracker.NativeAttempt]
	encoded []byte
}

func (s *Service) readAttempts(ctx context.Context, scope hubscope.NativeScope, item string, params url.Values) (tracker.Page[tracker.NativeAttempt], error) {
	return s.runnerAttemptDeps().ReadAttempts(ctx, scope, item, params)
}

func (s *Service) readNativeAttemptPage(ctx context.Context, scope hubscope.NativeScope, item string, params url.Values, marshal func(any) ([]byte, error)) (nativeAttemptPage, error) {
	page, err := s.runnerAttemptDeps().ReadNativeAttemptPage(ctx, scope, item, params, marshal)
	return nativeAttemptPage{Page: page.Page, encoded: page.Encoded}, err
}

func nativeAttemptAuthorityProjection(attempt tracker.NativeAttempt) tracker.NativeAttempt {
	return executionapi.NativeAttemptAuthorityProjection(attempt)
}

func nativeAttemptDisplayIdentity(attempt tracker.NativeAttempt) *tracker.NativeExecutionIdentity {
	return executionapi.NativeAttemptDisplayIdentity(attempt)
}

func nativeRecordedAttemptIdentity(attempt leasestore.Attempt, runtime bool) (*tracker.NativeExecutionIdentity, bool) {
	return executionapi.NativeRecordedAttemptIdentity(attempt, runtime)
}

func nativeExecutionDisplayIdentity(execution *tracker.NativeExecutionIdentity, runtime *agentidentity.Identity) *tracker.NativeExecutionIdentity {
	return executionapi.NativeExecutionDisplayIdentity(execution, runtime)
}

func (s *Service) getNativeAttempt(c echo.Context) error {
	return s.runnerAttemptDeps().GetNativeAttempt(c)
}

func (s *Service) readNativeAttempt(ctx context.Context, scope hubscope.NativeScope, item, id string) (tracker.NativeAttempt, error) {
	return s.runnerAttemptDeps().ReadNativeAttempt(ctx, scope, item, id)
}

func nativeLeaseReleaseFailure(reason string, now time.Time) tracker.NativeTerminalFailure {
	return executionapi.NativeLeaseReleaseFailure(reason, now)
}

func (s *Service) readNativeAttemptTenant(ctx context.Context, scope hubscope.NativeScope, item, id string) (tracker.NativeAttempt, error) {
	return s.runnerAttemptDeps().ReadNativeAttemptTenant(ctx, scope, item, id)
}

func (s *Service) readNativeAttemptSnapshotTenant(ctx context.Context, scope hubscope.NativeScope, item, id string) (hubrecords.TenantAttemptSnapshot, error) {
	return s.runnerAttemptDeps().ReadNativeAttemptSnapshotTenant(ctx, scope, item, id)
}

func (s *Service) readNativeAttemptPageTenant(ctx context.Context, scope hubscope.NativeScope, item string, params url.Values, marshal func(any) ([]byte, error)) (nativeAttemptPage, error) {
	page, err := s.runnerAttemptDeps().ReadNativeAttemptPageTenant(ctx, scope, item, params, marshal)
	return nativeAttemptPage{Page: page.Page, encoded: page.Encoded}, err
}

func (s *Service) readNativeAttemptCheckpointTenant(ctx context.Context, snapshot hubrecords.TenantAttemptSnapshot) (*tracker.NativeCheckpoint, error) {
	return s.runnerAttemptDeps().ReadNativeAttemptCheckpointTenant(ctx, snapshot)
}

func (s *Service) hydrateNativeAttemptTenant(ctx context.Context, snapshot hubrecords.TenantAttemptSnapshot, now time.Time) (tracker.NativeAttempt, error) {
	return s.runnerAttemptDeps().HydrateNativeAttemptTenant(ctx, snapshot, now)
}

func nativeAttemptRunMatches(snapshot hubrecords.TenantAttemptSnapshot, run tracker.NativeRunData) bool {
	return executionapi.NativeAttemptRunMatches(snapshot, run)
}

func readTenantAttemptEvidence(ctx context.Context, r tenantstore.Reader, snapshot *hubrecords.TenantAttemptSnapshot) error {
	return executionapi.ReadTenantAttemptEvidence(ctx, r, snapshot)
}

func (s *Service) readNativeAttemptRunTenant(ctx context.Context, snapshot hubrecords.TenantAttemptSnapshot) (tracker.NativeRunData, error) {
	return s.runnerAttemptDeps().ReadNativeAttemptRunTenant(ctx, snapshot)
}

func nativeAttemptArchiveReadError(ref *attachment.ArchiveReference, err error) error {
	return executionapi.NativeAttemptArchiveReadError(ref, err)
}

func (s *Service) readAttemptCheckpointDetail(ctx context.Context, snapshot hubrecords.TenantAttemptSnapshot, detail *hubrecords.RunDetailReference) ([]byte, error) {
	return s.runnerAttemptDeps().ReadAttemptCheckpointDetail(ctx, snapshot, detail)
}

func (s *Service) populateCheckpointSource(ctx context.Context, organization tracker.OrganizationID, after string) (string, error) {
	return s.runnerAttemptDeps().PopulateCheckpointSource(ctx, organization, after)
}

type attemptSummaryContextKey = executionapi.AttemptSummaryContextKey

func attemptSummaryRead(ctx context.Context) bool {
	return executionapi.AttemptSummaryRead(ctx)
}

func (p nativeAttemptPage) candidateBytes(encoded []byte, cursor string) int {
	return (executionapi.NativeAttemptPage{Page: p.Page, Encoded: p.encoded}).CandidateBytes(encoded, cursor)
}

func (p *nativeAttemptPage) appendAttempt(attempt tracker.NativeAttempt, encoded []byte) {
	page := executionapi.NativeAttemptPage{Page: p.Page, Encoded: p.encoded}
	page.AppendAttempt(attempt, encoded)
	p.Page, p.encoded = page.Page, page.Encoded
}

func (p *nativeAttemptPage) finish() {
	page := executionapi.NativeAttemptPage{Page: p.Page, Encoded: p.encoded}
	page.Finish()
	p.Page, p.encoded = page.Page, page.Encoded
}

func readAttemptMetadataSnapshot(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, item string, attempts []leasestore.Attempt, evidence bool) (tenantstore.Reader, error) {
	return executionapi.ReadAttemptMetadataSnapshot(ctx, r, scope, item, attempts, evidence)
}

func (s *Service) readNativeCleanupAttempt(ctx context.Context, scope hubscope.NativeScope, item, id string) (tracker.NativeAttempt, error) {
	return s.runnerAttemptDeps().ReadNativeCleanupAttempt(ctx, scope, item, id)
}

type retainedAttemptReadPin = executionapi.RetainedAttemptReadPin

func (s *Service) readRetainedAttemptTenant(ctx context.Context, scope hubscope.NativeScope, item, id string, pin *retainedAttemptReadPin) (tracker.NativeAttempt, *retainedAttemptReadPin, error) {
	return s.runnerAttemptDeps().ReadRetainedAttemptTenant(ctx, scope, item, id, pin)
}

func (s *Service) runnerRuntimeDeps() executionapi.RuntimeDeps {
	return executionapi.RuntimeDeps{
		Repository:           s.config.TenantRepository,
		Now:                  s.config.Clock,
		HTTPError:            s.nativeAPIError,
		AuthorizeAccess:      s.authorizeAccessScope,
		DetailContext:        s.runDetailContext,
		RequestDetails:       s.withRequestDetails,
		DetailsFor:           s.runDetailStoreFor,
		HydrateAttempt:       s.hydrateNativeAttemptTenant,
		HydrateChange:        s.hydrateCurrentChangeDetail,
		HydrateWorkItem:      s.hydrateWorkItemRecord,
		HydrateRelationships: s.hydrateWorkItemRelationshipFacts,
		ReadAdmission:        s.readRuntimeAdmissionTenant,
		ReadSourceRecovery:   s.readRuntimeSourceRecoveryTenant,
		ReadWorkItem:         s.readWorkItemRecord,
		AttachChange:         attachRuntimeChangeDetail,
		Explain:              boundedNativeExplanation,
		ArchiveJSON:          nativeArchiveJSON,
		ReadOperatorEvidence: s.readRunnerOperatorEvidence,
	}
}

func validateNativeRuntime(r *tracker.NativeRuntimeObservation) error {
	return executionapi.ValidateNativeRuntime(nativeRejection, validateNativeGitHubScope, r)
}

func (s *Service) getNativeExplanation(c echo.Context) error {
	return s.runnerRuntimeDeps().GetNativeExplanation(c)
}

func (s *Service) getNativeRuntime(c echo.Context) error {
	return s.runnerRuntimeDeps().GetNativeRuntime(c)
}

func (s *Service) readNativeRuntime(ctx context.Context, scope hubscope.NativeScope, item, attemptID string, admission ...tracker.NativeAdmissionContext) (tracker.NativeRuntimeEvidence, error) {
	return s.runnerRuntimeDeps().ReadNativeRuntime(ctx, scope, item, attemptID, admission...)
}

func (s *Service) readNativeRuntimeTenant(ctx context.Context, scope hubscope.NativeScope, item, attemptID string, admission ...tracker.NativeAdmissionContext) (tracker.NativeRuntimeEvidence, error) {
	return s.runnerRuntimeDeps().ReadNativeRuntimeTenant(ctx, scope, item, attemptID, admission...)
}

type runtimeAttemptSelector struct {
	firstEvidencePage bool
	evidenceAfter     string
	attemptID         string
	latest            bool
	failures          *runtimeFailureRows
}

type runtimeFailureRows struct {
	rows   []leasestore.AttemptFailureRow
	legacy map[string]hubrecords.TenantAttemptSnapshot
}

const runtimeFailureHistory = executionapi.RuntimeFailureHistory

func (s *Service) readSelectedNativeRuntimeTenant(ctx context.Context, scope hubscope.NativeScope, item string, selector runtimeAttemptSelector) (tracker.NativeRuntimeEvidence, int64, bool, error) {
	value := executionapi.RuntimeAttemptSelector{AttemptID: selector.attemptID, Latest: selector.latest, FirstEvidencePage: selector.firstEvidencePage, EvidenceAfter: selector.evidenceAfter}
	if selector.failures != nil {
		value.Failures = &executionapi.RuntimeFailureRows{Rows: selector.failures.rows, Legacy: selector.failures.legacy}
	}
	evidence, sequence, found, err := s.runnerRuntimeDeps().ReadSelectedNativeRuntimeTenant(ctx, scope, item, value)
	if selector.failures != nil {
		selector.failures.rows, selector.failures.legacy = value.Failures.Rows, value.Failures.Legacy
	}
	return evidence, sequence, found, err
}

func validateNativeAdmissionContext(c tracker.NativeAdmissionContext) error {
	return executionapi.ValidateNativeAdmissionContext(c)
}

func (s *Service) hydrateSchedulerDecisionRows(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, evidence tracker.NativeRuntimeEvidence, rows eventstore.SchedulerDecisionRows) (tracker.NativeRuntimeEvidence, error) {
	return s.runnerRuntimeDeps().HydrateSchedulerDecisionRows(ctx, scope, item, evidence, rows)
}

func (s *Service) readRunnerOperatorEvidence(ctx context.Context, scope hubscope.NativeScope, issue tracker.NativeIssue, withFailures bool) (tracker.NativeRuntimeEvidence, error) {
	evidence, _, _, err := s.readOperatorEvidence(ctx, scope, issue, withFailures)
	return evidence, err
}

func (s *Service) runnerHeartbeatDeps() runnerfleet.HeartbeatDeps {
	return runnerfleet.HeartbeatDeps{
		Repository:                  s.config.TenantRepository,
		Now:                         s.config.Clock,
		HTTPError:                   s.nativeAPIError,
		NotifyDispatch:              s.runnerNotifyDispatch(),
		CheckAdmission:              s.checkRunnerAdmission,
		ApplyUsage:                  s.applyRunnerTenantUsage,
		RequestTimeout:              s.requestTimeout,
		ReadPage:                    s.readNativePage,
		ReadGitHubTask:              s.readGitHubBatchTaskRecord,
		ReleasePublished:            s.runnerReleasePublished,
		StoreUpdateObservation:      s.storeRunnerUpdateObservationTenant,
		ValidateConfigurationRunner: validateConfigurationRunner,
		ValidateCheckoutReport:      validateRunnerCheckoutReport,
		ValidSpriteSlug:             validSpritesSlug,
		StoreProjectObservation:     storeRunnerProjectObservationTenant,
		PrepareAdmissionDecisions:   s.prepareRunnerAdmissionDecisions,
		ReadAttemptRun:              s.readNativeAttemptRunTenant,
		AttemptRunMatches:           nativeAttemptRunMatches,
		Logger:                      s.config.Logger,
		Version:                     s.config.Version,
		StartSpriteLifecycle:        s.startSpriteLifecycle,
		Plans:                       func() *hostedplans.HostedPlansConfig { return s.database.plansService().Plans() },
		ReadCapabilitiesDigest:      s.runnerHeartbeatCapabilitiesDigest,
		PrepareConfiguration:        s.prepareRunnerHeartbeatConfiguration,
		ErrConfigurationUnavailable: errConfigurationPayloadUnavailable,
		ReadCleanupCursor:           s.readFleetCleanupCursor,
		AdvanceCleanupCursor:        s.advanceFleetCleanupCursor,
	}
}

func (s *Service) heartbeatNativeMachineTenant(c echo.Context, request nativeMachineHeartbeatRequest) error {
	return s.runnerHeartbeatDeps().HeartbeatNativeMachineTenant(c, request)
}

func validateTenantHeartbeatProjection(request nativeMachineHeartbeatRequest, projectID tracker.ProjectID) error {
	return runnerfleet.ValidateTenantHeartbeatProjection(request, projectID)
}

func (s *Service) readRunnerResponseSnapshot(ctx context.Context, organization tracker.OrganizationID, credential, runner string, project tracker.ProjectID, read func(context.Context, tenantstore.Reader, time.Time) error) error {
	return s.runnerHeartbeatDeps().ReadRunnerResponseSnapshot(ctx, organization, credential, runner, project, read)
}

func readRunnerResponseRouting(ctx context.Context, r tenantstore.Reader, identity runnerstore.Identity, snapshot *runnerauth.RoutingSnapshot) error {
	return runnerfleet.ReadRunnerResponseRouting(ctx, r, identity, snapshot)
}

func readHeartbeatChangesTenant(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, cursor nativeCursor, key []byte, reset bool, digest, policyID string, now time.Time) (*runnerauth.HeartbeatChanges, error) {
	return runnerfleet.ReadHeartbeatChangesTenant(ctx, reader, scope, cursor, key, reset, digest, policyID, now)
}

func (s *Service) readRegistrationClaimStateTenant(ctx context.Context, scope hubscope.NativeScope, snapshot *runnerauth.RoutingSnapshot) error {
	return s.runnerHeartbeatDeps().ReadRegistrationClaimStateTenant(ctx, scope, snapshot)
}

func registrationCleanupRequired(candidate hubrecords.TenantAttemptSnapshot, run tracker.NativeRunData, runner string) (bool, error) {
	deps := runnerfleet.HeartbeatDeps{AttemptRunMatches: nativeAttemptRunMatches}
	return deps.RegistrationCleanupRequired(candidate, run, runner)
}

func nativeRunCleanupRequired(run tracker.NativeRunData) bool {
	return runnerfleet.NativeRunCleanupRequired(run)
}

func (s *Service) readRunnerCleanupCandidates(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, candidates []runnerauth.ReleasedExecution) ([]hubrecords.TenantAttemptSnapshot, error) {
	return s.runnerHeartbeatDeps().ReadRunnerCleanupCandidates(ctx, r, scope, candidates)
}

func (s *Service) resolveRunnerCleanupCandidates(ctx context.Context, scope hubscope.NativeScope, runner string, candidates []hubrecords.TenantAttemptSnapshot, state *runnerauth.ClaimState) error {
	return s.runnerHeartbeatDeps().ResolveRunnerCleanupCandidates(ctx, scope, runner, candidates, state)
}

func (s *Service) runnerHeartbeatCapabilitiesDigest(c echo.Context) (string, bool, error) {
	capabilities, err := s.readNativeCapabilities(c.Request().Context())
	if err != nil {
		return "", false, s.nativeAPIError(c, err)
	}
	digest, err := capabilitiesDigest(capabilities)
	return digest, true, err
}

func (s *Service) prepareRunnerHeartbeatConfiguration(ctx context.Context, credential hubapi.APICredential, project tracker.ProjectID, view *runnerauth.ProjectConfiguration) (runnerfleet.ApplyHeartbeatConfiguration, error) {
	prepared, err := s.prepareConfigurationHeartbeat(ctx, credential, project, view)
	if prepared == nil || err != nil {
		return nil, err
	}
	return func(ctx context.Context, w tenantstore.Writer, identity runnerstore.Identity, snapshot *runnerauth.RoutingSnapshot) error {
		return s.applyConfigurationHeartbeat(ctx, w, identity, project, prepared, snapshot)
	}, nil
}

func (s *Service) readFleetCleanupCursor(key runnerfleet.CleanupCursorKey) (string, error) {
	return s.runnerCleanupCursor(releasedExecutionCursorKey{organization: key.Organization, project: key.Project, runner: key.Runner})
}

func (s *Service) advanceFleetCleanupCursor(key runnerfleet.CleanupCursorKey, next string) {
	s.advanceRunnerCleanupCursor(releasedExecutionCursorKey{organization: key.Organization, project: key.Project, runner: key.Runner}, next)
}

type machineSpriteFacts = runnerfleet.MachineSpriteFacts

func spriteFactValue(value *string) string {
	return runnerfleet.SpriteFactValue(value)
}

func runtimeRunnersFromSnapshot(snapshot runnerstore.RuntimeCapacitySnapshot, _ hubscope.NativeScope, now time.Time) []runnerauth.Runner {
	return runnerfleet.RuntimeRunnersFromSnapshot(landingCapacityKey, snapshot, now)
}

func runtimeLandingCapacity(snapshot runnerstore.RuntimeProviderSnapshot, project tracker.ProjectID) (bool, int) {
	return runnerfleet.RuntimeLandingCapacity(nativeLandingCapacityUnknown, snapshot, project)
}

func (s *Service) runnerClaimHTTPDeps() claimapi.HTTPDeps {
	return claimapi.HTTPDeps{
		Repository:       s.config.TenantRepository,
		Now:              s.config.Clock,
		HTTPError:        s.nativeAPIError,
		InvalidRequest:   s.invalidAPIRequest,
		AuthorizeAccess:  s.authorizeAccessScope,
		NotifyDispatch:   s.runnerNotifyDispatch(),
		TTL:              apiTTL,
		Claim:            s.claimNativeIssueTenant,
		RespondLease:     s.respondNativeLeaseTenant,
		RenewLease:       s.renewNativeLeaseTenant,
		ReleaseLease:     s.releaseNativeLeaseTenant,
		ApplyUsage:       s.applyRunnerTenantUsage,
		ReadClaimState:   s.readRegistrationClaimStateTenant,
		ValidateCheckout: validateRunnerCheckoutReport,
	}
}

func (s *Service) claimNativeIssue(c echo.Context) error {
	return s.runnerClaimHTTPDeps().ClaimNativeIssue(c)
}

func (s *Service) renewNativeLease(c echo.Context) error {
	return s.runnerClaimHTTPDeps().RenewNativeLease(c)
}

func (s *Service) releaseNativeLease(c echo.Context) error {
	return s.runnerClaimHTTPDeps().ReleaseNativeLease(c)
}

func (s *Service) registerNativeMachine(c echo.Context) error {
	return s.runnerClaimHTTPDeps().RegisterNativeMachine(c)
}

func (s *Service) runnerWaitDeps() claimapi.WaitDeps {
	return claimapi.WaitDeps{
		NewTimer:        newDispatchWaitTimer,
		Repository:      s.config.TenantRepository,
		Now:             s.config.Clock,
		HTTPError:       s.nativeAPIError,
		AuthorizeAccess: s.authorizeAccessScope,
		Cursor:          s.notifications.dispatchCursor,
		NotificationKey: dispatchNotificationKey,
		Subscribe:       func(key string) (*eventbroker.Subscription, func()) { return s.notifications.Subscribe(key) },
	}
}

func (s *Service) waitNativeCandidates(c echo.Context) error {
	return s.runnerWaitDeps().WaitNativeCandidates(c)
}

func (s *Service) runnerAdmissionDeps() claimapi.AdmissionDeps {
	return claimapi.AdmissionDeps{
		RuntimeRunners:         runtimeRunnersFromSnapshot,
		Repository:             s.config.TenantRepository,
		Now:                    s.config.Clock,
		Details:                s.runDetailStore,
		AuthorizeAccess:        s.authorizeAccessScope,
		DetailsFor:             s.runDetailStoreFor,
		ReadCheckpoint:         s.readNativeAttemptCheckpointTenant,
		AnsweredExclusion:      s.nativeClaimAnsweredExclusion,
		IssueResponse:          s.nativeIssueResponse,
		ReadReleaseDelivery:    readReleaseDeliveryContractFacts,
		LandingClaimable:       nativeChangeLandingClaimable,
		CutoverSource:          cutoverSourceReference,
		ReleaseDeliveryCurrent: releaseDeliveryContractFactsCurrent,
		Hosted:                 s.config.Hosted != nil,
		LandingCapacityUnknown: nativeLandingCapacityUnknown,
		ValidateRunnerVersion:  func(version string) error { return runnerVersionError(minimumRunnerVersion(s.config.Version), version) },
	}
}

type runtimeAdmissionPageKey = claimapi.RuntimeAdmissionPageKey

type runtimeAdmissionPage = claimapi.RuntimeAdmissionPage

func (s *Service) readRuntimeAdmissionTenant(ctx context.Context, scope hubscope.NativeScope, issue tracker.NativeIssue, evidence *tracker.NativeRuntimeEvidence, supplied []tracker.NativeAdmissionContext) error {
	return s.runnerAdmissionDeps().ReadRuntimeAdmissionTenant(ctx, scope, issue, evidence, supplied)
}

func nativeRuntimeIssueContractEvaluation(plan hubrecords.NativeClaimPlan) (nativeContractEvaluation, error) {
	evaluation, err := claimapi.NativeRuntimeIssueContractEvaluation(releaseDeliveryContractFactsCurrent, plan)
	return nativeContractEvaluation{Allowed: evaluation.Allowed, ReasonCode: evaluation.ReasonCode, Reason: evaluation.Reason, Missing: evaluation.Missing}, err
}

func (s *Service) readRuntimeUnresolvedDependencies(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, issue tracker.NativeIssue, project projectstore.Record) ([]tracker.NativeDependency, bool, error) {
	return s.runnerAdmissionDeps().ReadRuntimeUnresolvedDependencies(ctx, r, scope, issue, project)
}

func decodeRunHistory(ctx context.Context, q hubapi.NativeQueryer, raw string, result *tracker.CollaborationData) error {
	return executionapi.DecodeRunHistory(ctx, q, raw, result)
}

func attemptFailureSummary(data tracker.NativeRunData, status string) *leasestore.AttemptFailure {
	return executionapi.AttemptFailureSummary(data, status)
}

func applyHealthAttemptData(a *healthRateAttempt, data tracker.NativeRunData) {
	executionapi.ApplyHealthAttemptData(a, data)
}

type nativeRunReplaySnapshot struct {
	rows     []eventstore.Event
	attempts []leasestore.Attempt
}

func readNativeRunReplaySnapshot(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, item string, events []tracker.NativeRunEvent) (*nativeRunReplaySnapshot, error) {
	result, err := executionapi.ReadNativeRunReplaySnapshot(nativeAttemptHistoryID, ctx, r, scope, item, events)
	if err != nil {
		return nil, err
	}
	return &nativeRunReplaySnapshot{rows: result.Rows, attempts: result.Attempts}, nil
}
