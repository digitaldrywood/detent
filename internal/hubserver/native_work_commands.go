package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) createNativeIssueCommand(ctx context.Context, scope nativeScope, request tracker.CreateIssue) (json.RawMessage, error) {
	if request.ScheduledValidation != nil {
		return s.recordScheduledValidation(ctx, scope, request)
	}
	if request.ScheduledFailure != nil {
		return s.attributeScheduledFailure(ctx, scope, request)
	}
	if origin, ok := issueorigin.Parse(request.Body); ok && origin.Kind == "doctor" && origin.Instance == "github-actions" {
		scope.scheduledRepair = s.observeScheduledRepair(ctx, scope, request)
		if scope.scheduledRepair == nil {
			return nil, hubapi.NativePublicInvalid("Scheduled report is not authorized by the approved workflow")
		}
	}
	scope.action = operatortool.FileIssue
	operation := nativeIssueCreationOperation(scope)
	if request.ChallengeToken != "" {
		operation += "/duplicate-confirmation"
	}
	result, err := s.executeWorkItemRecordMutation(ctx, scope, hubapi.NativeCommandOptions{OperationID: operation, Feature: "collaboration"}, request.Mutation, request, "")
	if err == nil && s.wakeSpriteRunnersAfter != nil {
		s.wakeSpriteRunnersAfter(scope, result)
	}
	return result, err
}

func nativeIssueCreationOperation(scope nativeScope) string {
	return "POST /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items"
}

func normalizeNativeMachineIntake(scope nativeScope, states []tracker.NativeState, request tracker.CreateIssue) (tracker.CreateIssue, error) {
	origin, _ := issueorigin.Parse(request.Body)
	canonical := issueorigin.DefectFingerprint(request.Body)
	if canonical != "" {
		if origin.Kind == "worker" {
			origin.Fingerprint = canonical
			request.Body = issueorigin.Stamp(request.Body, origin)
		}
		if string(scope.project) == issueorigin.DetentCloudProjectID {
			backlog, todo := false, false
			for _, state := range states {
				backlog = backlog || state.Name == "Backlog" && !state.Dispatchable && !state.Terminal && !state.OperatorOnly
				todo = todo || state.Name == "Todo" && state.Dispatchable && !state.Terminal && !state.OperatorOnly
			}
			if !backlog || !todo {
				return request, hubapi.NativePublicInvalid("Defect reporting requires nondispatchable Backlog and dispatchable Todo")
			}
			request.State = "Todo"
			if request.Priority == nil || *request.Priority > 1 {
				request.Priority = new(1)
			}
		}
	}
	return request, nil
}

func (s *Service) updateNativeIssueCommand(ctx context.Context, scope nativeScope, item string, request tracker.UpdateIssue) (json.RawMessage, error) {
	scope.action, scope.actionItem = operatortool.EditItem, item
	options := hubapi.NativeCommandOptions{OperationID: "PATCH /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items" + "/" + item, Item: item, RequireLease: true, Feature: "collaboration"}
	if s.config.TenantRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	return s.executeWorkItemRecordMutation(ctx, scope, options, request.Mutation, request, "")
}

func (s *Service) changeNativeDependencyCommand(ctx context.Context, scope nativeScope, item string, request tracker.DependencyMutation) (json.RawMessage, error) {
	options := hubapi.NativeCommandOptions{OperationID: "POST /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items" + "/" + item + "/dependencies", Item: item, RequireLease: true, Feature: "collaboration"}
	scope.action = operatortool.SetDependency
	if s.config.TenantRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	return s.executeWorkItemRecordMutation(ctx, scope, options, request.Mutation, request, "")
}

func (s *Service) createNativeCommentCommand(ctx context.Context, scope nativeScope, item string, request tracker.CreateComment) (json.RawMessage, error) {
	scope.action, scope.actionItem = operatortool.AddComment, item
	options := hubapi.NativeCommandOptions{OperationID: "POST /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items" + "/" + item + "/comments", Item: item, RequireLease: true, Feature: "collaboration"}
	if s.config.TenantRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	return s.executeWorkItemRecordMutation(ctx, scope, options, request.Mutation, request, "")
}

func (s *Service) updateNativeCommentCommand(ctx context.Context, scope nativeScope, item string, comment string, request tracker.UpdateComment) (json.RawMessage, error) {
	scope.action = operatortool.EditComment
	scope.actionItem, scope.actionComment = item, comment
	options := hubapi.NativeCommandOptions{OperationID: "PATCH /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items" + "/" + item + "/comments/" + comment, Item: item, RequireLease: true, Feature: "collaboration"}
	if s.config.TenantRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	return s.executeWorkItemRecordMutation(ctx, scope, options, request.Mutation, request, comment)
}

func (s *Service) setNativeArchiveCommand(ctx context.Context, scope nativeScope, item string, request archiveIssueRequest, archived bool) (json.RawMessage, error) {
	operation := "restore"
	if archived {
		operation = "archive"
	}
	options := hubapi.NativeCommandOptions{OperationID: "POST /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items/" + item + "/" + operation, Item: item, RequireLease: true, Feature: "collaboration", Completion: archived}
	if s.config.TenantRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	return s.executeWorkItemRecordMutation(ctx, scope, options, request.Mutation, workItemArchive{archiveIssueRequest: request, Archived: archived}, "")
}

func (s *Service) nativeWorkObservation(ctx context.Context, scope nativeScope, item string) (tracker.NativeIssue, error) {
	return s.readWorkItem(ctx, scope, item)
}

func (s *Service) nativeWorkComments(ctx context.Context, scope nativeScope, item string, limit int, cursor string) (tracker.Page[tracker.NativeComment], error) {
	params := url.Values{"limit": {strconv.Itoa(limit)}, "cursor": {cursor}}
	return s.readComments(ctx, scope, item, params)
}

func (s *Service) operatorCreateNativeWork(ctx context.Context, scope nativeScope, request tracker.CreateIssue) (json.RawMessage, error) {
	project, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return nil, err
	}
	states := project.States
	if request.State == "" && request.GitHubIssueURL == "" {
		request.State = defaultNativeIssueState(states)
	}
	return s.createNativeIssueCommand(ctx, scope, request)
}

func (s *Service) operatorNativeWork(ctx context.Context, scope nativeScope, name string, request operatortool.WorkArguments) (json.RawMessage, error) {
	command := tracker.MutationForContext(ctx, "")
	if request.Target == "pr" {
		return nil, hubapi.NativePublicInvalid("application service is unavailable")
	}
	current, err := s.nativeWorkObservation(ctx, scope, request.Identifier)
	if err != nil {
		return nil, err
	}
	if current.Profile == "github_compatible" {
		return s.operatorCompatibilityWork(ctx, scope, name, request, command)
	}
	switch name {
	case operatortool.TransferItem:
		return s.transferNativeSourceCommand(ctx, scope, request.Identifier, sourceTransferRequest{Mutation: command, ExpectedRevision: tracker.Revision(request.ExpectedRevision), VersionID: request.VersionID, DestinationRunnerID: request.DestinationRunnerID})
	case operatortool.EditItem:
		return s.updateNativeIssueCommand(ctx, scope, request.Identifier, tracker.UpdateIssue{Mutation: command, ExpectedRevision: tracker.Revision(request.ExpectedRevision), Type: request.Type, Title: request.Title, Body: request.Body, Labels: request.Labels, Priority: tracker.SetPriority(request.Priority)})
	case operatortool.AddComment:
		return s.createNativeCommentCommand(ctx, scope, request.Identifier, tracker.CreateComment{Mutation: command, Body: *request.Body})
	case operatortool.VerifyFix:
		return s.verifyNativeIssueCommand(ctx, scope, request.Identifier, tracker.VerifyFix{Mutation: command, ExpectedRevision: tracker.Revision(request.ExpectedRevision), Result: request.Result, Comment: request.Comment})
	case operatortool.EditComment:
		if request.ExpectedRevision <= 0 {
			return nil, hubapi.NativePublicInvalid("expected revision is required")
		}
		return s.updateNativeCommentCommand(ctx, scope, request.Identifier, request.CommentID, tracker.UpdateComment{Mutation: command, ExpectedRevision: tracker.Revision(request.ExpectedRevision), Body: *request.Body})
	case operatortool.SetDependency:
		if request.ExpectedRevision <= 0 {
			return nil, hubapi.NativePublicInvalid("expected revision is required")
		}
		return s.changeNativeDependencyCommand(ctx, scope, request.Identifier, tracker.DependencyMutation{Mutation: command, ExpectedRevision: tracker.Revision(request.ExpectedRevision), RelatedWorkItemID: tracker.NativeWorkItemID(request.Related), RelationshipType: request.RelationshipType, Operation: request.Operation})
	case operatortool.RestoreItem:
		return s.setNativeArchiveCommand(ctx, scope, request.Identifier, archiveIssueRequest{Mutation: command, ExpectedRevision: tracker.Revision(request.ExpectedRevision)}, false)
	case operatortool.OrderItem, operatortool.SetQueuePriority:
		return nil, &hubapi.NativeError{Code: "invalid_request", Message: "Queue ordering and priority overrides require a GitHub-compatible project; native projects use issue priority", Status: http.StatusUnprocessableEntity, PublicMessage: true}
	default:
		return nil, hubapi.NativePublicInvalid("application service is unavailable")
	}
}

func (s *Service) operatorCompatibilityWork(ctx context.Context, scope nativeScope, name string, request operatortool.WorkArguments, command tracker.Mutation) (json.RawMessage, error) {
	if s.config.TenantRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	record, _, err := s.readWorkItemRecord(ctx, scope, request.Identifier)
	if err != nil {
		return nil, err
	}
	switch name {
	case operatortool.OrderItem, operatortool.SetQueuePriority:
		return s.mutateQueueRecord(ctx, scope, name, request, command)
	case operatortool.SetDependency:
		return s.mutateCompatibilityDependency(ctx, scope, record, request, command)
	default:
		return nil, hubapi.NativePublicInvalid("application service is unavailable")
	}
}
