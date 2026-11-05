package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func authorizeClaimScope(ctx context.Context, tx *sql.Tx, request tracker.ClaimRequest, scope *nativeScope) error {
	if scope != nil && scope.credential.Runner.RunnerID != "" && scope.credential.Runner.MachineID != request.MachineID {
		return tracker.ErrMachineNotFound
	}
	var count int
	query := "SELECT count(*) FROM machines WHERE id = ? AND organization_id IS NULL"
	args := []any{request.MachineID}
	if scope != nil {
		query = "SELECT count(*) FROM machines WHERE id = ? AND organization_id = ? AND token_id = ?"
		args = append(args, scope.organization, scope.credential.ID)
		if scope.credential.Runner.RunnerID != "" {
			query = "SELECT count(*) FROM runner_identities WHERE machine_id = ? AND organization_id = ? AND token_id = ?"
		}
	}
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return tracker.ErrMachineNotFound
	}
	if request.WorkItemID != 0 {
		return authorizeClaimItem(ctx, tx, request.WorkItemID, scope)
	}
	return nil
}

func authorizeClaimItem(ctx context.Context, tx *sql.Tx, id tracker.WorkItemID, scope *nativeScope) error {
	var count int
	query := "SELECT count(*) FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = ? AND p.profile = 'github_compatible'"
	args := []any{id}
	if scope != nil {
		query = "SELECT count(*) FROM issues WHERE id = ? AND organization_id = ? AND project_id = ?"
		args = append(args, scope.organization, scope.project)
	}
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return tracker.ErrWorkItemNotFound
	}
	return nil
}

func (s *Service) claimNativeIssue(c echo.Context) error {
	ctx := c.Request().Context()
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	var request tracker.NativeClaim
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := validateProviderCandidates(request.ProviderCandidates); err != nil {
		return s.nativeAPIError(c, err)
	}
	if request.LandingCapacity < 0 {
		return s.nativeAPIError(c, nativePublicInvalid("Landing capacity must be non-negative"))
	}
	if request.ProtocolMajor != tracker.NativeProtocolMajor || !slices.Contains(request.Capabilities, "scoped_collaboration") || !slices.Contains(request.Capabilities, "native_issues") {
		return s.nativeAPIError(c, nativePublicInvalid("Native protocol and required collaboration capabilities must be negotiated"))
	}
	for _, capability := range request.Capabilities {
		if !slices.Contains([]string{"native_issues", "scoped_collaboration", "revision_conflicts", "idempotent_mutations", tracker.NativeExecutionCapability, tracker.NativeProviderCapacityCapability, tracker.NativeWorkspaceCapability, tracker.NativeLandingCapacityCapability}, capability) {
			return s.nativeAPIError(c, nativePublicInvalid("Unknown required capability"))
		}
	}
	if len(request.SessionID) > 128 {
		return s.nativeAPIError(c, nativePublicInvalid("Session ID is too long"))
	}
	ttl, err := apiTTL(request.TTLSeconds)
	if err != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Lease TTL is invalid"))
	}
	scope := nativeRequestScope(c)
	if request.WorkItemID == "" && len(request.ProviderCandidates) == 1 {
		request.WorkItemID = request.ProviderCandidates[0].WorkItemID
	}
	if s.config.TenantRepository != nil {
		lease, err := s.claimNativeIssueTenant(ctx, scope, request, ttl)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return s.respondNativeLeaseTenant(c, scope, lease.LeaseID)
	}
	var id tracker.WorkItemID
	if request.WorkItemID != "" {
		_, id, err = readNativeIssue(readCtx, s.database.reader, scope, string(request.WorkItemID))
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	lease, err := s.database.claimNext(ctx, tracker.ClaimRequest{WorkItemID: id, MachineID: request.MachineID, SessionID: request.SessionID, TTL: ttl}, claimCandidateQuery{
		LandingCapacity:      request.LandingCapacity,
		LandingCapacityKnown: request.LandingCapacity > 0 || slices.Contains(request.Capabilities, tracker.NativeLandingCapacityCapability),
		MinimumRunnerVersion: minimumRunnerVersion(s.config.Version),
		PolicyID:             request.PolicyID, RequirePolicy: true, ProviderCandidates: request.ProviderCandidates, RequireProviderCapacity: slices.Contains(request.Capabilities, tracker.NativeProviderCapacityCapability),
		DispatchPriorityByState: request.DispatchPriorityByState, DispatchPriorityByLabel: request.DispatchPriorityByLabel, PrioritizeUnblockers: request.PrioritizeUnblockers,
		NativeScope: &scope, Scope: string(scope.project), WorkflowStates: request.WorkflowStates, Authors: request.Authors, Assignees: request.Assignees, LabelInclude: request.LabelInclude, LabelExclude: request.LabelExclude,
		// Only a lane that declares the workspace capability is offered
		// workspace items.
		WorkspaceLane: slices.Contains(request.Capabilities, tracker.NativeWorkspaceCapability),
	}, s.config.ReconcileInterval)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.respondNativeLease(c, scope, lease)
}

func (s *Service) respondNativeLease(c echo.Context, scope nativeScope, lease tracker.Lease) error {
	if s.config.TenantRepository != nil {
		return s.respondNativeLeaseTenant(c, scope, string(lease.ID))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	policyID, err := s.database.leasePolicyID(ctx, lease.ID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var id tracker.NativeWorkItemID
	if err := s.database.reader.QueryRowContext(ctx, "SELECT native_id FROM issues WHERE id = ? AND organization_id = ? AND project_id = ?", lease.WorkItemID, scope.organization, scope.project).Scan(&id); err != nil {
		return s.nativeAPIError(c, err)
	}
	reservation, reserved, err := readProviderReservation(ctx, s.database.reader, lease.ID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var providerReservation *providercapacity.Reservation
	if reserved {
		providerReservation = &reservation
	}
	var policy *isolation.Policy
	if scope.credential.Runner.RunnerID != "" {
		var encoded string
		if err := s.database.reader.QueryRowContext(ctx, "SELECT isolation_policy_json FROM lease_runners WHERE lease_id = ? AND runner_id = ?", lease.ID, scope.credential.Runner.RunnerID).Scan(&encoded); err != nil {
			return s.nativeAPIError(c, err)
		}
		policy = &isolation.Policy{}
		if err := json.Unmarshal([]byte(encoded), policy); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return c.JSON(http.StatusOK, tracker.NativeLease{RunDetailBatching: s.runDetailStore() != nil, IsolationPolicy: policy, ProviderReservation: providerReservation, ServerTime: s.config.now().UTC(), PolicyID: policyID, ID: lease.ID, WorkItemID: id, MachineID: lease.Machine.ID, SessionID: lease.SessionID, FencingToken: lease.FencingToken, AcquiredAt: lease.AcquiredAt, RenewedAt: lease.RenewedAt, ExpiresAt: lease.ExpiresAt})
}

func (s *Service) requireNativeLease(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	return requireLeaseRunner(ctx, s.database.reader, tracker.LeaseID(c.Param("lease")), scope)
}

func (s *Service) renewNativeLease(c echo.Context) error {
	if s.config.TenantRepository != nil {
		return s.renewNativeLeaseTenant(c)
	}
	if err := s.requireNativeLease(c); err != nil {
		return s.nativeAPIError(c, err)
	}
	var request tracker.NativeLeaseMutation
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if request.LandingCapacity < 0 {
		return s.nativeAPIError(c, nativePublicInvalid("Landing capacity must be non-negative"))
	}
	ttl, err := apiTTL(request.TTLSeconds)
	if err != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Lease TTL is invalid"))
	}
	lease, err := s.database.renew(c.Request().Context(), tracker.RenewRequest{LeaseID: tracker.LeaseID(c.Param("lease")), FencingToken: request.FencingToken, TTL: ttl}, true, request.LandingCapacity, nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.respondNativeLease(c, nativeRequestScope(c), lease)
}

func (s *Service) releaseNativeLease(c echo.Context) error {
	if s.config.TenantRepository != nil {
		return s.releaseNativeLeaseTenant(c)
	}
	if err := s.requireNativeLease(c); err != nil {
		return s.nativeAPIError(c, err)
	}
	var request tracker.NativeLeaseMutation
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if request.CancelAttempt && request.Reason != "cancelled" || !slices.Contains([]string{"completed", "cancelled", "failed", "work_item_hydration_failed", "work_item_identity_missing", "released"}, request.Reason) {
		return s.nativeAPIError(c, nativePublicInvalid("Release reason is invalid"))
	}
	ctx := c.Request().Context()
	if request.Reason == "cancelled" {
		var err error
		ctx, err = s.prepareNativeLeaseCancellation(ctx, nativeRequestScope(c), tracker.LeaseID(c.Param("lease")), request.FencingToken)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	if err := s.database.releaseLease(ctx, tracker.ReleaseRequest{LeaseID: tracker.LeaseID(c.Param("lease")), FencingToken: request.FencingToken, Reason: request.Reason}); err != nil {
		return s.nativeAPIError(c, err)
	}
	s.notifyNativeDispatch(c.Request().Context(), nativeRequestScope(c), request)
	s.startSpritePoolForQueue(nativeRequestScope(c))
	if s.conversations != nil {
		s.conversations.leaseReleased(c.Request().Context(), tracker.LeaseID(c.Param("lease")))
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) registerNativeMachine(c echo.Context) error {
	var request struct {
		BackendIsolation isolation.Report          `json:"backend_isolation,omitempty"`
		ID               tracker.MachineID         `json:"id"`
		Hostname         string                    `json:"hostname"`
		DisplayName      string                    `json:"display_name"`
		Capacity         int                       `json:"capacity"`
		Version          string                    `json:"version"`
		OS               string                    `json:"os,omitempty"`
		Architecture     string                    `json:"architecture,omitempty"`
		ProviderReports  []providercapacity.Report `json:"provider_reports,omitempty"`
		// WorkspaceCapabilities and WorkspaceIsolation are what this runner
		// can serve for a workspace session, reported beside the provider
		// reports. A runner that never reports them
		// serves no surface and is never handed a workspace item.
		WorkspaceCapabilities *workspacesession.Capabilities `json:"workspace_capabilities,omitempty"`
		WorkspaceIsolation    string                         `json:"workspace_isolation,omitempty"`
		Skills                *[]skills.ProviderSkill        `json:"skills,omitempty"`
		CheckoutRepository    *string                        `json:"checkout_repository,omitempty"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if strings.TrimSpace(string(request.ID)) == "" || len(request.ID) > 128 || strings.TrimSpace(request.Hostname) == "" || len(request.Hostname) > 200 || len(request.DisplayName) > 200 || strings.TrimSpace(request.Version) == "" || len(request.Version) > 100 || request.Capacity < 0 || !validRunnerPlatform(request.OS, request.Architecture) {
		return s.nativeAPIError(c, nativePublicInvalid("Machine identity, hostname, version, and nonnegative capacity are required"))
	}
	scope := nativeRequestScope(c)
	if scope.credential.Runner.RunnerID != "" && request.ID != scope.credential.Runner.MachineID {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if !workspacesession.ValidIsolation(request.WorkspaceIsolation) {
		return s.nativeAPIError(c, nativePublicInvalid("Workspace isolation must be sandbox, container or user"))
	}
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, nativeRunEventStorageUnavailable())
	}
	if scope.credential.Runner.RunnerID == "" {
		if request.WorkspaceCapabilities != nil {
			return s.nativeAPIError(c, nativePublicInvalid("Workspace capabilities require an enrolled runner"))
		}
		if len(request.ProviderReports) != 0 {
			return s.nativeAPIError(c, nativePublicInvalid("Provider reports require an enrolled runner"))
		}
		if request.Skills != nil {
			return s.nativeAPIError(c, nativePublicInvalid("Skill reports require an enrolled runner"))
		}
		if request.CheckoutRepository != nil && *request.CheckoutRepository != "" {
			return s.nativeAPIError(c, nativePublicInvalid("Checkout reports require an enrolled runner"))
		}
	}
	if err := validateProviderReports(request.ProviderReports); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := request.BackendIsolation.Validate(); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	if request.Skills != nil {
		if len(*request.Skills) > runnerstore.MaxProjectSkills {
			return s.nativeAPIError(c, nativePublicInvalid("Runner skill report exceeds its item limit"))
		}
		for _, skill := range *request.Skills {
			if !skills.ValidProviderSkill(skill) {
				return s.nativeAPIError(c, nativePublicInvalid("Invalid runner skill metadata"))
			}
		}
	}
	if scope.credential.Runner.RunnerID != "" {
		if err := validateRunnerCheckoutReport(scope, request.CheckoutRepository); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	var snapshot runnerauth.RoutingSnapshot
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		credential := scope.credential
		if credential.Runner.RunnerID == "" {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			owner, ok := w.(leasestore.MachineRegistrationWriter)
			if !ok {
				return nativeRunEventStorageUnavailable()
			}
			return owner.RegisterOwnedMachine(ctx, leasestore.Machine{OrganizationID: scope.organization, ID: string(request.ID), Hostname: request.Hostname, DisplayName: request.DisplayName, Capacity: int64(request.Capacity), Version: request.Version}, credential.ID)
		}
		identity, err := w.RunnerIdentityByCredential(ctx, credential.ID)
		if err != nil {
			return err
		}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		current, err := w.AccessCredential(ctx, credential.ID)
		if err != nil {
			return err
		}
		now := s.config.now().UTC()
		if !runnerIdentityMatchesHeldLease(identity, scope.organization, credential.Runner.RunnerID, credential.ID, request.ID, now) || current.Scope != string(apiScopeWorker) || current.Kind != "service" || !current.Valid(credential.Hash, now) || !slices.Contains(identity.Operations, runnerauth.Heartbeat) {
			return runnerUnauthorized()
		}
		allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, now)
		if err != nil {
			return err
		}
		if !allowed {
			return nativeNotFound()
		}
		identity.ReportedCapacity, identity.OS, identity.Architecture = int64(request.Capacity), request.OS, request.Architecture
		if now.After(identity.LastHeartbeatAt) {
			identity.LastHeartbeatAt = now
		}
		if err := w.UpdateRunnerIdentity(ctx, identity); err != nil {
			return err
		}
		machine, err := w.LockMachine(ctx, string(request.ID))
		if err != nil {
			return err
		}
		machine.Version = request.Version
		if err := w.HeartbeatMachine(ctx, machine, now); err != nil {
			return err
		}
		if err := w.ReplaceRunnerBackendIsolation(ctx, identity.RunnerID, request.BackendIsolation); err != nil {
			return err
		}
		if request.WorkspaceCapabilities != nil {
			if err := w.ReplaceRunnerWorkspaceReport(ctx, identity.RunnerID, *request.WorkspaceCapabilities, request.WorkspaceIsolation); err != nil {
				return err
			}
		}
		if request.Skills != nil {
			if err := w.ReplaceRunnerProjectSkills(ctx, identity.RunnerID, scope.project, *request.Skills, now); err != nil {
				return err
			}
		}
		if request.CheckoutRepository != nil {
			if err := w.ReplaceRunnerCheckoutReport(ctx, runnerstore.CheckoutReport{OrganizationID: scope.organization, RunnerID: identity.RunnerID, ProjectID: scope.project, Repository: *request.CheckoutRepository, ReportedAt: now}); err != nil {
				return err
			}
		}
		previous, err := w.RunnerProviderReports(ctx, identity.RunnerID)
		if err != nil {
			return err
		}
		if err := w.ReplaceRunnerProviderReports(ctx, identity.RunnerID, mergeRunnerProviderReports(previous, request.ProviderReports, now)); err != nil {
			return err
		}
		snapshot.RunnerID = identity.RunnerID
		active, err := w.RunnerHasActiveLease(ctx, identity.RunnerID, now)
		if err != nil {
			return err
		}
		return s.applyRunnerTenantUsage(ctx, w, now, newNativeID("runner_registration"), active, false)
	})
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	if scope.credential.Runner.RunnerID == "" {
		return c.JSON(http.StatusOK, request)
	}
	if err := s.readRegistrationClaimStateTenant(ctx, scope, &snapshot); err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	if s.notifications != nil {
		s.notifications.notify(dispatchNotificationKey(scope.organization))
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, snapshot)
}
