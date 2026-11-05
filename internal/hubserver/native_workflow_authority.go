package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) authorizeNativeWorkflowScope(ctx context.Context, reader accessstore.Reader, scope nativeScope) error {
	if err := s.authorizeAccessScope(ctx, reader, scope, true); err != nil {
		return err
	}
	if scope.credential.Hosted == nil {
		return nil
	}
	member, err := reader.AuthorizeAccess(ctx, s.accessAuthority(scope, true), s.config.now())
	if err != nil {
		return err
	}
	if member.UserID != "" && lesserHostedRole(member.Role, scope.credential.HostedRole) != scope.credential.HostedRole {
		return auth.ErrHostedIdentity
	}
	return nil
}

func validateNativeWorkflowPolicy(project projectstore.Record, descriptor policy.Descriptor, found bool) error {
	if project.Core.WorkflowSource == "" {
		return nil
	}
	if !found || descriptor.Workflow == nil || descriptor.Validate() != nil {
		return policyMismatch("The configured workflow requires its approved policy")
	}
	workflowRevision := descriptor.Workflow.Revision
	if workflowRevision == "" {
		workflowRevision = descriptor.SourceRevision
	}
	if descriptor.Workflow.Source != project.Core.WorkflowSource || workflowRevision != project.Core.WorkflowSourceRevision || !slices.EqualFunc(nativeTriageStates(descriptor.Workflow.States), project.States, func(a, b policy.State) bool {
		return a.Name == b.Name && a.Terminal == b.Terminal && a.Dispatchable == b.Dispatchable && a.OperatorOnly == b.OperatorOnly && slices.Equal(a.Transitions, b.Transitions)
	}) {
		return policyMismatch("The configured workflow differs from its approved policy")
	}
	return nil
}

func (s *Service) settleHumanWorkflowLease(ctx context.Context, w tenantstore.Writer, scope nativeScope, lease leasestore.Lease, request tracker.Transition, prepared *preparedNativeLeaseCancellation, stopState string) error {
	fence := leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}
	if !lease.ExpiresAt.After(s.config.now().UTC()) {
		_, err := w.Expire(ctx, fence)
		if errors.Is(err, tracker.ErrStaleFencingToken) {
			return tracker.ErrLeaseConflict
		}
		return err
	}
	if request.State != "Cancelled" && !strings.EqualFold(request.State, "Blocked") && request.State != stopState {
		return tracker.ErrLeaseConflict
	}
	leaseAuthority := NativeLeaseAuthority{Scope: scope, Lease: lease, Request: tracker.NativeLeaseMutation{FencingToken: tracker.FencingToken(lease.FencingToken), Reason: "cancelled", CancelAttempt: true}, Operation: "release"}
	if err := (tenantNativeRunEventParticipant{service: s}).validateNativeLeaseMutation(ctx, w, &leaseAuthority, true); err != nil {
		return err
	}
	if prepared == nil {
		return corestore.ErrSerializationFailure
	}
	if prepared.attempt.ID == "" {
		current, err := w.LatestAttempt(ctx, lease.IssueID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && current.LeaseID == lease.LeaseID && current.FencingToken == lease.FencingToken && current.Status == "running" {
			return corestore.ErrSerializationFailure
		}
	}
	if prepared.attempt.ID != "" {
		attempt, err := w.LatestAttempt(ctx, lease.IssueID)
		if err != nil {
			return err
		}
		if attempt.ID != prepared.attempt.ID || attempt.LeaseID != lease.LeaseID || attempt.FencingToken != lease.FencingToken || attempt.Sequence != prepared.attempt.Sequence || attempt.Status != "running" {
			return leasestore.ErrAttemptConflict
		}
		item, err := w.WorkItem(ctx, scope.project, prepared.workItem.NativeID)
		if err != nil {
			return err
		}
		authority := NativeRunEventAuthority{Scope: scope, OrganizationID: scope.organization, ProjectID: scope.project, WorkItem: item, Lease: lease, Event: prepared.event}
		if err := (tenantNativeRunEventParticipant{service: s}).validateNativeRunEvent(ctx, w, &authority, true); err != nil {
			return err
		}
		event := leasestore.AttemptEvent{ID: attempt.ID, Fence: fence, RunID: attempt.RunID, Sequence: attempt.Sequence + 1, Status: "cancelled", DispatchGeneration: authority.DispatchGeneration, PayloadObjectKey: prepared.payloadObjectKey}
		stored, duplicate, err := w.RecordAttemptEvent(ctx, event)
		if err != nil {
			return err
		}
		if duplicate || stored.Status != "cancelled" || stored.Sequence != event.Sequence {
			return leasestore.ErrAttemptConflict
		}
		if _, err := w.AppendEvent(ctx, prepared.history); err != nil {
			return err
		}
		if err := s.config.NativeRunEvents.ApplyNativeRunEvent(ctx, w, authority); err != nil {
			return err
		}
	}
	if _, err := w.Release(ctx, fence, "cancelled"); err != nil {
		return err
	}
	if err := s.config.NativeRunEvents.ApplyNativeLeaseMutation(ctx, w, leaseAuthority); err != nil {
		return err
	}
	if s.conversations != nil {
		return s.conversations.leaseReleasedTenant(ctx, w, lease.LeaseID, lease.FencingToken, "cancelled")
	}
	return nil
}

func (s *Service) prepareHumanWorkflowCancellation(ctx context.Context, scope nativeScope, issueID int64, request tracker.Transition, stopState string) (*preparedNativeLeaseCancellation, error) {
	if request.State != "Cancelled" && !strings.EqualFold(request.State, "Blocked") && request.State != stopState {
		return nil, nil
	}
	if s.config.NativeRunEvents == nil {
		return nil, nativeRunEventStorageUnavailable()
	}
	var lease leasestore.Lease
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeNativeWorkflowScope(ctx, r, scope); err != nil {
			return err
		}
		var err error
		lease, err = r.ActiveLeaseForIssue(ctx, issueID)
		if err != nil {
			return err
		}
		return nil
	})
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.prepareLeaseCancellationTenant(ctx, scope, lease.LeaseID, tracker.NativeLeaseMutation{FencingToken: tracker.FencingToken(lease.FencingToken), Reason: "cancelled", CancelAttempt: true}, true)
}
