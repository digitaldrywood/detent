package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) createNativeIssueCommand(ctx context.Context, scope nativeScope, request tracker.CreateIssue) (json.RawMessage, error) {
	scope.action = operatortool.FileIssue
	operation := "POST /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items"
	if request.ChallengeToken != "" {
		operation += "/duplicate-confirmation"
	}
	return s.executeWorkItemRecordMutation(ctx, scope, nativeCommandOptions{OperationID: operation, Feature: "collaboration"}, request.Mutation, request, "")
}

func createNativeMachineIntakeTx(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, now time.Time, terminalRepair bool) (tracker.NativeIssue, error) {
	project, err := validateNativeIssueDraft(ctx, tx, scope, request)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	request, err = normalizeNativeMachineIntake(scope, project.States, request)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	return reportNativeMachineOccurrenceTx(ctx, tx, scope, request, now, terminalRepair)
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
				return request, nativePublicInvalid("Defect reporting requires nondispatchable Backlog and dispatchable Todo")
			}
			request.State = "Todo"
			if request.Priority == nil || *request.Priority > 1 {
				request.Priority = new(1)
			}
		}
	}
	return request, nil
}

func reportNativeMachineIssueTx(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, now time.Time) (tracker.NativeIssue, error) {
	if _, err := validateNativeIssueDraft(ctx, tx, scope, request); err != nil {
		return tracker.NativeIssue{}, err
	}
	return reportNativeMachineOccurrenceTx(ctx, tx, scope, request, now, false)
}

func reportNativeMachineOccurrenceTx(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, now time.Time, terminalRepair bool) (tracker.NativeIssue, error) {
	if scope.credential.Scope == apiScopeWorker {
		if _, err := validateRunnerLeaseTx(ctx, tx, scope, request.LeaseID, request.FencingToken, now); err != nil {
			return tracker.NativeIssue{}, err
		}
	}
	return recordNativeMachineOccurrenceTx(ctx, tx, scope, request, now, terminalRepair)
}

func recordNativeMachineOccurrenceTx(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, now time.Time, terminalRepair bool) (tracker.NativeIssue, error) {
	matches, err := resolveNativeDuplicatesTx(ctx, tx, scope, request, now, true)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	var existing string
	if len(matches) != 0 {
		existing = string(matches[0].WorkItemID)
	}
	if existing != "" {
		issue, _, err := readNativeIssue(ctx, tx, scope, existing)
		if err != nil {
			return tracker.NativeIssue{}, err
		}
		if issue.Terminal {
			if terminalRepair && issue.State != "Cancelled" && !strings.Contains(strings.ToLower(issue.Body), "detent:no-dispatch") {
				return createNativeIssue(ctx, tx, scope, request, now, true)
			}
			issue.PublicationReused = true
			return issue, nil
		}
		occurrence := issueorigin.Occurrence(request.Body)
		if len(occurrence) > 64<<10 {
			return tracker.NativeIssue{}, nativePublicInvalid("Comment body must contain 1 byte to 64 KiB")
		}
		if request.Priority != nil && (issue.Priority == nil || *request.Priority < *issue.Priority) {
			if err := requireNativeEdit(issue, issue.Revision); err != nil {
				return tracker.NativeIssue{}, err
			}
			issue.Priority = request.Priority
			issue, err = persistNativeIssue(ctx, tx, scope, issue, "issue.edited", tracker.CollaborationData{Fields: []string{"priority"}}, now)
			if err != nil {
				return tracker.NativeIssue{}, err
			}
		}
		if _, err := insertNativeComment(ctx, tx, scope, issue, occurrence, nil, now); err != nil {
			return tracker.NativeIssue{}, err
		}
		issue.PublicationReused = true
		return issue, nil
	}
	return createNativeIssue(ctx, tx, scope, request, now, true)
}

func (s *Service) updateNativeIssueCommand(ctx context.Context, scope nativeScope, item string, request tracker.UpdateIssue) (json.RawMessage, error) {
	scope.action, scope.actionItem = operatortool.EditItem, item
	options := nativeCommandOptions{OperationID: "PATCH /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items" + "/" + item, Item: item, RequireLease: true, Feature: "collaboration"}
	if s.config.TenantRepository != nil {
		return s.executeWorkItemRecordMutation(ctx, scope, options, request.Mutation, request, "")
	}
	return s.executeNativeIssueMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		issue, _, err := readNativeIssue(ctx, tx, scope, item)
		if err != nil {
			return nil, err
		}
		if err := requireReporterTriage(scope, issue); err != nil {
			return nil, err
		}
		if scope.credential.HostedRole == "reporter" && (request.Type != nil || request.Priority.Present() || request.Labels != nil || request.Assignees != nil) {
			return nil, operatortool.ErrAccessDenied
		}
		if err := requireNativeEdit(issue, request.ExpectedRevision); err != nil {
			return nil, err
		}
		fields := []string{}
		if request.Type != nil {
			if err := validateNativeIssueType(*request.Type, true); err != nil {
				return nil, err
			}
			issue.Type = *request.Type
			fields = append(fields, "type")
		}
		if request.Title != nil {
			issue.Title = *request.Title
			fields = append(fields, "title")
		}
		if request.Body != nil {
			bodyChanged := issue.Body != *request.Body
			issue.Body = *request.Body
			if bodyChanged && issue.IssueContract == nil {
				issue.IssueContract = &issuecontract.State{}
			}
			contractChanged := false
			if scope.actor().Kind == "human" && scope.credential.HostedRole != "reporter" {
				confirmed := config.IssueContractSectionDigests(issue.Body)
				contract := issue.IssueContract
				if contract == nil {
					contract = &issuecontract.State{}
				}
				if bodyChanged || !maps.Equal(contract.ConfirmedSections, confirmed) {
					contract.ReleaseDelivery = nil
					contract.ConfirmedSections = confirmed
					contract.RecordedAt = payloadTimestamp(ctx, now)
					issue.IssueContract = contract
					contractChanged = true
				}
			}
			if (bodyChanged || contractChanged) && issue.IssueContract.ReturnState != "" {
				contract, err := nativeIssueContract(ctx, tx, scope)
				if err != nil {
					return nil, err
				}
				evaluation := contract.Evaluate(nativeContractIssue(issue))
				if evaluation.Satisfied() {
					issue.IssueContract.HumanAction = ""
				} else {
					issue.IssueContract.HumanAction = contract.HumanAction(nativeContractIssue(issue), evaluation)
				}
			}
			fields = append(fields, "body")
		}
		if request.Labels != nil {
			if err := requireUnreservedLabels(ctx, *request.Labels); err != nil {
				return nil, err
			}
			issue.Labels = *request.Labels
			fields = append(fields, "labels")
		}
		if request.Assignees != nil {
			issue.Assignees = *request.Assignees
			fields = append(fields, "assignees")
		}
		// A present patch either sets the level or clears it; an absent one
		// leaves whatever the issue has (tracker.PriorityPatch).
		if request.Priority.Present() {
			issue.Priority = request.Priority.Level()
			fields = append(fields, "priority")
		}
		if len(fields) == 0 {
			return nil, nativePublicInvalid("At least one field must be supplied")
		}
		if err := validateNativeContent(issue.Title, issue.Body, issue.Labels, issue.Assignees, issue.Priority); err != nil {
			return nil, err
		}
		return persistNativeIssue(ctx, tx, scope, issue, "issue.edited", tracker.CollaborationData{Fields: fields}, now)
	})
}

func (s *Service) changeNativeDependencyCommand(ctx context.Context, scope nativeScope, item string, request tracker.DependencyMutation) (json.RawMessage, error) {
	options := nativeCommandOptions{OperationID: "POST /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items" + "/" + item + "/dependencies", Item: item, RequireLease: true, Feature: "collaboration"}
	if s.config.TenantRepository != nil {
		scope.action = operatortool.SetDependency
		return s.executeWorkItemRecordMutation(ctx, scope, options, request.Mutation, request, "")
	}
	return s.executeNativeIssueMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		return changeNativeDependencyTx(ctx, tx, scope, item, request, now)
	})
}

func changeNativeDependencyTx(ctx context.Context, tx *sql.Tx, scope nativeScope, item string, request tracker.DependencyMutation, now time.Time) (tracker.NativeIssue, error) {
	issue, dependentID, err := readNativeIssue(ctx, tx, scope, item)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	if err := requireNativeEdit(issue, request.ExpectedRevision); err != nil {
		return tracker.NativeIssue{}, err
	}
	if request.RelationshipType != "" && request.RelationshipType != "dependency" && request.RelationshipType != "related" {
		return tracker.NativeIssue{}, nativePublicInvalid("Relationship type must be dependency or related")
	}
	if request.Operation != "add" && request.Operation != "remove" {
		return tracker.NativeIssue{}, nativePublicInvalid("Dependency operation must be add or remove")
	}
	condition, grantArgs := scope.credential.projectGrantSQL("i.organization_id", "i.project_id")
	args := append([]any{scope.organization, request.RelatedWorkItemID}, grantArgs...)
	var blockerID tracker.WorkItemID
	var blockerProject tracker.ProjectID
	err = tx.QueryRowContext(ctx, `SELECT i.id, i.project_id FROM issues i WHERE i.organization_id = ? AND i.native_id = ?
AND (`+condition+`)`, args...).Scan(&blockerID, &blockerProject)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	if request.RelationshipType == "related" {
		if blockerID == dependentID {
			return tracker.NativeIssue{}, nativePublicInvalid("An issue cannot be related to itself")
		}
		if err := changeRelatedIssuesTx(ctx, tx, blockerID, dependentID, request.Operation, now); err != nil {
			return tracker.NativeIssue{}, err
		}
		return persistNativeIssue(ctx, tx, scope, issue, "issue.edited", tracker.CollaborationData{RelatedWorkItemID: request.RelatedWorkItemID, Operation: request.Operation, Fields: []string{"related"}}, now)
	}
	if blockerID == dependentID {
		return tracker.NativeIssue{}, nativePublicInvalid("Dependencies cannot form a cycle")
	}
	if request.Operation == "add" {
		var cycle int
		err := tx.QueryRowContext(ctx, `WITH RECURSIVE reachable(id) AS (SELECT dependent_issue_id FROM issue_dependencies WHERE blocker_issue_id = ? UNION SELECT d.dependent_issue_id FROM issue_dependencies d JOIN reachable r ON d.blocker_issue_id = r.id) SELECT count(*) FROM reachable WHERE id = ?`, dependentID, blockerID).Scan(&cycle)
		if err != nil {
			return tracker.NativeIssue{}, err
		}
		if cycle != 0 {
			return tracker.NativeIssue{}, nativePublicInvalid("Dependencies cannot form a cycle")
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO issue_dependencies (blocker_issue_id, dependent_issue_id, provenance, created_at, updated_at) VALUES (?, ?, 'native', ?, ?) ON CONFLICT DO NOTHING", blockerID, dependentID, formatHubTime(now), formatHubTime(now))
		if err != nil {
			return tracker.NativeIssue{}, err
		}
		if !slices.Contains(issue.Dependencies, request.RelatedWorkItemID) {
			issue.Dependencies = append(issue.Dependencies, request.RelatedWorkItemID)
			slices.Sort(issue.Dependencies)
		}
	} else {
		if _, err := tx.ExecContext(ctx, "DELETE FROM issue_dependencies WHERE blocker_issue_id = ? AND dependent_issue_id = ?", blockerID, dependentID); err != nil {
			return tracker.NativeIssue{}, err
		}
		issue.Dependencies = slices.DeleteFunc(issue.Dependencies, func(id tracker.NativeWorkItemID) bool { return id == request.RelatedWorkItemID })
	}
	return persistNativeIssue(ctx, tx, scope, issue, "dependency.changed", tracker.CollaborationData{RelatedWorkItemID: request.RelatedWorkItemID, Operation: request.Operation}, now)
}

func (s *Service) createNativeCommentCommand(ctx context.Context, scope nativeScope, item string, request tracker.CreateComment) (json.RawMessage, error) {
	scope.action, scope.actionItem = operatortool.AddComment, item
	options := nativeCommandOptions{OperationID: "POST /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items" + "/" + item + "/comments", Item: item, RequireLease: true, Feature: "collaboration"}
	if s.config.TenantRepository != nil {
		return s.executeWorkItemRecordMutation(ctx, scope, options, request.Mutation, request, "")
	}
	return s.executeNativeMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		issue, _, err := readNativeIssue(ctx, tx, scope, item)
		if err != nil {
			return nil, err
		}
		if issue.Profile != "native" {
			return nil, nativePublicInvalid("Compatibility project discussion is externally owned")
		}
		if strings.TrimSpace(request.Body) == "" || len(request.Body) > 64<<10 {
			return nil, nativePublicInvalid("Comment body must contain 1 byte to 64 KiB")
		}
		if err := validateNativeProvenance(scope, request.Provenance); err != nil {
			return nil, err
		}
		if request.Provenance != nil {
			var existing string
			err := tx.QueryRowContext(ctx, "SELECT id FROM native_comments WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND source_key = ?", scope.organization, scope.project, issue.WorkItemID, request.Provenance.Provider+":"+request.Provenance.ExternalID).Scan(&existing)
			if err == nil {
				return readNativeComment(ctx, tx, scope, string(issue.WorkItemID), existing)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		return insertNativeComment(ctx, tx, scope, issue, request.Body, request.Provenance, now)
	})
}

func (s *Service) updateNativeCommentCommand(ctx context.Context, scope nativeScope, item string, comment string, request tracker.UpdateComment) (json.RawMessage, error) {
	scope.action = operatortool.EditComment
	scope.actionItem, scope.actionComment = item, comment
	options := nativeCommandOptions{OperationID: "PATCH /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items" + "/" + item + "/comments/" + comment, Item: item, RequireLease: true, Feature: "collaboration"}
	if s.config.TenantRepository != nil {
		return s.executeWorkItemRecordMutation(ctx, scope, options, request.Mutation, request, comment)
	}
	return s.executeNativeMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		comment, err := readNativeComment(ctx, tx, scope, item, comment)
		if err != nil {
			return nil, err
		}
		if comment.Actor.PrincipalID != scope.credential.ID && scope.credential.Scope == apiScopeWorker {
			return nil, nativeNotFound()
		}
		if scope.credential.Hosted != nil && comment.Actor.PrincipalID != scope.actor().PrincipalID {
			return nil, operatortool.ErrAccessDenied
		}
		if request.ExpectedRevision <= 0 {
			return nil, nativePublicInvalid("Expected revision must be positive")
		}
		if comment.Revision != request.ExpectedRevision {
			return nil, nativeConflict(comment.Revision)
		}
		if comment.Deleted {
			return nil, nativePublicInvalid("Deleted comments cannot be changed")
		}
		if !request.Deleted && (strings.TrimSpace(request.Body) == "" || len(request.Body) > 64<<10) {
			return nil, nativePublicInvalid("Comment body must contain 1 byte to 64 KiB")
		}
		if comment.Body == request.Body && comment.Deleted == request.Deleted {
			return comment, nil
		}
		if !request.Deleted {
			if err := validateCloudVisualReferences(ctx, tx, scope, cloudContentField{"body", request.Body}); err != nil {
				return nil, err
			}
		}
		comment.Body = request.Body
		comment.Deleted = request.Deleted
		if comment.Deleted {
			comment.Body = ""
		}
		comment.Revision++
		comment.UpdatedAt = payloadTimestamp(ctx, now)
		editor := scope.actor()
		comment.EditedBy = &editor
		editedBy, err := marshalNative(editor)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, "UPDATE native_comments SET body = ?, revision = ?, edited_by_json = ?,change_edited_by_kind = ?, updated_at = ?, deleted = ? WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND id = ?", comment.Body, comment.Revision, editedBy, scope.actor().Kind, formatHubTime(comment.UpdatedAt), comment.Deleted, scope.organization, scope.project, comment.WorkItemID, comment.ID)
		if err != nil {
			return nil, err
		}
		if err := bindCloudAttachmentReferences(ctx, tx, scope, string(comment.WorkItemID), comment.ID, comment.Body); err != nil {
			return nil, err
		}
		if err := recordNativeChange(ctx, tx, scope, comment, string(comment.WorkItemID), comment.Revision, "comment.edited", tracker.CollaborationData{CommentID: comment.ID, Revision: comment.Revision}, now); err != nil {
			return nil, err
		}
		return comment, nil
	})
}

func (s *Service) setNativeArchiveCommand(ctx context.Context, scope nativeScope, item string, request archiveIssueRequest, archived bool) (json.RawMessage, error) {
	operation := "restore"
	if archived {
		operation = "archive"
	}
	options := nativeCommandOptions{OperationID: "POST /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items/" + item + "/" + operation, Item: item, RequireLease: true, Feature: "collaboration", Completion: archived}
	if s.config.TenantRepository != nil {
		return s.executeWorkItemRecordMutation(ctx, scope, options, request.Mutation, workItemArchive{archiveIssueRequest: request, Archived: archived}, "")
	}
	return s.executeNativeIssueMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		return setNativeArchiveTx(ctx, tx, scope, item, request.ExpectedRevision, archived, now)
	})
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
		return nil, nativePublicInvalid("application service is unavailable")
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
			return nil, nativePublicInvalid("expected revision is required")
		}
		return s.updateNativeCommentCommand(ctx, scope, request.Identifier, request.CommentID, tracker.UpdateComment{Mutation: command, ExpectedRevision: tracker.Revision(request.ExpectedRevision), Body: *request.Body})
	case operatortool.SetDependency:
		if request.ExpectedRevision <= 0 {
			return nil, nativePublicInvalid("expected revision is required")
		}
		return s.changeNativeDependencyCommand(ctx, scope, request.Identifier, tracker.DependencyMutation{Mutation: command, ExpectedRevision: tracker.Revision(request.ExpectedRevision), RelatedWorkItemID: tracker.NativeWorkItemID(request.Related), RelationshipType: request.RelationshipType, Operation: request.Operation})
	case operatortool.RestoreItem:
		return s.setNativeArchiveCommand(ctx, scope, request.Identifier, archiveIssueRequest{Mutation: command, ExpectedRevision: tracker.Revision(request.ExpectedRevision)}, false)
	case operatortool.OrderItem, operatortool.SetQueuePriority:
		return nil, &nativeError{Code: "invalid_request", Message: "Queue ordering and priority overrides require a GitHub-compatible project; native projects use issue priority", status: http.StatusUnprocessableEntity, publicMessage: true}
	default:
		return nil, nativePublicInvalid("application service is unavailable")
	}
}

func (s *Service) operatorCompatibilityWork(ctx context.Context, scope nativeScope, name string, request operatortool.WorkArguments, command tracker.Mutation) (json.RawMessage, error) {
	record, _, err := s.readWorkItemRecord(ctx, scope, request.Identifier)
	if err != nil {
		return nil, err
	}
	id := tracker.WorkItemID(record.ID)
	switch name {
	case operatortool.SetQueuePriority:
		result, err := s.workItemPriorityCommand(ctx, id, priorityMutationRequest{Scope: request.Scope, State: request.State, Priority: request.QueuePriority, IdempotencyKey: command.IdempotencyKey})
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	case operatortool.SetDependency, operatortool.OrderItem:
		options := nativeCommandOptions{OperationID: name + " " + string(scope.organization) + "/" + string(scope.project) + "/" + request.Identifier, Feature: "collaboration"}
		return s.executeNativeMutation(ctx, scope, options, command, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			_, id, err := readNativeIssue(ctx, tx, scope, request.Identifier)
			if err != nil {
				return nil, err
			}
			if name == operatortool.SetDependency {
				if request.RelationshipType == "related" {
					return nil, nativePublicInvalid("Related links require native issue authority")
				}
				_, relatedID, err := readNativeIssue(ctx, tx, scope, request.Related)
				if err != nil {
					return nil, err
				}
				if relatedID == id {
					return nil, nativePublicInvalid("dependency must name another item")
				}
				if err := changeDependencyTx(ctx, tx, id, dependencyMutationRequest{Action: request.Operation, BlockerWorkItemID: relatedID, Provenance: "hub"}, now); err != nil {
					return nil, err
				}
			} else {
				if err := changeQueueOrderTx(ctx, tx, id, orderMutationRequest{Scope: request.Scope, State: request.State, Rank: request.Rank}, now); err != nil {
					return nil, err
				}
			}
			return mutationResponse{WorkItemID: id, Kind: name, Status: "applied"}, nil
		})
	default:
		return nil, nativePublicInvalid("application service is unavailable")
	}
}
