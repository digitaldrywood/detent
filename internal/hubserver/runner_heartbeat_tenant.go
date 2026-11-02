package hubserver

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) heartbeatNativeMachineTenant(c echo.Context, request nativeMachineHeartbeatRequest) error {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.Runner.RunnerID == "" || credential.ID == "" || credential.Runner.OrganizationID != tracker.OrganizationID(c.Param("organization")) || credential.Runner.MachineID != tracker.MachineID(c.Param("machine")) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if len(request.DisplayName) == 0 || len(request.DisplayName) > 200 || request.Capacity < 0 || request.Capacity > 10000 || strings.TrimSpace(request.Version) == "" || len(request.Version) > 100 || !validRunnerPlatform(request.OS, request.Architecture) || request.ProtocolMajor != 0 && request.ProtocolMajor != 2 {
		return s.nativeAPIError(c, nativePublicInvalid("Display name, version and nonnegative capacity are required"))
	}
	if err := validateTenantHeartbeatProjection(request); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := validateProviderReports(request.ProviderReports); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := request.BackendIsolation.Validate(); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	if request.SpriteName != "" && !validSpritesSlug(request.SpriteName) {
		return s.nativeAPIError(c, nativePublicInvalid("Sprite name is invalid"))
	}
	if request.CapacityConfig != nil {
		if err := request.CapacityConfig.Validate(); err != nil {
			return s.nativeAPIError(c, nativeInvalid(err.Error()))
		}
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	var snapshot runnerauth.RoutingSnapshot
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
		routing = routing.Normalized()
		if err := routing.Validate(); err != nil {
			return nativeRunEventStorageUnavailable()
		}
		snapshot = runnerauth.RoutingSnapshot{RunnerID: identity.RunnerID, Revision: identity.Revision, Routing: routing}
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
	if len(request.HostMetrics) != 0 && string(request.HostMetrics) != "null" && string(request.HostMetrics) != "[]" {
		return nativeRunEventStorageUnavailable()
	}
	if request.ChangeCursor != nil || request.Admission != nil || request.Update != nil || request.LocalChecks != nil || len(request.Problems) != 0 || request.ProtocolMajor > 0 && request.ProtocolMajor != 2 || request.SettingsRejected || request.WorkspaceCapabilities != nil || request.WorkspaceIsolation != "" || request.Skills != nil || request.CheckoutRepository != nil {
		return nativeRunEventStorageUnavailable()
	}
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
