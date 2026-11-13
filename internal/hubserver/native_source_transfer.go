package hubserver

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type sourceTransferRequest struct {
	tracker.Mutation
	ExpectedRevision    tracker.Revision `json:"expected_revision,string"`
	VersionID           string           `json:"version_id"`
	DestinationRunnerID string           `json:"destination_runner_id"`
}

type sourceRecoveryView = tracker.NativeSourceRecovery
type sourceRecoveryRunner = tracker.NativeSourceRecoveryRunner

func (s *Service) getNativeSourceRecovery(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	scope := hubscope.NativeRequestScope(c)
	view, err := s.readNativeSourceRecoveryTenant(ctx, scope, c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	view.Destinations, view.DestinationsNext, err = s.nativeSourceRecoveryDestinationsTenant(ctx, scope, c.QueryParam("destinations_after"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, view)
}

type nativeSourceRecoverySnapshot struct {
	item            corestore.WorkItem
	itemVersion     eventstore.CollaborationVersion
	version         *eventstore.Version
	attempt         hubrecords.TenantAttemptSnapshot
	attemptFound    bool
	unreleasedLease bool
	runnerName      string
}

func (s *Service) readNativeSourceRecoveryTenant(ctx context.Context, scope hubscope.NativeScope, item string, supplied ...tracker.NativeIssue) (sourceRecoveryView, error) {
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok || len(supplied) > 1 {
		return sourceRecoveryView{}, hubapi.NativeRunEventStorageUnavailable()
	}
	var snapshot nativeSourceRecoverySnapshot
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.Profile != "native" || project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		snapshot.item, err = r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		leases, ok := r.(leasestore.UnreleasedIssueLeaseReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		snapshot.unreleasedLease, err = leases.HasUnreleasedIssueLease(ctx, snapshot.item.ID)
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{Kind: hubapi.StringValue(snapshot.item.ActorKind), PrincipalID: hubapi.StringValue(snapshot.item.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		if len(supplied) == 0 {
			snapshot.itemVersion, err = r.CollaborationVersion(ctx, item, snapshot.item.Revision)
			if err != nil {
				return err
			}
		} else if supplied[0].WorkItemID != snapshot.item.NativeID || supplied[0].OrganizationID != scope.Organization || supplied[0].ProjectID != scope.Project || int64(supplied[0].Revision) != snapshot.item.Revision || supplied[0].State != hubapi.StringValue(snapshot.item.WorkflowStateName) || supplied[0].Archived != snapshot.item.Archived {
			return corestore.ErrSerializationFailure
		}
		change, err := r.LatestChange(ctx, scope.Project, snapshot.item.NativeID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil && change.CurrentVersionID != nil && *change.CurrentVersionID != "" {
			version, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
			if err != nil {
				return err
			}
			if change.OrganizationID != scope.Organization || change.ProjectID != scope.Project || change.WorkItemID != snapshot.item.NativeID || version.OrganizationID != scope.Organization || version.ChangeID != change.ID {
				return artifact.ErrIntegrity
			}
			snapshot.version = &version
		}
		reader, ok := r.(leasestore.CheckpointAttemptReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		attempt, err := reader.SourceCheckpointAttempt(ctx, snapshot.item.ID)
		if err == nil && attempt.CheckpointPreservesSource == nil {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		if err == nil && (attempt.CheckpointObjectKey == nil || !*attempt.CheckpointPreservesSource) {
			return artifact.ErrIntegrity
		}
		if errors.Is(err, corestore.ErrNotFound) {
			if snapshot.version != nil && snapshot.version.AttemptID != "" {
				attempt, err = r.Attempt(ctx, snapshot.version.AttemptID)
				if err != nil {
					return err
				}
			} else {
				attempt, err = r.LatestAttempt(ctx, snapshot.item.ID)
			}
		}
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if attempt.OrganizationID != scope.Organization || attempt.IssueID != snapshot.item.ID {
			return artifact.ErrIntegrity
		}
		snapshot.attemptFound, snapshot.attempt.Attempt = true, attempt
		snapshot.attempt.Lease, err = r.Lease(ctx, attempt.LeaseID)
		if err != nil {
			return err
		}
		lease := snapshot.attempt.Lease
		if lease.OrganizationID != scope.Organization || lease.IssueID != snapshot.item.ID || lease.FencingToken != attempt.FencingToken {
			return artifact.ErrIntegrity
		}
		snapshot.attempt.Event, err = r.LatestRunEvent(ctx, scope.Project, attempt.ID)
		if err != nil {
			return err
		}
		if snapshot.attempt.Event.OrganizationID != scope.Organization || snapshot.attempt.Event.ProjectID != scope.Project || snapshot.attempt.Event.WorkItemID != snapshot.item.NativeID || snapshot.attempt.Event.AttemptID != attempt.ID {
			return artifact.ErrIntegrity
		}
		if err := hubrecords.ReadTenantAttemptPayload(ctx, r, &snapshot.attempt); err != nil {
			return err
		}
		identity, err := r.RunnerForLease(ctx, attempt.LeaseID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil {
			snapshot.runnerName = identity.DisplayName
		}
		if snapshot.runnerName == "" {
			machine, err := r.Machine(ctx, lease.MachineID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			snapshot.runnerName = lease.MachineID
			if err == nil {
				snapshot.runnerName = machine.Hostname
			}
		}
		return nil
	})
	if err != nil {
		return sourceRecoveryView{}, hubapi.ProjectRecordError(err)
	}
	var issue tracker.NativeIssue
	if len(supplied) != 0 {
		issue = supplied[0]
	} else {
		issue, err = s.hydrateCanonicalWorkItemRecord(ctx, snapshot.item, snapshot.itemVersion)
		if err != nil {
			return sourceRecoveryView{}, err
		}
	}
	view := sourceRecoveryView{Destinations: []sourceRecoveryRunner{}, WorkItemID: snapshot.item.NativeID, Revision: tracker.Revision(snapshot.item.Revision)}
	interrupted := false
	if snapshot.attemptFound {
		run, err := s.readNativeAttemptRunTenant(ctx, snapshot.attempt)
		if err != nil {
			return view, err
		}
		if err := hubrecords.NativeAttemptRunMismatch(snapshot.attempt, run); err != nil {
			return view, err
		}
		if run.MachineID != tracker.MachineID(snapshot.attempt.Lease.MachineID) {
			return view, artifact.ErrIntegrity
		}
		checkpoint, err := s.readNativeAttemptCheckpointTenant(ctx, snapshot.attempt)
		if err != nil {
			return view, err
		}
		view.AttemptID, view.SourceMachineID, view.SourceRunnerID, view.SourceRunnerName = run.AttemptID, run.MachineID, run.RunnerID, snapshot.runnerName
		if checkpoint != nil {
			view.HeadSHA = checkpoint.HeadSHA
		}
		attempt := snapshot.attempt.Attempt
		unacknowledged := run.TerminalFailure != nil && run.TerminalFailure.Stage == "lease_release" && attempt.CleanupAcknowledgedAt == nil
		interrupted = snapshot.attempt.Lease.ReleasedAt != nil && unacknowledged
		view.Quiesced = snapshot.attempt.Lease.ReleasedAt != nil && !unacknowledged && (attempt.Status == "succeeded" || attempt.Status == "failed" || attempt.Status == "cancelled")
	}
	if snapshot.version != nil {
		row := *snapshot.version
		view.VersionID, view.BaseSHA = row.ID, row.BaseSHA
		if view.AttemptID == "" {
			view.AttemptID = row.AttemptID
		}
		view.Available, err = s.verifiedNativeSourceTenant(ctx, scope, row)
		if err != nil {
			return view, err
		}
		if view.Available {
			view.HeadSHA = row.HeadSHA
		}
	}
	if snapshot.item.RecoveryVersionID == view.VersionID {
		view.DestinationRunnerID = snapshot.item.RecoveryRunnerID
	}
	view.Quiesced = view.Quiesced && !snapshot.unreleasedLease && !issue.Archived && !issue.Terminal
	switch {
	case issue.Archived || issue.Terminal:
		view.Reason = "Restore or explicitly reopen this work item before changing recovery ownership"
	case !view.Available:
		view.Reason = "No verified Hub source is available for recovery"
	case interrupted:
		view.Reason = "Source execution has no acknowledged terminal outcome; acknowledge that work on the source runner has stopped before transfer"
	case !view.Quiesced:
		view.Reason = "Finish or cancel work on the source runner and release its lease before transfer; expiry or an unreachable owner does not prove quiescence"
	default:
		view.Reason = "Verified retained source can be restored before continuing; workflow, human and delivery holds remain in force"
	}
	if err := s.recheckChangeReadAuthority(ctx, scope); err != nil {
		return view, err
	}
	return view, nil
}

func (s *Service) readRuntimeSourceRecoveryTenant(ctx context.Context, scope hubscope.NativeScope, item string, evidence *tracker.NativeRuntimeEvidence) error {
	view, err := s.readNativeSourceRecoveryTenant(ctx, scope, item, evidence.Issue)
	if err == nil && evidence.CurrentVersion != nil && evidence.CurrentVersion.VersionID != view.VersionID {
		err = corestore.ErrSerializationFailure
	}
	if err != nil {
		var failure *hubapi.NativeError
		if errors.As(err, &failure) && failure.Status < http.StatusInternalServerError {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if authorityErr := s.recheckChangeReadAuthority(ctx, scope); authorityErr != nil {
			return authorityErr
		}
		s.config.Logger.Warn("Source recovery projection unavailable", "error", err, "organization_id", scope.Organization, "project_id", scope.Project, "work_item_id", item)
		evidence.SourceRecovery = nil
		if !slices.Contains(evidence.Unavailable, "source_recovery_projection") {
			evidence.Unavailable = append(evidence.Unavailable, "source_recovery_projection")
		}
		return nil
	}
	evidence.Unavailable = slices.DeleteFunc(evidence.Unavailable, func(value string) bool { return value == "source_recovery_projection" })
	if view.AttemptID != "" || view.VersionID != "" {
		evidence.SourceRecovery = &view
	}
	return nil
}

func (s *Service) nativeSourceRecoveryDestinationsTenant(ctx context.Context, scope hubscope.NativeScope, after string) ([]sourceRecoveryRunner, string, error) {
	if len(after) > 256 {
		return nil, "", hubapi.NativePublicInvalid("Destination cursor exceeds 256 bytes")
	}
	approval, err := s.readTenantProjectPolicy(ctx, scope.Organization, scope.Project)
	if err != nil {
		return nil, "", err
	}
	if approval.Policy.ID == "" {
		return []sourceRecoveryRunner{}, "", nil
	}
	now := s.config.Clock().UTC()
	destinations := []sourceRecoveryRunner{}
	var next string
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		candidates, ok := reader.(runnerstore.SourceRecoveryReader)
		if !ok {
			return corestore.ErrInvalidPage
		}
		page, err := candidates.ProjectSourceRecoveryPage(ctx, scope.Project, after, runnerstore.SourceRecoveryPageLimit, now)
		if err != nil {
			return err
		}
		next = page.Next
		urgent, err := reader.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		for _, candidate := range page.Items {
			record := candidate.Record
			runner := runnerFromFleetRecord(record, now, urgent)
			if !record.Isolation.SupportsOn(runner.IsolationTier, runner.OS) {
				continue
			}
			runner.Used, runner.HostUsed = candidate.RunnerUsed, candidate.HostUsed
			runner.ProjectUsed = map[tracker.ProjectID]int{scope.Project: candidate.ProjectUsed}
			runner.ProviderCapacity = candidate.Providers
			if placementRunnerCompatible(hubrecords.PlacementRunner{Runner: runner}, scope.Project, approval.Policy.Requirements, now, false) {
				name := runner.DisplayName
				if name == "" {
					name = runner.Hostname
				}
				destinations = append(destinations, sourceRecoveryRunner{ID: runner.RunnerID, Name: name})
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", hubapi.ProjectRecordError(err)
	}
	return destinations, next, nil
}

func (s *Service) verifiedNativeSourceTenant(ctx context.Context, scope hubscope.NativeScope, row eventstore.Version) (bool, error) {
	if row.OrganizationID != scope.Organization {
		return false, artifact.ErrIntegrity
	}
	content := eventstore.NewContent(s.runDetailStore())
	version, err := content.Version(ctx, row)
	if err != nil {
		return false, err
	}
	if version.Source == nil {
		return false, nil
	}
	_, err = content.SourceBundle(ctx, scope.Organization, row, *version.Source)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if !hubcontent.ExplanationRead(ctx) && (errors.Is(err, artifact.ErrIntegrity) || errors.Is(err, artifact.ErrMissing)) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) transferNativeSource(c echo.Context) error {
	var request sourceTransferRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	result, err := s.transferNativeSourceCommand(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.Blob(http.StatusOK, "application/json", result)
}

func sourceRecoveryRefusal(message string) error {
	return &hubapi.NativeError{Code: "invalid_request", Message: message, Status: http.StatusUnprocessableEntity, PublicMessage: true}
}

func (s *Service) authorizeSourceTransferTenant(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope) error {
	if err := s.authorizeRunnerCapacity(ctx, r, scope); err != nil {
		return err
	}
	if scope.Credential.Hosted != nil {
		member, err := r.AccessMember(ctx, scope.Credential.Hosted.Subject)
		if err != nil {
			return err
		}
		switch hubapi.LesserHostedRole(member.Role, scope.Credential.HostedRole) {
		case "owner", "admin":
		case "member":
			grant, err := r.AccessProjectGrant(ctx, member.UserID, scope.Project)
			if err != nil || !grant.ManageRunner {
				return accessstore.ErrDenied
			}
		default:
			return accessstore.ErrDenied
		}
	}
	return nil
}

func (s *Service) selectNativeSourceTransferTenant(ctx context.Context, scope hubscope.NativeScope, item string, request sourceTransferRequest) (sourceRecoveryView, error) {
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return sourceRecoveryView{}, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return sourceRecoveryView{}, err
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error { return s.authorizeSourceTransferTenant(ctx, r, scope) }); err != nil {
		return sourceRecoveryView{}, hubapi.ProjectRecordError(err)
	}
	view, err := s.readNativeSourceRecoveryTenant(ctx, scope, item)
	if err != nil {
		return view, err
	}
	if request.ExpectedRevision <= 0 || request.ExpectedRevision != view.Revision {
		return view, hubapi.NativeConflict(view.Revision)
	}
	if request.VersionID == "" || request.VersionID != view.VersionID {
		return view, sourceRecoveryRefusal("Re-read the current source version before transfer")
	}
	if !view.Quiesced {
		return view, sourceRecoveryRefusal(view.Reason)
	}
	if request.DestinationRunnerID != "" {
		if !view.Available {
			return view, sourceRecoveryRefusal(view.Reason)
		}
		approval, err := s.readTenantProjectPolicy(ctx, scope.Organization, scope.Project)
		if err != nil {
			return view, err
		}
		if approval.Policy.ID == "" {
			return view, hubrecords.ErrNoApprovedRepositoryPolicy
		}
		err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			return s.validateSourceDestinationTenant(ctx, r, scope, request.DestinationRunnerID, approval.Policy.Requirements)
		})
		if err != nil {
			return view, hubapi.ProjectRecordError(err)
		}
	}
	return view, nil
}

func (s *Service) validateSourceDestinationTenant(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, id string, requirements policy.Requirements) error {
	reader, ok := r.(runnerstore.SourceRecoveryDestinationReader)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	now := s.config.Clock().UTC()
	candidate, err := reader.SourceRecoveryDestination(ctx, scope.Project, id, now)
	if errors.Is(err, corestore.ErrNotFound) {
		return sourceRecoveryRefusal("Destination runner must be online, eligible and have available capacity")
	}
	if err != nil {
		return err
	}
	urgent, err := r.RunnerUrgentUpdate(ctx)
	if err != nil {
		return err
	}
	runner := runnerFromFleetRecord(candidate.Record, now, urgent)
	runner.Used, runner.HostUsed = candidate.RunnerUsed, candidate.HostUsed
	runner.ProjectUsed = map[tracker.ProjectID]int{scope.Project: candidate.ProjectUsed}
	runner.ProviderCapacity = candidate.Providers
	if !placementRunnerCompatible(hubrecords.PlacementRunner{Runner: runner}, scope.Project, requirements, now, false) {
		return sourceRecoveryRefusal("Destination runner must be online, eligible and have available capacity")
	}
	if !candidate.Record.Isolation.SupportsOn(runner.IsolationTier, runner.OS) {
		return sourceRecoveryRefusal("Destination runner must support its configured isolation tier")
	}
	return nil
}
