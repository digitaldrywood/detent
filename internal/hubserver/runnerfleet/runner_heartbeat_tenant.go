package runnerfleet

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runneradvice"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerupdate"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workpage"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type ApplyHeartbeatConfiguration func(context.Context, tenantstore.Writer, runnerstore.Identity, *runnerauth.RoutingSnapshot) error

type CleanupCursorKey struct {
	Organization tracker.OrganizationID
	Project      tracker.ProjectID
	Runner       string
}

const ReleasedExecutionsPageSize = 100

type HeartbeatDeps struct {
	Repository                  tenantstore.Repository
	Now                         func() time.Time
	HTTPError                   func(echo.Context, error, ...any) error
	NotifyDispatch              func(tracker.OrganizationID)
	CheckAdmission              func(ctx context.Context, writer billingstore.Writer, now time.Time, counted int64, metrics ...string) error
	ApplyUsage                  func(ctx context.Context, writer billingstore.Writer, now time.Time, receiptID string, completion, heartbeat bool) error
	RequestTimeout              func(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc)
	ReadPage                    func(ctx context.Context, scope hubscope.NativeScope, path string, params url.Values) (int, workpage.Cursor, []byte, error)
	ReadGitHubTask              func(ctx context.Context, scope hubscope.NativeScope) (*tracker.GitHubBatchTask, error)
	ReleasePublished            func(ctx context.Context, version, platform, architecture string) bool
	StoreUpdateObservation      func(ctx context.Context, writer tenantstore.Writer, identity runnerstore.Identity, report *runnerauth.UpdateObservation, protocol int, version, platform, architecture string, now time.Time, releasePublished bool) error
	ValidateConfigurationRunner func(view *runnerauth.ProjectConfiguration, identity runnerstore.Identity) error
	ValidateCheckoutReport      func(scope hubscope.NativeScope, report *string) error
	ValidSpriteSlug             func(slug string) bool
	StoreProjectObservation     func(ctx context.Context, scope hubscope.NativeScope, view runnerauth.ProjectConfiguration, receivedAt time.Time, writer runnerstore.Writer) error
	PrepareAdmissionDecisions   func(ctx context.Context, credential hubapi.APICredential, view *runnerauth.ProjectConfiguration) (PreparedRunnerAdmissionDecisions, error)
	ReadAttemptRun              func(ctx context.Context, snapshot hubrecords.TenantAttemptSnapshot) (tracker.NativeRunData, error)
	AttemptRunMatches           func(snapshot hubrecords.TenantAttemptSnapshot, run tracker.NativeRunData) bool
	Logger                      *slog.Logger
	Version                     string
	StartSpriteLifecycle        func(hubscope.NativeScope, string)
	Plans                       func() *hostedplans.HostedPlansConfig
	ReadCapabilitiesDigest      func(echo.Context) (string, bool, error)
	PrepareConfiguration        func(context.Context, hubapi.APICredential, tracker.ProjectID, *runnerauth.ProjectConfiguration) (ApplyHeartbeatConfiguration, error)
	ErrConfigurationUnavailable error
	ReadCleanupCursor           func(CleanupCursorKey) (string, error)
	AdvanceCleanupCursor        func(CleanupCursorKey, string)
}

func (d HeartbeatDeps) HeartbeatNativeMachineTenant(c echo.Context, request NativeMachineHeartbeatRequest) error {
	credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
	if !ok || credential.Runner.RunnerID == "" || credential.ID == "" || credential.Runner.OrganizationID != tracker.OrganizationID(c.Param("organization")) || credential.Runner.MachineID != tracker.MachineID(c.Param("machine")) {
		return d.HTTPError(c, hubapi.NativeNotFound())
	}
	if len(request.DisplayName) == 0 || len(request.DisplayName) > 200 || request.Capacity < 0 || request.Capacity > 10000 || strings.TrimSpace(request.Version) == "" || len(request.Version) > 100 || !ValidRunnerPlatform(request.OS, request.Architecture) {
		return d.HTTPError(c, hubapi.NativePublicInvalid("Display name, version and nonnegative capacity are required"))
	}
	projectID := tracker.ProjectID(c.Param("project"))
	if err := ValidateTenantHeartbeatProjection(request, projectID); err != nil {
		return d.HTTPError(c, err)
	}
	if err := ValidateProviderReports(request.ProviderReports); err != nil {
		return d.HTTPError(c, err)
	}
	if err := request.BackendIsolation.Validate(); err != nil {
		return d.HTTPError(c, hubapi.NativeInvalid(err.Error()))
	}
	if err := validateUpdateObservation(request.Update, request.ProtocolMajor, request.Version, request.OS, request.Architecture); err != nil {
		return d.HTTPError(c, err)
	}
	if request.SpriteName != "" && !d.ValidSpriteSlug(request.SpriteName) {
		return d.HTTPError(c, hubapi.NativePublicInvalid("Sprite name is invalid"))
	}
	if !workspacesession.ValidIsolation(request.WorkspaceIsolation) {
		return d.HTTPError(c, hubapi.NativePublicInvalid("Workspace isolation must be sandbox, container or user"))
	}
	if request.CapacityConfig != nil {
		if err := request.CapacityConfig.Validate(); err != nil {
			return d.HTTPError(c, hubapi.NativeInvalid(err.Error()))
		}
	}
	if request.Admission != nil && (request.Admission.RunnerRevision < 1 || !request.Admission.ReceivedAt.IsZero() || request.Admission.Context.Validate() != nil) {
		return d.HTTPError(c, hubapi.NativePublicInvalid("Admission observation is invalid"))
	}
	if err := runnerauth.ValidateReportedProblems(request.Problems); err != nil {
		return d.HTTPError(c, hubapi.NativeInvalid(err.Error()))
	}
	if request.LocalChecks != nil {
		if projectID == "" {
			return d.HTTPError(c, hubapi.NativeNotFound())
		}
		if err := request.LocalChecks.Validate(); err != nil {
			return d.HTTPError(c, hubapi.NativeInvalid(err.Error()))
		}
	}
	if request.Skills != nil {
		if projectID == "" {
			return d.HTTPError(c, hubapi.NativeNotFound())
		}
		if len(*request.Skills) > runnerstore.MaxProjectSkills {
			return d.HTTPError(c, hubapi.NativePublicInvalid("Runner skill report exceeds its item limit"))
		}
		for _, skill := range *request.Skills {
			if !skills.ValidProviderSkill(skill) {
				return d.HTTPError(c, hubapi.NativePublicInvalid("Invalid runner skill metadata"))
			}
		}
	}
	if err := d.ValidateCheckoutReport(hubscope.NativeScope{Credential: credential}, request.CheckoutRepository); err != nil {
		return d.HTTPError(c, err)
	}
	updateTarget := runnerupdate.Target(d.Version)
	updateReleasePublished := updateTarget != "" && request.OS != "" && request.Architecture != "" && d.ReleasePublished(c.Request().Context(), updateTarget, request.OS, request.Architecture)
	var changeCursor workpage.Cursor
	var changeKey []byte
	var changeReset bool
	var changeDigest string
	if request.ChangeCursor != nil {
		params := url.Values{"cursor": {*request.ChangeCursor}}
		var err error
		_, changeCursor, changeKey, err = d.ReadPage(c.Request().Context(), hubscope.NativeRequestScope(c), c.Request().URL.EscapedPath()+"/changes", params)
		if err != nil {
			params.Set("cursor", "")
			_, changeCursor, changeKey, err = d.ReadPage(c.Request().Context(), hubscope.NativeRequestScope(c), c.Request().URL.EscapedPath()+"/changes", params)
		}
		if err != nil {
			return d.HTTPError(c, err)
		}
		changeReset = *request.ChangeCursor == "" || changeCursor.After == ""
		var ready bool
		changeDigest, ready, err = d.ReadCapabilitiesDigest(c)
		if !ready || err != nil {
			return err
		}
	}
	hostSummaries := runneradvice.PrepareHostMetrics(request.HostMetrics)

	ctx, cancel := d.RequestTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	if request.SpriteName != "" {
		if err := d.Repository.ReadTenant(ctx, credential.Runner.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			machine, err := r.Machine(ctx, string(credential.Runner.MachineID))
			if err != nil {
				return err
			}
			if machine.Hostname != request.SpriteName {
				return hubapi.NativePublicInvalid("Sprite name must match the runner hostname")
			}
			return nil
		}); err != nil {
			return d.HTTPError(c, err)
		}
	}
	configuration, err := d.PrepareConfiguration(ctx, credential, projectID, request.ProjectConfiguration)
	if err != nil {
		if !errors.Is(err, d.ErrConfigurationUnavailable) {
			return d.HTTPError(c, err)
		}
		d.Logger.Warn("project configuration payload unavailable", "runner_id", credential.Runner.RunnerID, "error", err)
		configuration = nil
	}
	admissionDecisions, err := d.PrepareAdmissionDecisions(ctx, credential, request.ProjectConfiguration)
	if err != nil {
		return d.HTTPError(c, err)
	}
	var snapshot runnerauth.RoutingSnapshot
	var acknowledgments []hostmetrics.Acknowledgment
	dispatchChanged := false
	write := func(ctx context.Context, w tenantstore.Writer) error {
		dispatchChanged = false
		identity, err := w.RunnerIdentityByCredential(ctx, credential.ID)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				return hubscope.RunnerUnauthorized()
			}
			return err
		}
		current, err := w.AccessCredential(ctx, credential.ID)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				return hubscope.RunnerUnauthorized()
			}
			return err
		}
		receivedAt := d.Now().UTC()
		if identity.OrganizationID != credential.Runner.OrganizationID || identity.RunnerID != credential.Runner.RunnerID || identity.MachineID != credential.Runner.MachineID || identity.RemovedAt != nil || identity.TokenID != credential.ID || !identity.ExpiresAt.After(receivedAt) || current.OrganizationID != identity.OrganizationID || current.Scope != string(hubapi.APIScopeWorker) || current.Kind != "service" || !current.Valid(credential.Hash, receivedAt) || !slices.Contains(identity.Operations, runnerauth.Heartbeat) {
			return hubscope.RunnerUnauthorized()
		}
		if err := AppendRunnerAdmissionDecisions(ctx, w, identity, admissionDecisions, receivedAt); err != nil {
			return err
		}
		if request.Skills != nil || request.CheckoutRepository != nil {
			if projectID == "" {
				return hubapi.NativeNotFound()
			}
			allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, projectID, receivedAt)
			if err != nil {
				return err
			}
			if !allowed {
				return hubapi.NativeNotFound()
			}
		}
		previous := identity
		if identity.CapacityConfig != nil {
			config := *identity.CapacityConfig
			previous.CapacityConfig = &config
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
				return hubapi.NativePublicInvalid("Sprite name must match the runner hostname")
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
				return hubapi.NativeNotFound()
			}
			allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, projectID, receivedAt)
			if err != nil {
				return err
			}
			if !allowed {
				return hubapi.NativeNotFound()
			}
			observation := *request.Admission
			observation.ReceivedAt = receivedAt
			if err := w.ReplaceRunnerAdmissionObservation(ctx, identity.RunnerID, projectID, observation); err != nil {
				return err
			}
		}
		if request.ProviderReports != nil {
			equal, err := w.RunnerProviderReportsMatch(ctx, identity.RunnerID, request.ProviderReports)
			if err != nil {
				return err
			}
			if !equal {
				previousReports, err := w.RunnerProviderReports(ctx, identity.RunnerID)
				if err != nil {
					return err
				}
				if err := w.ReplaceRunnerProviderReports(ctx, identity.RunnerID, MergeRunnerProviderReports(previousReports, request.ProviderReports, receivedAt)); err != nil {
					return err
				}
			}
		}
		if request.CapacityConfig != nil {
			config := *request.CapacityConfig
			config.ObservedAt = receivedAt
			if err := w.UpdateRunnerCapacityConfig(ctx, identity.RunnerID, config); err != nil {
				return err
			}
		}
		if request.Update != nil || request.ProtocolMajor < 2 {
			if err := d.StoreUpdateObservation(ctx, w, identity, request.Update, request.ProtocolMajor, request.Version, request.OS, request.Architecture, receivedAt, updateReleasePublished); err != nil {
				return err
			}
		}
		if request.ProjectConfiguration != nil {
			view := *request.ProjectConfiguration
			if err := d.ValidateConfigurationRunner(&view, identity); err != nil {
				return err
			}
			allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, tracker.ProjectID(view.ProjectID), receivedAt)
			if err != nil {
				return err
			}
			if !allowed {
				return hubapi.NativeNotFound()
			}
			if err := d.StoreProjectObservation(ctx, hubscope.NativeScope{Organization: identity.OrganizationID, Project: tracker.ProjectID(view.ProjectID), Credential: credential}, view, receivedAt, w); err != nil {
				return err
			}
		}
		if request.LocalChecks != nil {
			allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, projectID, receivedAt)
			if err != nil {
				return err
			}
			if !allowed {
				return hubapi.NativeNotFound()
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
		problems, err := w.RunnerProblems(ctx, identity.RunnerID)
		if err != nil {
			return err
		}
		mergedProblems := MergeProblems(problems, request.Problems, request.ProtocolMajor, request.SettingsRejected, receivedAt)
		if !slices.Equal(problems, mergedProblems) {
			if err := w.ReplaceRunnerProblems(ctx, identity.RunnerID, mergedProblems); err != nil {
				return err
			}
		}
		identity, err = w.RunnerIdentity(ctx, identity.RunnerID)
		if err != nil {
			return err
		}
		heartbeatFresh := func(record runnerstore.Identity) bool {
			return !receivedAt.Before(record.LastHeartbeatAt) && receivedAt.Before(record.LastHeartbeatAt.Add(runnerauth.HeartbeatTimeout))
		}
		capacityFresh := func(record runnerstore.Identity) bool {
			return record.CapacityConfig != nil && !receivedAt.Before(record.CapacityConfig.ObservedAt) && receivedAt.Before(record.CapacityConfig.ObservedAt.Add(runnerauth.HeartbeatTimeout))
		}
		dispatchChanged = previous.ReportedCapacity != identity.ReportedCapacity || heartbeatFresh(previous) != heartbeatFresh(identity) || capacityFresh(previous) != capacityFresh(identity) || (previous.CapacityConfig == nil) != (identity.CapacityConfig == nil)
		if previous.CapacityConfig != nil && identity.CapacityConfig != nil {
			dispatchChanged = dispatchChanged || previous.CapacityConfig.LocalLimit != identity.CapacityConfig.LocalLimit || previous.CapacityConfig.ClientLimit != identity.CapacityConfig.ClientLimit || previous.CapacityConfig.RuntimeLimit != identity.CapacityConfig.RuntimeLimit
		}

		snapshot = runnerauth.RoutingSnapshot{RunnerID: identity.RunnerID, HostMetricsAcknowledged: acknowledgments}
		if configuration != nil {
			if err := configuration(ctx, w, identity, &snapshot); err != nil {
				return err
			}
		}
		if updateReleasePublished {
			snapshot.TargetRunnerVersion = strings.TrimPrefix(updateTarget, "v")
		}
		hasActiveLease, err := w.RunnerHasActiveLease(ctx, identity.RunnerID, receivedAt)
		if err != nil {
			return err
		}
		if plans := d.Plans(); plans != nil && !hasActiveLease && !previous.LastHeartbeatAt.After(receivedAt.Add(-time.Duration(plans.ConnectedSeconds)*time.Second)) {
			if err := d.CheckAdmission(ctx, w, receivedAt, 1, "connected_runners"); err != nil {
				return err
			}
		}
		return d.ApplyUsage(ctx, w, receivedAt, hubapi.NewID("runner_heartbeat"), hasActiveLease, true)
	}
	if command, ok := d.Repository.(interface {
		WriteRunnerHeartbeat(context.Context, tracker.OrganizationID, string, string, func(context.Context, tenantstore.Writer) error) error
	}); ok {
		err = command.WriteRunnerHeartbeat(ctx, credential.Runner.OrganizationID, credential.ID, credential.Runner.RunnerID, write)
		if errors.Is(err, accessstore.ErrDenied) {
			err = hubscope.RunnerUnauthorized()
		}
	} else {
		err = d.Repository.WriteTenant(ctx, credential.Runner.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error { return write(ctx, w) })
	}
	if err != nil {
		return d.HTTPError(c, err)
	}
	if dispatchChanged {
		d.NotifyDispatch(credential.Runner.OrganizationID)
	}
	if d.StartSpriteLifecycle != nil {
		d.StartSpriteLifecycle(hubscope.NativeScope{Organization: credential.Runner.OrganizationID, Project: projectID}, "")
	}
	assigned := false
	cleanupKey := CleanupCursorKey{Organization: credential.Runner.OrganizationID, Project: projectID, Runner: credential.Runner.RunnerID}
	cleanupAfter, err := d.ReadCleanupCursor(cleanupKey)
	if err != nil {
		return d.HTTPError(c, err)
	}
	var cleanupNext string
	var cleanupCandidates []hubrecords.TenantAttemptSnapshot
	err = d.ReadRunnerResponseSnapshot(ctx, credential.Runner.OrganizationID, credential.ID, credential.Runner.RunnerID, projectID, func(ctx context.Context, r tenantstore.Reader, now time.Time) error {
		identity, err := r.RunnerIdentityByCredential(ctx, credential.ID)
		if err != nil {
			return err
		}
		current, err := r.AccessCredential(ctx, credential.ID)
		if err != nil {
			return err
		}
		if current.OrganizationID != credential.Runner.OrganizationID || current.Scope != string(hubapi.APIScopeWorker) || current.Kind != "service" || !current.Valid(credential.Hash, now) || !RunnerIdentityMatchesHeldLease(identity, credential.Runner.OrganizationID, credential.Runner.RunnerID, credential.ID, credential.Runner.MachineID, now) || !slices.Contains(identity.Operations, runnerauth.Heartbeat) {
			return hubscope.RunnerUnauthorized()
		}
		if err := ReadRunnerResponseRouting(ctx, r, identity, &snapshot); err != nil {
			return err
		}
		assigned, err = r.RunnerProjectAuthorized(ctx, identity.RunnerID, projectID, now)
		if err != nil {
			return err
		}
		if !assigned {
			snapshot.ClaimState = nil
			snapshot.Changes = nil
			snapshot.GitHubIntake = nil
			snapshot.ProjectConfiguration = nil
			snapshot.ProjectConfigurationRequest = nil
			snapshot.TargetRunnerVersion = ""
			return nil
		}
		reader, ok := r.(runnerstore.ClaimStateReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		claims, err := reader.RunnerClaimState(ctx, identity.RunnerID, projectID, cleanupAfter, ReleasedExecutionsPageSize, now)
		if err != nil {
			return err
		}
		cleanupCandidates, err = d.ReadRunnerCleanupCandidates(ctx, r, hubscope.NativeScope{Organization: identity.OrganizationID, Project: projectID, Credential: credential}, claims.Unclassified)
		if err != nil {
			return err
		}
		cleanupNext = claims.Next
		claims.State.ReleasedExecutions = claims.Candidates
		snapshot.ClaimState = &claims.State
		if request.ChangeCursor != nil {
			policyID := ""
			policy, err := r.ProjectPolicy(ctx, projectID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if err == nil {
				policyID = policy.PolicyID
			}
			snapshot.Changes, err = ReadHeartbeatChangesTenant(ctx, r, hubscope.NativeScope{Organization: identity.OrganizationID, Project: projectID, Credential: credential}, changeCursor, changeKey, changeReset, changeDigest, policyID, now)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return d.HTTPError(c, err)
	}
	if !assigned {
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusOK, snapshot)
	}
	if err := d.ResolveRunnerCleanupCandidates(ctx, hubscope.NativeScope{Organization: credential.Runner.OrganizationID, Project: projectID, Credential: credential}, credential.Runner.RunnerID, cleanupCandidates, snapshot.ClaimState); err != nil {
		return d.HTTPError(c, err)
	}
	d.AdvanceCleanupCursor(cleanupKey, cleanupNext)
	task, err := d.ReadGitHubTask(ctx, hubscope.NativeScope{Organization: credential.Runner.OrganizationID, Project: projectID, Credential: credential})
	if err != nil {
		d.Logger.Warn("GitHub intake task unavailable", "organization_id", credential.Runner.OrganizationID, "project_id", projectID, "runner_id", credential.Runner.RunnerID, "error", err)
	} else {
		snapshot.GitHubIntake = task
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, snapshot)
}

func ValidateTenantHeartbeatProjection(request NativeMachineHeartbeatRequest, projectID tracker.ProjectID) error {
	if request.ProjectConfiguration == nil {
		return nil
	}
	view := request.ProjectConfiguration
	invalid := view.Validate() != nil || (view.Authority != "local_global_configuration" && view.Authority != "hub_configuration") || projectID != "" && view.ProjectID != string(projectID)
	if diagnostics := view.Diagnostics; invalid || diagnostics != nil && diagnostics.Source != "runner_runtime_and_durable_attempt_owners" {
		return hubapi.NativePublicInvalid("Invalid project configuration observation")
	}
	return nil
}

func (d HeartbeatDeps) ReadRunnerResponseSnapshot(ctx context.Context, organization tracker.OrganizationID, credential, runner string, project tracker.ProjectID, read func(context.Context, tenantstore.Reader, time.Time) error) error {
	if owner, ok := d.Repository.(interface {
		ReadRunnerHeartbeatSnapshot(context.Context, tracker.OrganizationID, string, string, tracker.ProjectID, func() time.Time, func(context.Context, tenantstore.Reader, time.Time) error) error
	}); ok {
		return owner.ReadRunnerHeartbeatSnapshot(ctx, organization, credential, runner, project, d.Now, read)
	}
	repository, ok := d.Repository.(tenantstore.SnapshotRepository)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	return repository.ReadTenantSnapshot(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error { return read(ctx, r, d.Now().UTC()) })
}

func ReadRunnerResponseRouting(ctx context.Context, r tenantstore.Reader, identity runnerstore.Identity, snapshot *runnerauth.RoutingSnapshot) error {
	projects, err := r.RunnerProjects(ctx, identity.RunnerID, runnerstore.MaxRunnerProjectList)
	if err != nil {
		return err
	}
	if identity.Scope == "organization" {
		projects = []tracker.ProjectID{}
	}
	record, err := r.Machine(ctx, string(identity.MachineID))
	if err != nil {
		return err
	}
	routing := runnerauth.Routing{
		Scope: identity.Scope, CapacityRequest: identity.CapacityRequest, ProjectRankOverrides: identity.ProjectRanks, DisplayName: identity.DisplayName, MachineUse: identity.MachineUse, Tags: slices.Clone(identity.Tags),
		State: identity.State, CapacityLimit: int(identity.CapacityLimit), ProjectCapacityLimits: identity.ProjectCapacityLimits,
		ProjectRanks: identity.ProjectRanks, ProjectIDs: projects, IsolationTier: identity.IsolationTier, HostServices: slices.Clone(identity.HostServices),
		Availability: runnerauth.Availability{Timezone: identity.Availability.Timezone, Windows: slices.Clone(identity.Availability.Windows), WindowSlots: identity.Availability.WindowSlots, HardDeadline: identity.Availability.HardDeadline},
	}
	updateState, err := r.RunnerUpdateState(ctx, identity.RunnerID)
	if err != nil {
		return err
	}
	updateRunner := runnerauth.Runner{Binding: runnerauth.Binding{RunnerID: identity.RunnerID}, Routing: runnerauth.Routing{UpdateRequest: updateState.Desired}, Update: updateState.Observation}
	urgentUpdate, err := r.RunnerUrgentUpdate(ctx)
	if err != nil {
		return err
	}
	if err := runnerupdate.ApplyAvailableRouting(ctx, r, &updateRunner, urgentUpdate, record.Version); err != nil {
		return err
	}
	routing.UpdateRequest = updateRunner.UpdateRequest
	routing = routing.Normalized()
	if err := routing.Validate(); err != nil {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	snapshot.RunnerID, snapshot.Revision, snapshot.Routing = identity.RunnerID, identity.Revision, routing
	snapshot.RepositoryListRequest = ""
	listing, err := r.RunnerRepositoryList(ctx, identity.RunnerID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if err == nil && listing.Pending {
		snapshot.RepositoryListRequest = listing.RequestID
	}
	return nil
}

func validateUpdateObservation(report *runnerauth.UpdateObservation, protocol int, version, platform, architecture string) error {
	return hubapi.FromFeatureError(runnerupdate.ValidateObservation(report, protocol, version, platform, architecture))
}
