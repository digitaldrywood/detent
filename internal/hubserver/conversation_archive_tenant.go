package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) executeCoordinatorArchiveTenant(ctx context.Context, scope nativeScope, action chat.Action, archive chat.IssueArchive) (json.RawMessage, error) {
	if scope.credential.Scope == apiScopeWorker {
		return nil, nativePublicInvalid("Archive and restore require an operator")
	}
	operation := nativeOperation(scope, "POST", "/work-items/archive")
	command := tracker.MutationForContext(ctx, action.RequestID)
	hash, err := nativeCommandHash(archive)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
			return nil, err
		}
		receipt, found, err := s.workItemRecordReceipt(ctx, scope, operation, command.IdempotencyKey, hash)
		if err != nil {
			return nil, err
		}
		if found {
			return content.Receipt(ctx, receipt)
		}
		records, issues, prepareErr := s.prepareCoordinatorArchiveTenant(ctx, scope, archive)
		if prepareErr != nil {
			replay, found, receiptErr := s.workItemRecordReceipt(ctx, scope, operation, command.IdempotencyKey, hash)
			if receiptErr != nil {
				return nil, receiptErr
			}
			if found {
				return content.Receipt(ctx, replay)
			}
			return nil, prepareErr
		}

		raw, err := json.Marshal(issues)
		if err != nil {
			return nil, err
		}
		now := s.config.now().UTC()
		receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
		if err != nil {
			return nil, err
		}
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
			}
			previous, err := w.Receipt(ctx, scope.credential.ID, operation, command.IdempotencyKey)
			if err == nil {
				if previous.RequestHash != hash {
					return nativeConflict(0)
				}
				return corestore.ErrSerializationFailure
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			currentProject, err := w.ProjectRecord(ctx, scope.project)
			if err != nil {
				return err
			}
			if currentProject.Core.DeletedAt != nil {
				return corestore.ErrNotFound
			}
			ordered := slices.Clone(records)
			slices.SortFunc(ordered, func(a, b coordinatorWorkRecord) int {
				if a.plan.current.ID < b.plan.current.ID {
					return -1
				}
				if a.plan.current.ID > b.plan.current.ID {
					return 1
				}
				return 0
			})
			for _, record := range ordered {
				if !sameWorkItemProjectPlan(record.plan.project, currentProject) {
					return corestore.ErrSerializationFailure
				}
				current, err := w.LockWorkItem(ctx, scope.project, record.plan.current.NativeID)
				if err != nil {
					return err
				}
				if current.Revision != record.plan.current.Revision || current.Archived {
					return corestore.ErrRevisionConflict
				}
				if valueOrEmpty(current.WorkflowStateName) == "Merging" {
					return nativePublicInvalid("A Merging issue cannot be archived")
				}
				active, err := w.WorkItemHasActiveLease(ctx, scope.project, current.NativeID, s.config.now())
				if err != nil {
					return err
				}
				if active {
					return nativePublicInvalid("Finish or stop the running issue before archiving")
				}
			}
			for _, record := range records {
				if err := record.write(ctx, w, s); err != nil {
					return err
				}
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			continue
		}
		if err != nil {
			return nil, projectRecordError(err)
		}
		s.signalNativeDispatch(scope)
		return raw, nil
	}
}

func (s *Service) prepareCoordinatorArchiveTenant(ctx context.Context, scope nativeScope, archive chat.IssueArchive) ([]coordinatorWorkRecord, []tracker.NativeIssue, error) {
	records := make([]coordinatorWorkRecord, 0, len(archive.Items))
	issues := make([]tracker.NativeIssue, 0, len(archive.Items))
	for _, item := range archive.Items {
		request := workItemArchive{archiveIssueRequest: archiveIssueRequest{ExpectedRevision: tracker.Revision(item.Revision)}, Archived: true}
		plan, err := s.loadWorkItemPlan(ctx, scope, item.WorkItemID, "", request)
		if err != nil {
			return nil, nil, err
		}
		if plan.issue.Archived {
			return nil, nil, nativePublicInvalid("Issue is already archived")
		}
		if plan.issue.State == "Merging" {
			return nil, nil, nativePublicInvalid("A Merging issue cannot be archived")
		}
		plan, err = s.planWorkItemRecord(ctx, scope, plan, request)
		if err != nil {
			return nil, nil, err
		}
		record, err := s.prepareCoordinatorWorkRecord(ctx, scope, plan, tracker.CollaborationData{Operation: "archive"})
		if err != nil {
			return nil, nil, err
		}
		records = append(records, record)
		issues = append(issues, s.nativeIssueResponse(plan.issue))
	}
	return records, issues, nil
}
