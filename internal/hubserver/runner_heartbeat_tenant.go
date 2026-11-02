package hubserver

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func (s *Service) heartbeatNativeMachineTenant(c echo.Context, request nativeMachineHeartbeatRequest) error {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.Runner.RunnerID == "" || credential.ID == "" || credential.Runner.OrganizationID != tracker.OrganizationID(c.Param("organization")) || credential.Runner.MachineID != tracker.MachineID(c.Param("machine")) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if len(request.DisplayName) == 0 || len(request.DisplayName) > 200 || request.Capacity < 0 || request.Capacity > 10000 || strings.TrimSpace(request.Version) == "" || len(request.Version) > 100 || !validRunnerPlatform(request.OS, request.Architecture) {
		return s.nativeAPIError(c, nativePublicInvalid("Display name, version and nonnegative capacity are required"))
	}
	projectID := tracker.ProjectID(c.Param("project"))
	if err := validateTenantHeartbeatProjection(request); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := validateProviderReports(request.ProviderReports); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := request.BackendIsolation.Validate(); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	if err := validateRunnerUpdateObservation(request.Update, request.ProtocolMajor, request.Version, request.OS, request.Architecture); err != nil {
		return s.nativeAPIError(c, err)
	}
	if request.SpriteName != "" && !validSpritesSlug(request.SpriteName) {
		return s.nativeAPIError(c, nativePublicInvalid("Sprite name is invalid"))
	}
	if !workspacesession.ValidIsolation(request.WorkspaceIsolation) {
		return s.nativeAPIError(c, nativePublicInvalid("Workspace isolation must be sandbox, container or user"))
	}
	if request.CapacityConfig != nil {
		if err := request.CapacityConfig.Validate(); err != nil {
			return s.nativeAPIError(c, nativeInvalid(err.Error()))
		}
	}
	if request.Admission != nil && (request.Admission.RunnerRevision < 1 || !request.Admission.ReceivedAt.IsZero() || request.Admission.Context.Validate() != nil) {
		return s.nativeAPIError(c, nativePublicInvalid("Admission observation is invalid"))
	}
	if err := runnerauth.ValidateReportedProblems(request.Problems); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	if request.LocalChecks != nil {
		if projectID == "" {
			return s.nativeAPIError(c, nativeNotFound())
		}
		if err := request.LocalChecks.Validate(); err != nil {
			return s.nativeAPIError(c, nativeInvalid(err.Error()))
		}
	}
	if request.Skills != nil {
		if projectID == "" {
			return s.nativeAPIError(c, nativeNotFound())
		}
		if len(*request.Skills) > runnerstore.MaxProjectSkills {
			return s.nativeAPIError(c, nativePublicInvalid("Runner skill report exceeds its item limit"))
		}
		for _, skill := range *request.Skills {
			if !skills.ValidProviderSkill(skill) {
				return s.nativeAPIError(c, nativePublicInvalid("Invalid runner skill metadata"))
			}
		}
	}
	if err := validateRunnerCheckoutReport(nativeScope{credential: credential}, request.CheckoutRepository); err != nil {
		return s.nativeAPIError(c, err)
	}
	updateTarget := runnerUpdateTarget(s.config.Version)
	updateReleasePublished := request.Update != nil && request.Update.Supported && updateTarget != "" && s.runnerReleasePublished(c.Request().Context(), updateTarget, request.OS, request.Architecture)
	var changeCursor nativeCursor
	var changeKey []byte
	var changeReset bool
	var changeDigest string
	if request.ChangeCursor != nil {
		params := url.Values{"cursor": {*request.ChangeCursor}}
		var err error
		_, changeCursor, changeKey, err = s.readNativePage(c.Request().Context(), nativeRequestScope(c), c.Request().URL.EscapedPath()+"/changes", params)
		if err != nil {
			params.Set("cursor", "")
			_, changeCursor, changeKey, err = s.readNativePage(c.Request().Context(), nativeRequestScope(c), c.Request().URL.EscapedPath()+"/changes", params)
		}
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		changeReset = *request.ChangeCursor == "" || changeCursor.After == ""
		capabilities, err := s.readNativeCapabilities(c.Request().Context())
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		changeDigest, err = capabilitiesDigest(capabilities)
		if err != nil {
			return err
		}
	}
	hostSummaries := prepareRunnerHostMetrics(request.HostMetrics)

	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	var snapshot runnerauth.RoutingSnapshot
	var heartbeatChanges *runnerauth.HeartbeatChanges
	var acknowledgments []hostmetrics.Acknowledgment
	err := s.config.TenantRepository.WriteTenant(ctx, credential.Runner.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		identity, err := w.RunnerIdentityByCredential(ctx, credential.ID)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				return runnerUnauthorized()
			}
			return err
		}
		current, err := w.AccessCredential(ctx, credential.ID)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				return runnerUnauthorized()
			}
			return err
		}
		receivedAt := s.config.now().UTC()
		if identity.OrganizationID != credential.Runner.OrganizationID || identity.RunnerID != credential.Runner.RunnerID || identity.MachineID != credential.Runner.MachineID || identity.RemovedAt != nil || identity.TokenID != credential.ID || !identity.ExpiresAt.After(receivedAt) || current.OrganizationID != identity.OrganizationID || current.Scope != string(apiScopeWorker) || current.Kind != "service" || !current.Valid(credential.Hash, receivedAt) || !slices.Contains(identity.Operations, runnerauth.Heartbeat) {
			return runnerUnauthorized()
		}
		if request.Skills != nil || request.CheckoutRepository != nil {
			if projectID == "" {
				return nativeNotFound()
			}
			allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, projectID, receivedAt)
			if err != nil {
				return err
			}
			if !allowed {
				return nativeNotFound()
			}
		}
		heartbeatAt := receivedAt
		if identity.LastHeartbeatAt.After(heartbeatAt) {
			heartbeatAt = identity.LastHeartbeatAt
		}
		identity.ReportedCapacity = int64(request.Capacity)
		identity.OS = request.OS
		identity.Architecture = request.Architecture
		identity.LastHeartbeatAt = heartbeatAt
		if err := w.UpdateRunnerIdentity(ctx, identity); err != nil {
			return err
		}
		machineUpdate := leasestore.Machine{OrganizationID: identity.OrganizationID, ID: string(identity.MachineID), DisplayName: request.DisplayName, Capacity: int64(request.Capacity), Version: request.Version, SpriteName: request.SpriteName}
		if request.SpriteName != "" {
			machine, err := w.Machine(ctx, machineUpdate.ID)
			if err != nil {
				return err
			}
			if machine.Hostname != request.SpriteName {
				return nativePublicInvalid("Sprite name must match the runner hostname")
			}
		}
		if err := w.HeartbeatMachine(ctx, machineUpdate, receivedAt); err != nil {
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
		acknowledgments, err = w.AppendRunnerHostMetricSegments(ctx, identity.RunnerID, hostSummaries, receivedAt)
		if err != nil {
			return err
		}
		if request.Admission != nil {
			if projectID == "" {
				return nativeNotFound()
			}
			allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, projectID, receivedAt)
			if err != nil {
				return err
			}
			if !allowed {
				return nativeNotFound()
			}
			observation := *request.Admission
			observation.ReceivedAt = receivedAt
			if err := w.ReplaceRunnerAdmissionObservation(ctx, identity.RunnerID, projectID, observation); err != nil {
				return err
			}
		}
		previousReports, err := w.RunnerProviderReports(ctx, identity.RunnerID)
		if err != nil {
			return err
		}
		if err := w.ReplaceRunnerProviderReports(ctx, identity.RunnerID, mergeRunnerProviderReports(previousReports, request.ProviderReports, receivedAt)); err != nil {
			return err
		}
		if request.CapacityConfig != nil {
			config := *request.CapacityConfig
			config.ObservedAt = receivedAt
			if err := w.UpdateRunnerCapacityConfig(ctx, identity.RunnerID, config); err != nil {
				return err
			}
		}
		if err := s.storeRunnerUpdateObservationTenant(ctx, w, identity, request.Update, request.ProtocolMajor, request.Version, request.OS, request.Architecture, receivedAt, updateReleasePublished); err != nil {
			return err
		}
		if request.ProjectConfiguration != nil {
			view := *request.ProjectConfiguration
			if view.RunnerID != "" && view.RunnerID != identity.RunnerID || view.RunnerRevision != 0 && view.RunnerRevision != identity.Revision {
				return nativePublicInvalid("Project diagnostics do not match the current runner identity")
			}
			allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, tracker.ProjectID(view.ProjectID), receivedAt)
			if err != nil {
				return err
			}
			if !allowed {
				return nativeNotFound()
			}
			if err := storeRunnerProjectObservationTenant(ctx, nativeScope{organization: identity.OrganizationID, project: tracker.ProjectID(view.ProjectID), credential: credential}, view, receivedAt, w); err != nil {
				return err
			}
		}
		if request.LocalChecks != nil {
			allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, projectID, receivedAt)
			if err != nil {
				return err
			}
			if !allowed {
				return nativeNotFound()
			}
			checks := *request.LocalChecks
			checks.ObservedAt = receivedAt
			if err := w.ReplaceRunnerLocalChecks(ctx, identity.RunnerID, projectID, checks); err != nil {
				return err
			}
		}
		if request.Skills != nil {
			if err := w.ReplaceRunnerProjectSkills(ctx, identity.RunnerID, projectID, *request.Skills, receivedAt); err != nil {
				return err
			}
		}
		if request.CheckoutRepository != nil {
			if err := w.ReplaceRunnerCheckoutReport(ctx, runnerstore.CheckoutReport{OrganizationID: identity.OrganizationID, RunnerID: identity.RunnerID, ProjectID: projectID, Repository: *request.CheckoutRepository, ReportedAt: receivedAt}); err != nil {
				return err
			}
		}
		if request.ChangeCursor != nil {
			allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, projectID, receivedAt)
			if err != nil {
				return err
			}
			if !allowed {
				return nativeNotFound()
			}
			policyID := ""
			policy, err := w.ProjectPolicy(ctx, projectID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if err == nil {
				policyID = policy.PolicyID
			}
			heartbeatChanges, err = readHeartbeatChangesTenant(ctx, w, nativeScope{organization: identity.OrganizationID, project: projectID, credential: credential}, changeCursor, changeKey, changeReset, changeDigest, policyID, receivedAt)
			if err != nil {
				return err
			}
		}
		problems, err := w.RunnerProblems(ctx, identity.RunnerID)
		if err != nil {
			return err
		}
		if err := w.ReplaceRunnerProblems(ctx, identity.RunnerID, mergeRunnerProblems(problems, request.Problems, request.ProtocolMajor, request.SettingsRejected, receivedAt)); err != nil {
			return err
		}
		identity, err = w.RunnerIdentity(ctx, identity.RunnerID)
		if err != nil {
			return err
		}
		projects, err := w.RunnerProjects(ctx, identity.RunnerID, runnerstore.MaxRunnerProjectList)
		if err != nil {
			return err
		}
		repositoryList, repositoryListErr := w.RunnerRepositoryList(ctx, identity.RunnerID)
		if repositoryListErr != nil && !errors.Is(repositoryListErr, corestore.ErrNotFound) {
			return repositoryListErr
		}
		if identity.Scope == "organization" {
			projects = []tracker.ProjectID{}
		}
		routing := runnerauth.Routing{
			Scope: identity.Scope, DisplayName: identity.DisplayName, MachineUse: identity.MachineUse, Tags: slices.Clone(identity.Tags),
			State: identity.State, CapacityLimit: int(identity.CapacityLimit), ProjectCapacityLimits: identity.ProjectCapacityLimits,
			ProjectRanks: identity.ProjectRanks, ProjectIDs: projects, IsolationTier: identity.IsolationTier, HostServices: slices.Clone(identity.HostServices),
			Availability: runnerauth.Availability{Timezone: identity.Availability.Timezone, Windows: slices.Clone(identity.Availability.Windows), WindowSlots: identity.Availability.WindowSlots, HardDeadline: identity.Availability.HardDeadline},
		}
		updateState, err := w.RunnerUpdateState(ctx, identity.RunnerID)
		if err != nil {
			return err
		}
		updateRunner := runnerauth.Runner{Routing: runnerauth.Routing{UpdateRequest: updateState.Desired}, Update: updateState.Observation}
		urgentUpdate, err := w.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		applyUrgentRunnerRouting(&updateRunner, urgentRunnerUpdate{Revision: urgentUpdate.Revision, Request: urgentUpdate.Request}, request.Version)
		routing.UpdateRequest = updateRunner.UpdateRequest
		routing = routing.Normalized()
		if err := routing.Validate(); err != nil {
			return nativeRunEventStorageUnavailable()
		}
		snapshot = runnerauth.RoutingSnapshot{RunnerID: identity.RunnerID, Revision: identity.Revision, Routing: routing}
		snapshot.HostMetricsAcknowledged = acknowledgments
		snapshot.Changes = heartbeatChanges
		if updateReleasePublished {
			snapshot.TargetRunnerVersion = strings.TrimPrefix(updateTarget, "v")
		}
		if repositoryListErr == nil && repositoryList.Pending {
			snapshot.RepositoryListRequest = repositoryList.RequestID
		}
		activeLeases, err := w.RunnerActiveLeaseIDs(ctx, identity.RunnerID, receivedAt, 1)
		if err != nil {
			return err
		}
		return s.applyRunnerTenantUsage(ctx, w, receivedAt, newNativeID("runner_heartbeat"), len(activeLeases) > 0, true)
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, snapshot)
}

func validateTenantHeartbeatProjection(request nativeMachineHeartbeatRequest) error {
	if request.ProjectConfiguration == nil {
		return nil
	}
	view := request.ProjectConfiguration
	if view.Validate() != nil || view.Authority != "local_global_configuration" || view.Diagnostics == nil || view.Registered || view.RuntimeRegistered || view.Paused || view.Draining || view.UnsettledAttempts != 0 || view.ConfigRevision != "" || view.SelectedPolicy != nil || view.EffectivePolicy != nil || view.LocalBindingPolicy != nil || view.RestrictedBindingPolicy != nil || view.Pending || view.Saved || view.Applied || view.Constraint != "" || view.LastOperation != nil || view.LocalIntakeEnabled || len(view.LocalIntakeRemaining) != 0 || len(view.LocalIntakeBlocked) != 0 {
		return nativeRunEventStorageUnavailable()
	}
	diagnostics := view.Diagnostics
	if diagnostics.Source != "runner_runtime_and_durable_attempt_owners" || diagnostics.RunningBuild != nil || len(diagnostics.Admissions) != 0 || len(diagnostics.Counts) != 0 || diagnostics.Truncated || len(diagnostics.Unavailable) != 0 {
		return nativeRunEventStorageUnavailable()
	}
	for _, record := range diagnostics.Records {
		if !record.OperationStartedAt.IsZero() || !record.OperationObservedAt.IsZero() || record.LatestErrorCode != "" || record.LatestError != "" || !record.DeferredAt.IsZero() || !record.RetryAt.IsZero() || record.RetryAttempt != 0 || record.RecoveryOwner != "" || !record.LeaseExpiresAt.IsZero() || len(record.Unavailable) != 0 {
			return nativeRunEventStorageUnavailable()
		}
	}
	return nil
}
