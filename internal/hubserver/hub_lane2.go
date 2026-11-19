package hubserver

import (
	"context"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/executionapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerfleet"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/tracker"
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
