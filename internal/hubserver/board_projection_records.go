package hubserver

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type boardProjectionSnapshot struct {
	project       projectstore.Record
	item          corestore.WorkItem
	version       eventstore.CollaborationVersion
	attempt       *tenantAttemptSnapshot
	attemptCount  int64
	lease         *leasestore.Lease
	change        *eventstore.Change
	changeVersion *eventstore.Version
}

func (s *Service) maintainBoardRecords(ctx context.Context) error {
	organization := tracker.OrganizationID(s.appOrganizationID())
	if organization == "" {
		return corestore.ErrTenantMismatch
	}
	for range 32 {
		var candidate viewstore.BoardCandidate
		err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
			var err error
			candidate, err = r.NextBoardCandidate(ctx, tracker.NativeWorkItemID(s.boardRefreshCursor))
			return err
		})
		if errors.Is(err, corestore.ErrNotFound) {
			s.boardRefreshCursor = ""
			return nil
		}
		if err != nil {
			return err
		}
		s.boardRefreshCursor = string(candidate.WorkItemID)
		if err := s.refreshBoardRecord(ctx, organization, candidate, s.config.now()); err != nil && !errors.Is(err, corestore.ErrRevisionConflict) {
			return err
		}
	}
	return nil
}

func (s *Service) boardProjectionSnapshot(ctx context.Context, organization tracker.OrganizationID, candidate viewstore.BoardCandidate, now time.Time) (boardProjectionSnapshot, error) {
	var snapshot boardProjectionSnapshot
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		snapshot.project.Core, err = r.Project(ctx, candidate.ProjectID)
		if err != nil {
			return err
		}
		if snapshot.project.Core.DeletedAt == nil && snapshot.project.Core.Profile == "native" {
			snapshot.project, err = r.ProjectRecord(ctx, candidate.ProjectID)
			if err != nil {
				return err
			}
		}
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
		snapshot.version, err = r.CollaborationVersion(ctx, string(candidate.WorkItemID), snapshot.item.Revision)
		if err != nil {
			return err
		}
		attempt, err := r.LatestAttempt(ctx, snapshot.item.ID)
		if err == nil {
			value := tenantAttemptSnapshot{attempt: attempt}
			value.lease, err = r.Lease(ctx, attempt.LeaseID)
			if err != nil {
				return err
			}
			if boardAttemptCurrent(value, now) {
				value.event, err = r.LatestRunEvent(ctx, candidate.ProjectID, attempt.ID)
				if err != nil {
					return err
				}
			}
			snapshot.attempt = &value
			snapshot.attemptCount, err = r.BoardAttemptCount(ctx, snapshot.item.ID)
			if err != nil {
				return err
			}
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		lease, err := r.ActiveLeaseForIssue(ctx, snapshot.item.ID)
		if err == nil {
			snapshot.lease = &lease
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		change, err := r.LatestChange(ctx, candidate.ProjectID, candidate.WorkItemID)
		if err == nil {
			snapshot.change = &change
			if change.CurrentVersionID != nil {
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

func (s *Service) refreshBoardRecord(ctx context.Context, organization tracker.OrganizationID, candidate viewstore.BoardCandidate, now time.Time) error {
	snapshot, err := s.boardProjectionSnapshot(ctx, organization, candidate, now)
	if err != nil {
		return err
	}
	update := viewstore.BoardUpdate{ProjectID: candidate.ProjectID, WorkItemID: candidate.WorkItemID, ExpectedRevision: snapshot.item.Revision, ExpectedEventSequence: snapshot.item.EventSequence, ExpectedProjectRevision: snapshot.project.Core.ContentSettingsRevision}
	if snapshot.item.ID != 0 && snapshot.project.Core.DeletedAt == nil && snapshot.project.Core.Profile == "native" {
		issue, err := s.hydrateWorkItemRecord(ctx, snapshot.item, snapshot.version)
		if err != nil {
			return err
		}
		card := tracker.NativeBoardCard{Issue: issue}
		var latest *tracker.NativeAttempt
		if historical := snapshot.attempt; historical != nil {
			attempt, err := s.boardAttemptDetails(ctx, *historical, now)
			if err != nil {
				return err
			}
			if attempt != nil {
				latest = attempt
				card.Attempt = &tracker.NativeBoardAttempt{Count: int(snapshot.attemptCount), ID: attempt.AttemptID, Status: attempt.Status, RunnerID: attempt.RunnerID, MachineID: string(attempt.MachineID), Identity: nativeAttemptDisplayIdentity(*attempt), StartedAt: attempt.StartedAt, ExpiresAt: attempt.LeaseExpiresAt}
			} else {
				status := historical.attempt.Status
				if status == "running" {
					status = "interrupted"
				}
				card.Attempt = &tracker.NativeBoardAttempt{Count: int(snapshot.attemptCount), ID: historical.attempt.ID, Status: status, MachineID: historical.lease.MachineID, StartedAt: historical.attempt.StartedAt, ExpiresAt: historical.lease.ExpiresAt}
			}
		}
		if lease := snapshot.lease; lease != nil && lease.ReleasedAt == nil && !now.Before(lease.RenewedAt) && now.Before(lease.ExpiresAt) {
			card.Execution = &tracker.NativeBoardExecution{ID: lease.LeaseID, ExpiresAt: lease.ExpiresAt}
			if card.Attempt != nil && snapshot.attempt.lease.LeaseID == lease.LeaseID {
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
		if snapshot.change != nil {
			card.Change = &tracker.NativeBoardChange{ID: snapshot.change.ID, Title: snapshot.change.Title}
			if v := snapshot.changeVersion; v != nil && v.ExternalProvider != "" {
				card.Change.External = &tracker.ChangeExternalReference{Provider: v.ExternalProvider, ID: v.ExternalID, URL: v.ExternalURL}
			}
		}
		card.StatusLine = nativeBoardStatusLine(card)
		payload, err := viewstore.NewContent(s.runDetailStore()).PrepareBoard(ctx, card)
		if err != nil {
			return err
		}
		record := boardProjectionRecord(snapshot, card)
		record.Payload = payload
		update.Current = &record
	}
	return s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error { return w.ReplaceBoardItem(ctx, update) })
}

func boardAttemptCurrent(snapshot tenantAttemptSnapshot, now time.Time) bool {
	return snapshot.attempt.Status == "running" && snapshot.lease.ReleasedAt == nil && now.Before(snapshot.lease.ExpiresAt)
}

func (s *Service) boardAttemptDetails(ctx context.Context, snapshot tenantAttemptSnapshot, now time.Time) (*tracker.NativeAttempt, error) {
	if !boardAttemptCurrent(snapshot, now) {
		return nil, nil
	}
	if snapshot.event.ID == "" {
		return nil, corestore.ErrNotFound
	}
	attempt, err := s.hydrateNativeAttemptTenant(ctx, snapshot, now)
	if err != nil {
		return nil, err
	}
	return &attempt, nil
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
	if issue.Verification != nil {
		record.VerificationState = issue.Verification.State
	}
	if card.Execution != nil {
		record.Running = !issue.Terminal && card.Execution.Running
		record.ExpiresAt = &card.Execution.ExpiresAt
		record.ExecutionFence = snapshot.lease.FencingToken
	}
	return record
}
