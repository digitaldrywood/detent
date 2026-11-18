package hubserver

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
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

func (s *Service) claimNativeIssue(c echo.Context) error {
	ctx := c.Request().Context()
	var request tracker.NativeClaim
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	ttl, err := validateNativeClaimRequest(request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	if request.WorkItemID == "" && len(request.ProviderCandidates) == 1 {
		request.WorkItemID = request.ProviderCandidates[0].WorkItemID
	}
	lease, err := s.claimNativeIssueTenant(ctx, scope, request, ttl)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.respondNativeLeaseTenant(c, scope, lease.LeaseID)
}

func validateNativeClaimRequest(request tracker.NativeClaim) (time.Duration, error) {
	if err := validateProviderCandidates(request.ProviderCandidates); err != nil {
		return 0, err
	}
	if request.LandingCapacity < 0 {
		return 0, hubapi.NativePublicInvalid("Landing capacity must be non-negative")
	}
	if request.ProtocolMajor != tracker.NativeProtocolMajor || !slices.Contains(request.Capabilities, "scoped_collaboration") || !slices.Contains(request.Capabilities, "native_issues") {
		return 0, hubapi.NativePublicInvalid("Native protocol and required collaboration capabilities must be negotiated")
	}
	for _, capability := range request.Capabilities {
		if !slices.Contains([]string{"native_issues", "scoped_collaboration", "revision_conflicts", "idempotent_mutations", tracker.NativeExecutionCapability, tracker.NativeProviderCapacityCapability, tracker.NativeWorkspaceCapability, tracker.NativeLandingCapacityCapability}, capability) {
			return 0, hubapi.NativePublicInvalid("Unknown required capability")
		}
	}
	if len(request.SessionID) > 128 {
		return 0, hubapi.NativePublicInvalid("Session ID is too long")
	}
	ttl, err := apiTTL(request.TTLSeconds)
	if err != nil {
		return 0, hubapi.NativePublicInvalid("Lease TTL is invalid")
	}
	return ttl, nil
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
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if strings.TrimSpace(string(request.ID)) == "" || len(request.ID) > 128 || strings.TrimSpace(request.Hostname) == "" || len(request.Hostname) > 200 || len(request.DisplayName) > 200 || strings.TrimSpace(request.Version) == "" || len(request.Version) > 100 || request.Capacity < 0 || !validRunnerPlatform(request.OS, request.Architecture) {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Machine identity, hostname, version, and nonnegative capacity are required"))
	}
	scope := hubscope.NativeRequestScope(c)
	if scope.Credential.Runner.RunnerID != "" && request.ID != scope.Credential.Runner.MachineID {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	if !workspacesession.ValidIsolation(request.WorkspaceIsolation) {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Workspace isolation must be sandbox, container or user"))
	}
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, hubapi.NativeRunEventStorageUnavailable())
	}
	if scope.Credential.Runner.RunnerID == "" {
		if request.WorkspaceCapabilities != nil {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("Workspace capabilities require an enrolled runner"))
		}
		if len(request.ProviderReports) != 0 {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("Provider reports require an enrolled runner"))
		}
		if request.Skills != nil {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("Skill reports require an enrolled runner"))
		}
		if request.CheckoutRepository != nil && *request.CheckoutRepository != "" {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("Checkout reports require an enrolled runner"))
		}
	}
	if err := validateProviderReports(request.ProviderReports); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := request.BackendIsolation.Validate(); err != nil {
		return s.nativeAPIError(c, hubapi.NativeInvalid(err.Error()))
	}
	if request.Skills != nil {
		if len(*request.Skills) > runnerstore.MaxProjectSkills {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("Runner skill report exceeds its item limit"))
		}
		for _, skill := range *request.Skills {
			if !skills.ValidProviderSkill(skill) {
				return s.nativeAPIError(c, hubapi.NativePublicInvalid("Invalid runner skill metadata"))
			}
		}
	}
	if scope.Credential.Runner.RunnerID != "" {
		if err := validateRunnerCheckoutReport(scope, request.CheckoutRepository); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	var snapshot runnerauth.RoutingSnapshot
	err := s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		credential := scope.Credential
		if credential.Runner.RunnerID == "" {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			owner, ok := w.(leasestore.MachineRegistrationWriter)
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			return owner.RegisterOwnedMachine(ctx, leasestore.Machine{OrganizationID: scope.Organization, ID: string(request.ID), Hostname: request.Hostname, DisplayName: request.DisplayName, Capacity: int64(request.Capacity), Version: request.Version}, credential.ID)
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
		now := s.config.Clock().UTC()
		if !runnerIdentityMatchesHeldLease(identity, scope.Organization, credential.Runner.RunnerID, credential.ID, request.ID, now) || current.Scope != string(hubapi.APIScopeWorker) || current.Kind != "service" || !current.Valid(credential.Hash, now) || !slices.Contains(identity.Operations, runnerauth.Heartbeat) {
			return hubscope.RunnerUnauthorized()
		}
		allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
		if err != nil {
			return err
		}
		if !allowed {
			return hubapi.NativeNotFound()
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
			if err := w.ReplaceRunnerProjectSkills(ctx, identity.RunnerID, scope.Project, *request.Skills, now); err != nil {
				return err
			}
		}
		if request.CheckoutRepository != nil {
			if err := w.ReplaceRunnerCheckoutReport(ctx, runnerstore.CheckoutReport{OrganizationID: scope.Organization, RunnerID: identity.RunnerID, ProjectID: scope.Project, Repository: *request.CheckoutRepository, ReportedAt: now}); err != nil {
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
		return s.applyRunnerTenantUsage(ctx, w, now, hubapi.NewID("runner_registration"), active, false)
	})
	if err != nil {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	if scope.Credential.Runner.RunnerID == "" {
		return c.JSON(http.StatusOK, request)
	}
	if err := s.readRegistrationClaimStateTenant(ctx, scope, &snapshot); err != nil {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	if s.notifications != nil {
		s.notifications.notify(dispatchNotificationKey(scope.Organization))
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, snapshot)
}
