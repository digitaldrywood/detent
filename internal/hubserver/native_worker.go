package hubserver

import (
	"context"
	"database/sql"
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
	lease, err := s.claimNativeIssueTenant(ctx, scope, request, ttl)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.respondNativeLeaseTenant(c, scope, lease.LeaseID)
}

func (s *Service) respondNativeLease(c echo.Context, scope nativeScope, lease tracker.Lease) error {
	return s.respondNativeLeaseTenant(c, scope, string(lease.ID))
}

func (s *Service) renewNativeLease(c echo.Context) error {
	return s.renewNativeLeaseTenant(c)
}

func (s *Service) releaseNativeLease(c echo.Context) error {
	return s.releaseNativeLeaseTenant(c)
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
