package hubserver

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/detailbatch"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const BoardMaintenanceBytes = hubmaintenance.ByteLimit

type boardRefreshError struct {
	item  tracker.NativeWorkItemID
	step  string
	cause error
}

func (e *boardRefreshError) Error() string { return e.cause.Error() }
func (e *boardRefreshError) Unwrap() error { return e.cause }

type boardProjectionSnapshot struct {
	organizationSlug   string
	changePrincipalID  string
	project            projectstore.Record
	item               corestore.WorkItem
	version            eventstore.CollaborationVersion
	transition         *eventstore.Event
	attempt            *hubrecords.TenantAttemptSnapshot
	attemptCount       int64
	lease              *leasestore.Lease
	runnerID           string
	diagnostics        *runnerauth.ProjectDiagnostics
	change             *eventstore.Change
	changeVersion      *eventstore.Version
	pullRequestVersion *eventstore.PullRequestVersion
}

func (s *Service) maintainBoardRecords(ctx context.Context) error {
	organization := tracker.OrganizationID(s.appOrganizationID())
	if organization == "" {
		return corestore.ErrTenantMismatch
	}
	var failures []error
	attempted := make([]tracker.NativeWorkItemID, 0, 32)
	for range 32 {
		next, processed, err := s.maintainBoardRecord(ctx, organization, tracker.NativeWorkItemID(s.boardRefreshCursor), attempted...)
		s.boardRefreshCursor = string(next)
		if err != nil {
			failures = append(failures, err)
		}
		if !processed {
			return errors.Join(failures...)
		}
		attempted = append(attempted, next)
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
	}
	return errors.Join(failures...)
}

func (s *Service) MaintainBoardRecord(ctx context.Context, organization tracker.OrganizationID, after tracker.NativeWorkItemID, attempted ...tracker.NativeWorkItemID) (tracker.NativeWorkItemID, bool, error) {
	if err := hubmaintenance.RequireAllowance(ctx, string(organization), BoardMaintenanceBytes); err != nil {
		return after, false, err
	}
	if _, ok := s.runDetailStore().(*hubmaintenance.Details); !ok {
		return after, false, errors.New("maintenance requires byte-limited payload storage")
	}
	ctx = detailbatch.Clear(ctx)
	ctx = context.WithValue(ctx, hubcontent.ChangeContentKey{}, nil)
	ctx = context.WithValue(ctx, hubcontent.RunDetailContextKey{}, nil)
	return s.maintainBoardRecord(ctx, organization, after, attempted...)
}

func (s *Service) maintainBoardRecord(ctx context.Context, organization tracker.OrganizationID, after tracker.NativeWorkItemID, attempted ...tracker.NativeWorkItemID) (tracker.NativeWorkItemID, bool, error) {
	if organization == "" || s.config.TenantRepository == nil {
		return after, false, corestore.ErrTenantMismatch
	}
	var candidate viewstore.BoardCandidate
	var settled bool
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		candidate, err = r.NextBoardCandidate(ctx, after, attempted...)
		if err == nil && !candidate.Required {
			settled, err = boardRecordSettled(ctx, r, candidate)
			if err != nil {
				return &boardRefreshError{item: candidate.WorkItemID, step: "candidate_state", cause: err}
			}
		}
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return after, false, err
	}
	if settled {
		return candidate.WorkItemID, true, nil
	}
	err = s.refreshBoardRecord(ctx, organization, candidate, s.config.Clock())
	if errors.Is(err, corestore.ErrRevisionConflict) {
		err = nil
	}
	return candidate.WorkItemID, true, err
}

func boardRecordSettled(ctx context.Context, r tenantstore.Reader, candidate viewstore.BoardCandidate) (bool, error) {
	card, err := r.BoardItem(ctx, candidate.ProjectID, candidate.WorkItemID)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil || !card.Display.Ready || card.PageIssue == nil || card.ListIssue == nil || card.RefreshAfter != nil || card.Display.Change != nil || card.Display.Attempt != nil || card.Display.Execution != nil || card.ExpiresAt != nil {
		return false, err
	}
	item, err := r.WorkItem(ctx, candidate.ProjectID, candidate.WorkItemID)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil || card.SourceRevision != item.Revision || card.SourceEventSequence != item.EventSequence {
		return false, err
	}
	project, err := r.Project(ctx, candidate.ProjectID)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil || project.Profile != "native" || project.DeletedAt != nil || card.SourceProjectRevision != project.ContentSettingsRevision {
		return false, err
	}
	if _, err := r.LatestChange(ctx, candidate.ProjectID, candidate.WorkItemID); !errors.Is(err, corestore.ErrNotFound) {
		return false, err
	}
	if _, err := r.LatestAttempt(ctx, item.ID); !errors.Is(err, corestore.ErrNotFound) {
		return false, err
	}
	if _, err := r.ActiveLeaseForIssue(ctx, item.ID); !errors.Is(err, corestore.ErrNotFound) {
		return false, err
	}
	return true, nil
}

func (s *Service) boardProjectionSnapshot(ctx context.Context, organization tracker.OrganizationID, candidate viewstore.BoardCandidate, now time.Time) (snapshot boardProjectionSnapshot, err error) {
	step := "project"
	defer func() {
		if err != nil {
			err = &boardRefreshError{item: candidate.WorkItemID, step: step, cause: err}
		}
	}()
	owner, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return snapshot, hubapi.NativeRunEventStorageUnavailable()
	}
	err = owner.ReadTenantSnapshot(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		snapshot.project.Core, err = r.Project(ctx, candidate.ProjectID)
		if err != nil {
			return err
		}
		if snapshot.project.Core.DeletedAt == nil && snapshot.project.Core.Profile == "native" {
			step = "project_record"
			snapshot.project, err = r.ProjectRecord(ctx, candidate.ProjectID)
			if err != nil {
				return err
			}
		}
		organizationRecord, err := r.AccessOrganization(ctx)
		if err != nil {
			return err
		}
		snapshot.organizationSlug = organizationRecord.Slug
		step = "work_item"
		snapshot.item, err = r.WorkItem(ctx, candidate.ProjectID, candidate.WorkItemID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if snapshot.project.Core.DeletedAt != nil || snapshot.project.Core.Profile != "native" {
			return nil
		}
		step = "collaboration_version"
		snapshot.version, err = r.CollaborationVersion(ctx, string(candidate.WorkItemID), snapshot.item.Revision)
		if err != nil {
			return err
		}
		step = "latest_transition"
		transitions, ok := r.(eventstore.WorkflowTransitionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		transition, err := transitions.LatestWorkflowTransition(ctx, candidate.ProjectID, candidate.WorkItemID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil {
			if transition.OrganizationID != organization || transition.ProjectID != candidate.ProjectID || transition.WorkItemID != candidate.WorkItemID || transition.Type != "workflow.transitioned" || transition.Sequence <= 0 || transition.Sequence > snapshot.item.EventSequence {
				return eventstore.ErrInvalidRecord
			}
			snapshot.transition = &transition
		}
		step = "latest_attempt"
		attempt, err := r.LatestAttempt(ctx, snapshot.item.ID)
		if err == nil {
			value := hubrecords.TenantAttemptSnapshot{Attempt: attempt}
			step = "attempt_lease"
			value.Lease, err = r.Lease(ctx, attempt.LeaseID)
			if err != nil {
				return err
			}
			current := boardAttemptCurrent(value, now)
			recoverClaim := attempt.Status == "succeeded" && attempt.ClaimDisposition == nil
			if current || recoverClaim {
				step = "run_event"
				value.Event, err = r.LatestRunEvent(ctx, candidate.ProjectID, attempt.ID)
				if err != nil && (current || !errors.Is(err, corestore.ErrNotFound)) {
					return err
				}
				if recoverClaim && value.Event.ID != "" {
					if err := hubrecords.ReadTenantAttemptPayload(ctx, r, &value); err != nil {
						return err
					}
				} else if value.Event.ID != "" {
					step = "attempt_checkpoint"
					if err := hubrecords.ReadTenantAttemptCheckpointPayload(ctx, r, &value); err != nil {
						return err
					}
				}
			}
			snapshot.attempt = &value
			step = "attempt_count"
			snapshot.attemptCount, err = r.BoardAttemptCount(ctx, snapshot.item.ID)
			if err != nil {
				return err
			}
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		step = "active_lease"
		lease, err := r.ActiveLeaseForIssue(ctx, snapshot.item.ID)
		if err == nil {
			snapshot.lease = &lease
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if lease := snapshot.lease; lease != nil && lease.RunnerID != nil && *lease.RunnerID != "" && lease.ProjectID != nil && *lease.ProjectID == candidate.ProjectID && lease.ReleasedAt == nil && !now.Before(lease.RenewedAt) && now.Before(lease.ExpiresAt) {
			step = "execution_runner"
			runnerReader, ok := r.(interface {
				LeaseMutationIdentity(context.Context, string) (runnerstore.Identity, error)
			})
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			identity, err := runnerReader.LeaseMutationIdentity(ctx, lease.LeaseID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if err == nil && identity.OrganizationID == organization && identity.RunnerID == *lease.RunnerID && string(identity.MachineID) == lease.MachineID && identity.RemovedAt == nil && now.Before(identity.ExpiresAt) {
				allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, candidate.ProjectID, now)
				if err != nil {
					return err
				}
				if allowed {
					step = "execution_observation"
					observation, _, err := r.RunnerProjectObservation(ctx, identity.RunnerID, candidate.ProjectID, string(candidate.WorkItemID))
					if err != nil && !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
					if err == nil && observation.OrganizationID == organization && observation.RunnerID == identity.RunnerID && observation.ProjectID == candidate.ProjectID {
						snapshot.runnerID, snapshot.diagnostics = identity.RunnerID, observation.Diagnostics
					}
				}
			}
		}
		step = "pull_request_version"
		retained, err := r.WorkItemPullRequestVersion(ctx, candidate.ProjectID, candidate.WorkItemID)
		if err == nil {
			snapshot.pullRequestVersion = &retained
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		step = "latest_change"
		change, err := r.LatestChange(ctx, candidate.ProjectID, candidate.WorkItemID)
		if err == nil {
			snapshot.change = &change
			step = "change_item"
			primary, err := r.WorkItemByNativeID(ctx, change.WorkItemID)
			if err != nil {
				return err
			}
			if primary.ActorPrincipalID != nil {
				snapshot.changePrincipalID = *primary.ActorPrincipalID
			}

			if change.CurrentVersionID != nil {
				step = "change_version"
				version, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
				if err != nil {
					return err
				}
				snapshot.changeVersion = &version
			}
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		return nil
	})
	return snapshot, err
}

func (s *Service) refreshBoardRecord(ctx context.Context, organization tracker.OrganizationID, candidate viewstore.BoardCandidate, now time.Time) (err error) {
	step := "snapshot"
	defer func() {
		var failure *boardRefreshError
		if err != nil && !errors.As(err, &failure) {
			err = &boardRefreshError{item: candidate.WorkItemID, step: step, cause: err}
		}
	}()
	snapshot, err := s.boardProjectionSnapshot(ctx, organization, candidate, now)
	if err != nil {
		return err
	}
	var runtimeSummary *leasestore.AttemptEvent
	var recoveredClaimFacts *leasestore.AttemptClaimFacts
	update := viewstore.BoardUpdate{ProjectID: candidate.ProjectID, WorkItemID: candidate.WorkItemID, ExpectedRevision: snapshot.item.Revision, ExpectedEventSequence: snapshot.item.EventSequence, ExpectedProjectRevision: snapshot.project.Core.ContentSettingsRevision}
	if snapshot.item.ID != 0 && snapshot.project.Core.DeletedAt == nil && snapshot.project.Core.Profile == "native" {
		step = "issue_payload"
		issue, err := s.hydrateWorkItemRecord(ctx, snapshot.item, snapshot.version)
		if err != nil {
			return err
		}
		if snapshot.transition != nil {
			step = "transition_payload"
			transition, err := eventstore.NewContent(s.runDetailStore()).Event(ctx, *snapshot.transition)
			if err != nil {
				return err
			}
			issue.LatestTransitionReason = transition.Data.ReasonDetail
			if issue.LatestTransitionReason == "" {
				issue.LatestTransitionReason = transition.Data.Reason
			}
		}
		card := tracker.NativeBoardCard{Issue: issue}
		var latest *tracker.NativeAttempt
		if historical := snapshot.attempt; historical != nil {
			if historical.Attempt.Status == "succeeded" && historical.Attempt.ClaimDisposition == nil && historical.Event.ID != "" {
				facts, recoverErr := s.prepareAttemptClaimFacts(ctx, *historical)
				if recoverErr == nil {
					recoveredClaimFacts = &facts
				} else if s.config.Logger != nil {
					s.config.Logger.Warn("attempt claim facts remain unavailable", "organization_id", organization, "project_id", candidate.ProjectID, "work_item_id", candidate.WorkItemID, "attempt_id", historical.Attempt.ID, "error", recoverErr)
				}
			}
			step = "attempt_payload"
			attempt, present, err := s.boardAttemptDetails(ctx, *historical, now)
			if err != nil {
				return err
			}
			if present {
				if historical.Attempt.Status == "running" && historical.Attempt.RuntimeSummarySequence != historical.Attempt.Sequence {
					runtimeSummary = &leasestore.AttemptEvent{ID: historical.Attempt.ID, Sequence: historical.Attempt.Sequence}
					if attempt.Runtime != nil {
						heartbeat := attempt.Runtime.HeartbeatAt
						runtimeSummary.RuntimeHeartbeatAt = &heartbeat
					}
				}
				latest = attempt
				identity, recorded := nativeRecordedAttemptIdentity(historical.Attempt, true)
				if !recorded {
					identity = nativeAttemptDisplayIdentity(*attempt)
				}
				card.Attempt = &tracker.NativeBoardAttempt{Count: int(snapshot.attemptCount), ID: attempt.AttemptID, Status: attempt.Status, RunnerID: attempt.RunnerID, MachineID: string(attempt.MachineID), Identity: identity, StartedAt: attempt.StartedAt, ExpiresAt: attempt.LeaseExpiresAt}
			} else {
				status := historical.Attempt.Status
				if status == "running" {
					status = "interrupted"
				}
				identity, _ := nativeRecordedAttemptIdentity(historical.Attempt, true)
				card.Attempt = &tracker.NativeBoardAttempt{Count: int(snapshot.attemptCount), ID: historical.Attempt.ID, Status: status, MachineID: historical.Lease.MachineID, Identity: identity, StartedAt: historical.Attempt.StartedAt, ExpiresAt: historical.Lease.ExpiresAt}
			}
		}
		if lease := snapshot.lease; lease != nil && lease.ReleasedAt == nil && !now.Before(lease.RenewedAt) && now.Before(lease.ExpiresAt) {
			card.Execution = &tracker.NativeBoardExecution{ID: lease.LeaseID, ExpiresAt: lease.ExpiresAt}
			if card.Attempt != nil && snapshot.attempt.Lease.LeaseID == lease.LeaseID {
				card.Execution.ID, card.Execution.Running = card.Attempt.ID, card.Attempt.Status == "running"
				card.Execution.RunnerID, card.Execution.Identity, card.Execution.StartedAt = card.Attempt.RunnerID, card.Attempt.Identity, &card.Attempt.StartedAt
				if latest != nil && latest.Runtime != nil && card.Execution.Running {
					card.Execution.Stage = latest.Runtime.Phase
					if card.Execution.Stage == "completed" {
						card.Execution.Running, card.Execution.Stage = false, ""
					}
				}
			}
		}
		if card.Execution != nil && snapshot.lease != nil {
			terminal := snapshot.attempt != nil && snapshot.attempt.Lease.LeaseID == snapshot.lease.LeaseID && (snapshot.attempt.Attempt.Status != "running" || latest != nil && latest.Runtime != nil && latest.Runtime.Phase == "completed")
			applyBoardExecutionDiagnostics(card.Execution, *snapshot.lease, latest, terminal, snapshot.runnerID, snapshot.diagnostics, string(candidate.WorkItemID), now)
		}
		if snapshot.change != nil {
			card.Change = &tracker.NativeBoardChange{ID: snapshot.change.ID, Title: snapshot.change.Title}
			if v := snapshot.changeVersion; v != nil && v.ExternalProvider != "" {
				card.Change.External = &tracker.ChangeExternalReference{Provider: v.ExternalProvider, ID: v.ExternalID, URL: v.ExternalURL}
			}
		}
		if card.Change != nil {
			scope := hubscope.NativeScope{Organization: organization, Project: candidate.ProjectID}
			step = "change_rows"
			changes, supported := s.prepareBoardChangeRowsFor(ctx, scope, []tracker.NativeBoardCard{card}, corestore.Background)
			if changes.err != nil {
				return changes.err
			}
			if !supported {
				return corestore.ErrNotFound
			}
			if row := changes.rows[tenantstore.BoardChangeRequest{WorkItemID: candidate.WorkItemID, ChangeID: card.Change.ID}]; row.Change == nil {
				card.Change, snapshot.changePrincipalID = nil, ""
			} else {
				step = "change_summary"
				summary, err := changes.summary(ctx, s, scope, candidate.WorkItemID, card.Change.ID, now)
				if err != nil {
					return err
				}
				card.Change.Status = summary.Status
			}
		}
		card.StatusLine = nativeBoardStatusLine(card)
		record := boardProjectionRecord(snapshot, card)
		record.ProjectedAt = now
		issue.WebURL = hubrecords.IssueBrowserPath(snapshot.organizationSlug, snapshot.project.Core.Slug, issue.Number)
		step = "issue_change"
		if err := s.prepareNativeIssueChangeRecord(ctx, snapshot, &issue); err != nil {
			return err
		}
		projected, reporter, err := s.boardProjectedRelationshipIssues(ctx, organization, candidate.ProjectID, issue)
		if err != nil {
			return err
		}
		if record.Terminal && record.ClosedAt != nil {
			for index, span := range nativeShippedWindowSpans {
				expires := record.ClosedAt.Add(span.span).Add(time.Microsecond)
				if now.Before(expires) && !record.ClosedAt.After(now) {
					record.WindowMask |= 1 << index
					if record.RefreshAfter == nil || expires.Before(*record.RefreshAfter) {
						record.RefreshAfter = &expires
					}
				}
			}
		}
		if record.ExpiresAt != nil && now.Before(*record.ExpiresAt) && (record.RefreshAfter == nil || record.ExpiresAt.Before(*record.RefreshAfter)) {
			record.RefreshAfter = record.ExpiresAt
		}
		projected = s.nativeIssueResponse(projected)
		board, list, boundedReporter, listBytes, err := boundBoardProjection(projected, reporter)
		if err != nil {
			return err
		}
		step = "board_payload"
		stored := card
		stored.Issue.Blockers, stored.Issue.Dependents, stored.Issue.Related = board.Blockers, board.Dependents, board.Related
		payload, err := viewstore.NewContent(s.runDetailStore()).PrepareBoard(ctx, stored)
		if err != nil {
			return err
		}
		record.ListIssue = &list
		record.ListBytes = listBytes
		record.PageIssue = &board
		record.ReporterRelations = &boundedReporter
		record.Payload = payload
		update.Current = &record
	}
	step = "replace_board_item"
	err = s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		if err := w.ReplaceBoardItem(ctx, update); err != nil {
			return err
		}
		if runtimeSummary != nil {
			if err := w.SetAttemptRuntimeHeartbeat(ctx, runtimeSummary.ID, runtimeSummary.Sequence, runtimeSummary.RuntimeHeartbeatAt); err != nil {
				return err
			}
		}
		if recoveredClaimFacts != nil {
			if err := w.SetAttemptClaimFacts(ctx, *recoveredClaimFacts); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil && update.Current != nil && candidate.QueuedAt != nil && s.config.Logger != nil {
		s.config.Logger.Info("board projection checkpoint", "organization_id", organization, "project_id", candidate.ProjectID, "work_item_id", candidate.WorkItemID, "source_revision", snapshot.item.Revision, "source_event_sequence", snapshot.item.EventSequence, "projected_at", now, "projection_lag", max(time.Duration(0), now.Sub(*candidate.QueuedAt)))
	}
	return err
}

func boardAttemptCurrent(snapshot hubrecords.TenantAttemptSnapshot, now time.Time) bool {
	return snapshot.Attempt.Status == "running" && snapshot.Lease.ReleasedAt == nil && now.Before(snapshot.Lease.ExpiresAt)
}

func (s *Service) boardAttemptDetails(ctx context.Context, snapshot hubrecords.TenantAttemptSnapshot, now time.Time) (*tracker.NativeAttempt, bool, error) {
	if !boardAttemptCurrent(snapshot, now) {
		return nil, false, nil
	}
	if snapshot.Event.ID == "" {
		return nil, false, corestore.ErrNotFound
	}
	attempt, err := s.hydrateNativeAttemptTenant(ctx, snapshot, now)
	if err != nil {
		return nil, false, err
	}
	return &attempt, true, nil
}

func boardProjectionRecord(snapshot boardProjectionSnapshot, card tracker.NativeBoardCard) viewstore.BoardRecord {
	issue := card.Issue
	position := viewstore.BoardPosition{Lane: 2147483647, State: issue.State, Priority: 4, ActivityMissing: issue.LastActivityAt.IsZero(), Identifier: string(issue.ProjectID) + "#" + strconv.Itoa(issue.Number)}
	dispatchable := false
	for index, state := range snapshot.project.States {
		if state.Name == issue.State {
			position.Lane = index
			dispatchable = state.Dispatchable
			break
		}
	}
	if issue.Priority != nil && *issue.Priority >= 0 && *issue.Priority <= 3 {
		position.Priority = *issue.Priority
	}
	if issue.Terminal {
		position.Priority = 0
	}
	if !issue.LastActivityAt.IsZero() {
		position.Activity = &issue.LastActivityAt
	}
	record := viewstore.BoardRecord{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, SourceRevision: snapshot.item.Revision, SourceEventSequence: snapshot.item.EventSequence, SourceProjectRevision: snapshot.project.Core.ContentSettingsRevision, Position: position, Archived: issue.Archived, Terminal: issue.Terminal, Running: false, Dispatchable: dispatchable, ClosedAt: issue.ClosedAt, ActorPrincipalID: issue.Actor.PrincipalID, IssueType: issue.Type, Priority: issue.Priority, CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt, Labels: issue.Labels, Assignees: issue.Assignees}
	record.Display = viewstore.BoardDisplay{Ready: true, Provenance: issue.Provenance, ChangePrincipalID: snapshot.changePrincipalID, Title: issue.Title, Number: issue.Number, Actor: issue.Actor, Reason: issue.LatestTransitionReason, StatusLine: card.StatusLine, Attempt: card.Attempt, Execution: card.Execution, Change: card.Change}
	record.Display.Actor.PrincipalID = ""
	if issue.Verification != nil {
		record.VerificationState = issue.Verification.State
	}
	record.Display.Exact = exactBoardDisplay(record, card)
	if card.Execution != nil {
		record.Running = !issue.Terminal && card.Execution.Running
		record.ExpiresAt = &card.Execution.ExpiresAt
		record.ExecutionFence = snapshot.lease.FencingToken
	}
	return record
}

func applyBoardExecutionDiagnostics(execution *tracker.NativeBoardExecution, lease leasestore.Lease, latest *tracker.NativeAttempt, terminal bool, runner string, d *runnerauth.ProjectDiagnostics, item string, now time.Time) {
	started := lease.AcquiredAt
	if runner != "" {
		execution.RunnerID = runner
	}
	if d == nil || d.ObservedAt.Before(started) || d.ObservedAt.After(now) {
		return
	}
	asOf := d.RuntimeObservedAt
	if asOf.IsZero() {
		asOf = d.ObservedAt
	}
	fresh := !asOf.Before(started) && !asOf.After(now) && now.Sub(asOf) <= runnerauth.HeartbeatTimeout && now.Sub(d.ObservedAt) <= runnerauth.HeartbeatTimeout
	var local *runnerauth.DiagnosticAttempt
	var claim *runnerauth.DiagnosticAttempt
	count := 0
	for i := range d.Records {
		record := &d.Records[i]
		if record.IssueID != item || record.Fence != 0 && record.Fence != uint64(lease.FencingToken) {
			continue
		}
		if record.Fence == uint64(lease.FencingToken) && slices.Contains(record.Membership, "native_claim") {
			claim = record
		}
		if record.LocalAttemptID > 0 && record.Generation > 0 && slices.Contains(record.Membership, "running") && !record.LeaseRenewedAt.Before(started) && (latest == nil || string(latest.LeaseID) != lease.LeaseID || record.NativeAttemptID == "" || record.NativeAttemptID == latest.AttemptID) {
			local = record
			count++
		}
	}
	if count == 1 && local != nil && !terminal && fresh && !d.Truncated {
		execution.ID = runner + "/" + strconv.FormatInt(local.LocalAttemptID, 10) + "/" + strconv.FormatUint(local.Generation, 10)
		if local.NativeAttemptID != "" {
			execution.ID = local.NativeAttemptID
		} else if claim != nil && claim.NativeAttemptID != "" {
			execution.ID = claim.NativeAttemptID
		}
		execution.Running, execution.Stage = true, local.Stage
		execution.ExpiresAt = hubapi.MinTime(execution.ExpiresAt, asOf.Add(runnerauth.HeartbeatTimeout))
	}
	if count != 1 {
		local = nil
	}
	completed := local != nil && !local.ProviderCompletedAt.Before(started) && !local.ProviderCompletedAt.After(now) || claim != nil && !claim.ProviderCompletedAt.Before(started) && !claim.ProviderCompletedAt.After(now)
	if completed {
		execution.Running, execution.Stage = false, ""
		if fresh && (claim != nil && claim.OperationPending && claim.HostOperation == "workspace.finalize_native_work" || local != nil && local.OperationPending && local.HostOperation == "workspace.finalize_native_work") {
			execution.Stage = "finalizing"
		}
	}
}
