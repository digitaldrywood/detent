package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type coordinatorSplitPlan struct {
	parent      workItemRecordPlan
	records     []coordinatorWorkRecord
	result      coordinatorSplitResult
	policy      policystore.Revision
	policyFound bool
}

func (s *Service) executeCoordinatorSplitTenant(ctx context.Context, scope nativeScope, action chat.Action, split chat.IssueSplit) (json.RawMessage, error) {
	if scope.credential.Scope == apiScopeWorker {
		return nil, nativePublicInvalid("Splitting issues requires an operator")
	}
	if err := validateCoordinatorIssueSplit(split); err != nil {
		return nil, err
	}
	operation := nativeOperation(scope, "POST", "/work-items/"+split.ParentID+"/split")
	command := tracker.MutationForContext(ctx, action.RequestID)
	hash, err := nativeCommandHash(split)
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
		plan, prepareErr := s.prepareCoordinatorSplitTenant(ctx, scope, split)
		if prepareErr != nil {
			replay, found, err := s.workItemRecordReceipt(ctx, scope, operation, command.IdempotencyKey, hash)
			if err != nil {
				return nil, err
			}
			if found {
				return content.Receipt(ctx, replay)
			}
			if errors.Is(prepareErr, corestore.ErrSerializationFailure) {
				continue
			}
			return nil, prepareErr
		}
		raw, err := json.Marshal(plan.result)
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
			if len(split.Edges) > 0 {
				if err := w.LockRelationshipGraph(ctx); err != nil {
					return err
				}
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
			if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", now); err != nil {
				return err
			}
			project, err := w.ProjectRecord(ctx, scope.project)
			if err != nil {
				return err
			}
			if project.Core.DeletedAt != nil {
				return corestore.ErrNotFound
			}
			if !sameWorkItemProjectPlan(plan.parent.project, project) {
				return corestore.ErrSerializationFailure
			}
			policy, err := w.ProjectPolicy(ctx, scope.project)
			if errors.Is(err, corestore.ErrNotFound) {
				if plan.policyFound {
					return corestore.ErrSerializationFailure
				}
			} else if err != nil {
				return err
			} else if !plan.policyFound || !sameNativeClaimProjectPolicy(plan.policy, policy) {
				return corestore.ErrSerializationFailure
			}
			parent, err := w.LockWorkItem(ctx, scope.project, plan.parent.current.NativeID)
			if err != nil {
				return err
			}
			if parent.Revision != plan.parent.current.Revision {
				return nativeConflict(tracker.Revision(parent.Revision))
			}
			if parent.Archived {
				return nativePublicInvalid("Archived issues cannot be edited")
			}
			if parent.EventSequence != plan.parent.current.EventSequence {
				return corestore.ErrSerializationFailure
			}
			for _, record := range plan.records {
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

func (s *Service) prepareCoordinatorSplitTenant(ctx context.Context, scope nativeScope, split chat.IssueSplit) (coordinatorSplitPlan, error) {
	var prepared coordinatorSplitPlan
	parent, err := s.loadWorkItemPlan(ctx, scope, split.ParentID, "", tracker.UpdateIssue{})
	if err != nil {
		return prepared, err
	}
	if err := requireNativeEdit(parent.issue, tracker.Revision(split.Revision)); err != nil {
		return prepared, err
	}
	prepared.parent = parent
	nodes := []workItemRecordPlan{parent}
	for index, child := range split.Children {
		request := tracker.CreateIssue{Title: child.Title, Body: child.Description, Priority: child.Priority, State: child.State}
		plan, err := s.loadWorkItemPlan(ctx, scope, "", "", request)
		if err != nil {
			return prepared, err
		}
		if !sameWorkItemProjectPlan(parent.project, plan.project) {
			return prepared, corestore.ErrSerializationFailure
		}
		if index > 0 {
			plan.issue.Number = nodes[1].issue.Number + index
		}
		plan, err = s.planWorkItemRecord(ctx, scope, plan, request)
		if err != nil {
			return prepared, err
		}
		policy, found, err := s.validateWorkItemRecordFilingTenant(ctx, scope, plan)
		if err != nil {
			return prepared, err
		}
		if index == 0 {
			prepared.policy, prepared.policyFound = policy, found
		} else if found != prepared.policyFound || found && !sameNativeClaimProjectPolicy(prepared.policy, policy) {
			return prepared, corestore.ErrSerializationFailure
		}
		record, err := s.prepareCoordinatorWorkRecord(ctx, scope, plan, tracker.CollaborationData{})
		if err != nil {
			return prepared, err
		}
		prepared.records = append(prepared.records, record)
		plan.create = false
		plan.current = corestore.WorkItem{OrganizationID: scope.organization, ProjectID: scope.project, NativeID: plan.issue.WorkItemID, Number: int64(plan.issue.Number), Revision: int64(plan.issue.Revision), EventSequence: 1, WorkflowStateName: new(plan.issue.State)}
		nodes = append(nodes, plan)
	}
	for _, edge := range split.Edges {
		dependent, blocker := nodes[edge.Dependent], nodes[edge.Blocker]
		dependent.related = blocker.current
		dependent.relationship = &corestore.Relationship{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: dependent.issue.WorkItemID, RelatedProjectID: scope.project, RelatedWorkItemID: blocker.issue.WorkItemID, Type: "dependency"}
		dependent.relationshipType, dependent.relationshipOperate = "dependency", "add"
		request := tracker.DependencyMutation{ExpectedRevision: dependent.issue.Revision, RelatedWorkItemID: blocker.issue.WorkItemID, Operation: "add"}
		dependent, err = s.planWorkItemRecord(ctx, scope, dependent, request)
		if err != nil {
			return prepared, err
		}
		record, err := s.prepareCoordinatorWorkRecord(ctx, scope, dependent, tracker.CollaborationData{RelatedWorkItemID: blocker.issue.WorkItemID, Operation: "add"})
		if err != nil {
			return prepared, err
		}
		prepared.records = append(prepared.records, record)
		dependent.current.Revision++
		dependent.current.EventSequence++
		dependent.relationship = nil
		nodes[edge.Dependent] = dependent
	}
	var body strings.Builder
	body.WriteString("Approved issue split:\n")
	prepared.result.Children = make([]tracker.NativeIssue, 0, len(split.Children))
	for _, child := range nodes[1:] {
		fmt.Fprintf(&body, "\n- #%d (%s): %s", child.issue.Number, child.issue.WorkItemID, child.issue.Title)
		prepared.result.Children = append(prepared.result.Children, s.nativeIssueResponse(child.issue))
	}
	if len(split.Edges) > 0 {
		body.WriteString("\n\nDependencies:\n")
		for _, edge := range split.Edges {
			fmt.Fprintf(&body, "\n- #%d is blocked by #%d", nodes[edge.Dependent].issue.Number, nodes[edge.Blocker].issue.Number)
		}
	}
	comment, err := s.planWorkItemRecord(ctx, scope, nodes[0], tracker.CreateComment{Body: body.String()})
	if err != nil {
		return prepared, err
	}
	comment.relationship = nil
	record, err := s.prepareCoordinatorWorkRecord(ctx, scope, comment, tracker.CollaborationData{})
	if err != nil {
		return prepared, err
	}
	prepared.records = append(prepared.records, record)
	prepared.result.Comment = comment.comment
	return prepared, nil
}
