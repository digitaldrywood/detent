package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
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

func readSourceRecoveryView(ctx context.Context, q nativeQueryer, scope nativeScope, item string) (sourceRecoveryView, tracker.WorkItemID, error) {
	issue, id, err := readNativeIssue(ctx, q, scope, item)
	view := sourceRecoveryView{Destinations: []sourceRecoveryRunner{}, WorkItemID: issue.WorkItemID, Revision: issue.Revision, Reason: "Capture the exact legacy source on its owning runner before transfer; unavailable copies cannot be recovered"}
	if err != nil {
		return view, id, err
	}
	var routedVersion string
	if err := q.QueryRowContext(ctx, "SELECT recovery_runner_id,recovery_version_id FROM issues WHERE id=?", id).Scan(&view.DestinationRunnerID, &routedVersion); err != nil {
		return view, id, err
	}
	source, err := readNativeSourceCheckpoint(ctx, q, scope, item)
	if err != nil {
		return view, id, err
	}
	view.AttemptID, view.SourceMachineID, view.SourceRunnerID, view.SourceRunnerName = source.AttemptID, source.MachineID, source.RunnerID, source.RunnerName
	rawStatus, released := source.Status, source.Released
	if source.Checkpoint != nil {
		view.HeadSHA = source.Checkpoint.HeadSHA
	}

	change, found, err := readLatestNativeChangeRequest(ctx, q, scope, item)
	if err != nil || !found || change.CurrentVersion == "" {
		return view, id, err
	}
	version, err := readChangeVersion(ctx, q, change.ID, change.CurrentVersion)
	if err != nil {
		return view, id, err
	}
	view.VersionID, view.BaseSHA = version.ID, version.BaseSHA
	if routedVersion != version.ID {
		view.DestinationRunnerID = ""
	}
	if view.AttemptID == "" {
		view.HeadSHA, view.AttemptID = version.HeadSHA, version.AttemptID
		err = q.QueryRowContext(ctx, `SELECT l.machine_id, COALESCE(lr.runner_id,''),COALESCE(NULLIF(r.display_name,''),m.hostname,''), a.status, l.released_at,coalesce(a.failure_stage,'')='lease_release' AND a.cleanup_acknowledged_at IS NULL FROM native_attempts a JOIN leases l ON l.lease_id=a.lease_id LEFT JOIN lease_runners lr ON lr.lease_id=l.lease_id LEFT JOIN runner_identities r ON r.id=lr.runner_id AND r.organization_id=a.organization_id LEFT JOIN machines m ON m.id=l.machine_id AND m.organization_id=a.organization_id WHERE a.id=? AND a.organization_id=? AND a.project_id=? AND a.work_item_id=?`, version.AttemptID, scope.organization, scope.project, item).Scan(&view.SourceMachineID, &view.SourceRunnerID, &view.SourceRunnerName, &rawStatus, &released, &source.UnacknowledgedRelease)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return view, id, err
		}
	}
	view.Quiesced = released.Valid && !source.UnacknowledgedRelease && (rawStatus == "succeeded" || rawStatus == "failed" || rawStatus == "cancelled")
	_, claimed, err := readUnreleasedLease(ctx, q, id)
	if err != nil {
		return view, id, err
	}
	view.Quiesced = view.Quiesced && !claimed && !issue.Archived && !issue.Terminal
	allowed, reason, err := nativeSourceClaimAllowed(ctx, q, scope, "", id)
	if err != nil {
		return view, id, err
	}
	view.Available = version.Source != nil && allowed
	if view.Available {
		view.HeadSHA = version.HeadSHA
	}
	if issue.Archived || issue.Terminal {
		view.Reason = "Restore or explicitly reopen this work item before changing recovery ownership"
	} else if !view.Available {
		view.Reason = reason
	} else if !view.Quiesced {
		view.Reason = "Finish or cancel work on the source runner and release its lease before transfer; expiry or an unreachable owner does not prove quiescence"
		if claimed {
			view.Reason = "Finish or cancel the current execution and release its claim before transfer; expiry alone does not prove the writer stopped"
		} else if released.Valid {
			view.Reason = "The source attempt has no acknowledged terminal outcome; finish or cancel work on the source runner before transfer; lease release alone does not prove quiescence"
		}
	} else {
		view.Reason = "Verified retained source can be restored before continuing; workflow, human and delivery holds remain in force"
	}
	return view, id, nil
}

func (s *Service) getNativeSourceRecovery(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	if s.config.TenantRepository != nil {
		view, err := s.readNativeSourceRecoveryTenant(ctx, scope, c.Param("item"), c.QueryParam("destinations_after"))
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, view)
	}
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer tx.Rollback()
	view, _, err := readSourceRecoveryView(ctx, tx, scope, c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	approval, found, err := readProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if found {
		runners, err := readPlacementRunners(ctx, tx, scope, s.config.now())
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		for _, r := range runners {
			if placementRunnerCompatible(r, scope.project, approval.Policy.Requirements, s.config.now(), false) && validateReadRunnerIsolation(ctx, tx, scope, r.Runner) == nil {
				view.Destinations = append(view.Destinations, sourceRecoveryRunner{ID: r.RunnerID, Name: r.DisplayName})
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, view)
}

func (s *Service) readNativeSourceRecoveryTenant(ctx context.Context, scope nativeScope, item, after string) (sourceRecoveryView, error) {
	recovery, err := s.readNativeRecoveryTenant(ctx, scope, item)
	if err != nil {
		return sourceRecoveryView{}, err
	}
	view := sourceRecoveryView{
		Destinations: []sourceRecoveryRunner{}, WorkItemID: recovery.Issue.WorkItemID,
		Revision: recovery.Issue.Revision,
		Reason:   "Capture the exact legacy source on its owning runner before transfer; unavailable copies cannot be recovered",
	}
	view.Destinations, view.DestinationsNext, err = s.nativeSourceRecoveryDestinationsTenant(ctx, scope, after)
	if err != nil {
		return sourceRecoveryView{}, err
	}
	if attempt := recovery.SourceAttempt(); attempt != nil {
		view.AttemptID = attempt.AttemptID
		view.SourceMachineID = attempt.MachineID
		view.SourceRunnerID = attempt.RunnerID
		view.SourceRunnerName, err = s.nativeSourceRunnerNameTenant(ctx, scope, attempt)
		if err != nil {
			return sourceRecoveryView{}, err
		}
		if attempt.Checkpoint != nil {
			view.HeadSHA = attempt.Checkpoint.HeadSHA
		}
		unacknowledgedRelease := attempt.TerminalFailure != nil && attempt.TerminalFailure.Stage == "lease_release" && attempt.CleanupAcknowledgedAt == nil
		view.Quiesced = !attempt.Current && attempt.ClaimReleasedAt != nil && !unacknowledgedRelease &&
			(attempt.Status == "succeeded" || attempt.Status == "failed" || attempt.Status == "cancelled")
	}
	if detail := recovery.ChangeDetail; detail != nil {
		for _, version := range detail.Versions {
			if version.ID != detail.Change.CurrentVersion {
				continue
			}
			view.VersionID, view.BaseSHA = version.ID, version.BaseSHA
			if view.AttemptID == "" {
				view.AttemptID = version.AttemptID
			}
			verified, err := s.verifiedNativeSourceTenant(ctx, scope, recovery.Issue.WorkItemID, detail.Change.ID, version)
			if err != nil {
				return sourceRecoveryView{}, err
			}
			view.Available = verified
			if verified {
				view.HeadSHA = version.HeadSHA
			}
			break
		}
	}
	view.Quiesced = view.Quiesced && !recovery.Issue.Archived && !recovery.Issue.Terminal
	switch {
	case recovery.Issue.Archived || recovery.Issue.Terminal:
		view.Reason = "Restore or explicitly reopen this work item before changing recovery ownership"
	case !view.Available:
		view.Reason = "No verified Hub source is available for recovery"
	case !view.Quiesced:
		view.Reason = "Finish or cancel work on the source runner and release its lease before transfer; expiry or an unreachable owner does not prove quiescence"
	default:
		view.Reason = "Verified retained source can be restored before continuing; workflow, human and delivery holds remain in force"
	}
	return view, nil
}

func (s *Service) nativeSourceRecoveryDestinationsTenant(ctx context.Context, scope nativeScope, after string) ([]sourceRecoveryRunner, string, error) {
	if len(after) > 256 {
		return nil, "", nativePublicInvalid("Destination cursor exceeds 256 bytes")
	}
	approval, err := s.readTenantProjectPolicy(ctx, scope.organization, scope.project)
	if err != nil {
		return nil, "", err
	}
	if approval.Policy.ID == "" {
		return []sourceRecoveryRunner{}, "", nil
	}
	now := s.config.now().UTC()
	destinations := []sourceRecoveryRunner{}
	var next string
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		candidates, ok := reader.(runnerstore.SourceRecoveryReader)
		if !ok {
			return corestore.ErrInvalidPage
		}
		page, err := candidates.ProjectSourceRecoveryPage(ctx, scope.project, after, runnerstore.SourceRecoveryPageLimit, now)
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
			runner.ProjectUsed = map[tracker.ProjectID]int{scope.project: candidate.ProjectUsed}
			runner.ProviderCapacity = candidate.Providers
			if placementRunnerCompatible(placementRunner{Runner: runner}, scope.project, approval.Policy.Requirements, now, false) {
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
		return nil, "", projectRecordError(err)
	}
	return destinations, next, nil
}

func (s *Service) nativeSourceRunnerNameTenant(ctx context.Context, scope nativeScope, attempt *tracker.NativeAttempt) (string, error) {
	var name string
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		identity, err := reader.RunnerForLease(ctx, string(attempt.LeaseID))
		if err == nil && identity.DisplayName != "" {
			name = identity.DisplayName
			return nil
		}
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		machine, machineErr := reader.Machine(ctx, string(attempt.MachineID))
		if machineErr != nil {
			if errors.Is(machineErr, corestore.ErrNotFound) {
				name = string(attempt.MachineID)
				return nil
			}
			return machineErr
		}
		name = machine.Hostname
		return nil
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return string(attempt.MachineID), nil
	}
	return name, projectRecordError(err)
}

func (s *Service) verifiedNativeSourceTenant(ctx context.Context, scope nativeScope, item tracker.NativeWorkItemID, changeID string, selected tracker.ChangeVersion) (bool, error) {
	var current string
	var row eventstore.Version
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		change, err := reader.Change(ctx, changeID)
		if err != nil {
			return err
		}
		if item == "" || change.OrganizationID != scope.organization || change.ProjectID != scope.project || change.WorkItemID != item || selected.ChangeID != changeID {
			return corestore.ErrNotFound
		}
		current = ""
		if change.CurrentVersionID != nil {
			current = *change.CurrentVersionID
		}
		if current != selected.ID {
			return nil
		}
		row, err = reader.Version(ctx, changeID, selected.ID)
		return err
	})
	if err != nil {
		return false, projectRecordError(err)
	}
	if current != selected.ID || selected.Source == nil {
		return false, nil
	}
	content := eventstore.NewContent(s.runDetailStore())
	contentCtx := s.runDetailContext(s.contentContext(ctx))
	version, err := content.Version(contentCtx, row)
	if err != nil {
		return false, err
	}
	if version.Source == nil {
		return false, nil
	}
	bundle, err := content.SourceBundle(contentCtx, scope.organization, row, *version.Source)
	if err != nil {
		if contentCtx.Err() != nil {
			return false, contentCtx.Err()
		}
		if errors.Is(err, artifact.ErrIntegrity) {
			return false, nil
		}
		return false, err
	}
	return version.Source.Validate(version.BaseSHA, version.HeadSHA, bundle) == nil, nil
}

func (s *Service) transferNativeSource(c echo.Context) error {
	var request sourceTransferRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	result, err := s.transferNativeSourceCommand(c.Request().Context(), nativeRequestScope(c), c.Param("item"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.Blob(http.StatusOK, "application/json", result)
}

func (s *Service) transferNativeSourceCommand(ctx context.Context, scope nativeScope, item string, request sourceTransferRequest) (json.RawMessage, error) {
	if s.config.TenantRepository != nil {
		if _, err := s.selectNativeSourceTransferTenant(ctx, scope, item, request); err != nil {
			return nil, err
		}
		return nil, workItemStorageUnavailable()
	}
	options := nativeCommandOptions{OperationID: "source_recovery " + string(scope.organization) + "/" + string(scope.project) + "/" + item, Item: item, Feature: "collaboration", RunnerAdministration: true}
	return s.executeNativeMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		view, id, err := readSourceRecoveryView(ctx, tx, scope, item)
		if err != nil {
			return nil, err
		}
		if request.ExpectedRevision <= 0 || request.ExpectedRevision != view.Revision {
			return nil, nativeConflict(view.Revision)
		}
		if request.VersionID == "" || request.VersionID != view.VersionID {
			return nil, sourceRecoveryRefusal("Re-read the current source version before transfer")
		}
		if !view.Quiesced {
			return nil, sourceRecoveryRefusal(view.Reason)
		}
		if request.DestinationRunnerID != "" {
			if !view.Available {
				return nil, sourceRecoveryRefusal(view.Reason)
			}
			approval, err := requireProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
			if err != nil {
				return nil, err
			}
			runners, err := readPlacementRunners(ctx, tx, scope, now)
			if err != nil {
				return nil, err
			}
			var destination *placementRunner
			for i := range runners {
				if runners[i].RunnerID == request.DestinationRunnerID {
					destination = &runners[i]
					break
				}
			}
			if destination == nil || !placementRunnerCompatible(*destination, scope.project, approval.Policy.Requirements, now, false) {
				return nil, sourceRecoveryRefusal("Destination runner must be online, eligible and have available capacity")
			}
			if err := validateReadRunnerIsolation(ctx, tx, scope, destination.Runner); err != nil {
				return nil, sourceRecoveryRefusal("Destination runner must support its configured isolation tier")
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE issues SET recovery_runner_id=?,recovery_version_id=?,revision=revision+1,native_updated_at=?,updated_at=? WHERE id=?`, request.DestinationRunnerID, view.VersionID, formatHubTime(now), formatHubTime(now), id)
		if err != nil {
			return nil, err
		}
		if err := requestNativeDispatch(ctx, tx, scope, item, now); err != nil {
			return nil, err
		}
		view.DestinationRunnerID, view.Revision = request.DestinationRunnerID, view.Revision+1
		issue, _, err := readNativeIssue(ctx, tx, scope, item)
		if err != nil {
			return nil, err
		}
		audit, err := json.Marshal(view)
		if err != nil {
			return nil, err
		}
		if err := recordNativeChange(ctx, tx, scope, issue, item, view.Revision, "issue.edited", tracker.CollaborationData{Revision: view.Revision, Fields: []string{"recovery_runner_id"}, Operation: "source_recovery", ReasonDetail: string(audit)}, now); err != nil {
			return nil, err
		}
		return view, nil
	})
}

func sourceRecoveryRefusal(message string) error {
	return &nativeError{Code: "invalid_request", Message: message, status: http.StatusUnprocessableEntity, publicMessage: true}
}

func (s *Service) authorizeSourceTransferTenant(ctx context.Context, r tenantstore.Reader, scope nativeScope) error {
	if err := s.authorizeRunnerCapacity(ctx, r, scope); err != nil {
		return err
	}
	if scope.credential.Hosted != nil {
		member, err := r.AccessMember(ctx, scope.credential.Hosted.Subject)
		if err != nil {
			return err
		}
		switch lesserHostedRole(member.Role, scope.credential.HostedRole) {
		case "owner", "admin":
		case "member":
			grant, err := r.AccessProjectGrant(ctx, member.UserID, scope.project)
			if err != nil || !grant.ManageRunner {
				return accessstore.ErrDenied
			}
		default:
			return accessstore.ErrDenied
		}
	}
	return nil
}

func (s *Service) selectNativeSourceTransferTenant(ctx context.Context, scope nativeScope, item string, request sourceTransferRequest) (sourceRecoveryView, error) {
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return sourceRecoveryView{}, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return sourceRecoveryView{}, err
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error { return s.authorizeSourceTransferTenant(ctx, r, scope) }); err != nil {
		return sourceRecoveryView{}, projectRecordError(err)
	}
	view, err := s.readNativeSourceRecoveryTenant(ctx, scope, item, "")
	if err != nil {
		return view, err
	}
	if request.ExpectedRevision <= 0 || request.ExpectedRevision != view.Revision {
		return view, nativeConflict(view.Revision)
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
		approval, err := s.readTenantProjectPolicy(ctx, scope.organization, scope.project)
		if err != nil {
			return view, err
		}
		if approval.Policy.ID == "" {
			return view, errNoApprovedRepositoryPolicy
		}
		err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			return s.validateSourceDestinationTenant(ctx, r, scope, request.DestinationRunnerID, approval.Policy.Requirements)
		})
		if err != nil {
			return view, projectRecordError(err)
		}
	}
	return view, nil
}

func (s *Service) validateSourceDestinationTenant(ctx context.Context, r tenantstore.Reader, scope nativeScope, id string, requirements policy.Requirements) error {
	reader, ok := r.(runnerstore.SourceRecoveryDestinationReader)
	if !ok {
		return workItemStorageUnavailable()
	}
	now := s.config.now().UTC()
	candidate, err := reader.SourceRecoveryDestination(ctx, scope.project, id, now)
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
	runner.ProjectUsed = map[tracker.ProjectID]int{scope.project: candidate.ProjectUsed}
	runner.ProviderCapacity = candidate.Providers
	if !placementRunnerCompatible(placementRunner{Runner: runner}, scope.project, requirements, now, false) {
		return sourceRecoveryRefusal("Destination runner must be online, eligible and have available capacity")
	}
	if !candidate.Record.Isolation.SupportsOn(runner.IsolationTier, runner.OS) {
		return sourceRecoveryRefusal("Destination runner must support its configured isolation tier")
	}
	return nil
}
