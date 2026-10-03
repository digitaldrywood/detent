package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) createNativeIssueCommand(ctx context.Context, scope nativeScope, request tracker.CreateIssue) (json.RawMessage, error) {
	options := nativeCommandOptions{OperationID: "POST /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items", Feature: "collaboration"}
	result, err := s.executeNativeIssueMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		if origin, machine := issueorigin.Parse(request.Body); machine && origin.Kind == "worker" && request.GitHubIssueURL == "" && request.State == "Backlog" && scope.credential.Runner.RunnerID != "" && request.LeaseID != "" {
			if _, err := validateRunnerLeaseTx(ctx, tx, scope, request.LeaseID, request.FencingToken, now); err != nil {
				return nil, err
			}
			return createNativeMachineIntakeTx(ctx, tx, scope, request, origin.Fingerprint, now)
		}
		return createNativeIssueTx(ctx, tx, scope, request, now)
	})
	if err == nil {
		s.wakeSpriteRunnersAfter(scope, result)
	}
	return result, err
}

func createNativeMachineIntakeTx(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, fingerprint string, now time.Time) (tracker.NativeIssue, error) {
	if _, err := validateNativeIssueDraft(ctx, tx, scope, request); err != nil {
		return tracker.NativeIssue{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT i.native_id, i.body FROM issues i JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE i.organization_id = ? AND i.project_id = ? AND i.archived = 0 AND ws.terminal = 0 ORDER BY i.number`, scope.organization, scope.project)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	var existing string
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			return tracker.NativeIssue{}, errors.Join(err, rows.Close())
		}
		if origin, machine := issueorigin.Parse(body); machine && origin.Fingerprint == fingerprint {
			existing = id
			break
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return tracker.NativeIssue{}, err
	}
	if existing != "" {
		occurrence := issueorigin.Occurrence(request.Body)
		if len(occurrence) > 64<<10 {
			return tracker.NativeIssue{}, nativeInvalid("Comment body must contain 1 byte to 64 KiB")
		}
		issue, _, err := readNativeIssue(ctx, tx, scope, existing)
		if err != nil {
			return tracker.NativeIssue{}, err
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
	options := nativeCommandOptions{OperationID: "PATCH /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items" + "/" + item, Item: item, RequireLease: true, Feature: "collaboration"}
	return s.executeNativeIssueMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		issue, _, err := readNativeIssue(ctx, tx, scope, item)
		if err != nil {
			return nil, err
		}
		if err := requireNativeEdit(issue, request.ExpectedRevision); err != nil {
			return nil, err
		}
		fields := []string{}
		if request.Title != nil {
			issue.Title = *request.Title
			fields = append(fields, "title")
		}
		if request.Body != nil {
			issue.Body = *request.Body
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
			return nil, nativeInvalid("At least one field must be supplied")
		}
		if err := validateNativeContent(issue.Title, issue.Body, issue.Labels, issue.Assignees, issue.Priority); err != nil {
			return nil, err
		}
		if request.Body != nil {
			if err := bindCloudAttachmentReferences(ctx, tx, scope, string(issue.WorkItemID), "", issue.Body); err != nil {
				return nil, err
			}
		}
		return persistNativeIssue(ctx, tx, scope, issue, "issue.edited", tracker.CollaborationData{Fields: fields}, now)
	})
}

func (s *Service) changeNativeDependencyCommand(ctx context.Context, scope nativeScope, item string, request tracker.DependencyMutation) (json.RawMessage, error) {
	options := nativeCommandOptions{OperationID: "POST /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items" + "/" + item + "/dependencies", Item: item, RequireLease: true, Feature: "collaboration"}
	return s.executeNativeIssueMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		issue, dependentID, err := readNativeIssue(ctx, tx, scope, item)
		if err != nil {
			return nil, err
		}
		if err := requireNativeEdit(issue, request.ExpectedRevision); err != nil {
			return nil, err
		}
		if request.Operation != "add" && request.Operation != "remove" {
			return nil, nativeInvalid("Dependency operation must be add or remove")
		}
		condition, grantArgs := scope.credential.projectGrantSQL("i.organization_id", "i.project_id")
		args := append([]any{scope.organization, request.RelatedWorkItemID}, grantArgs...)
		var blockerID tracker.WorkItemID
		var blockerProject tracker.ProjectID
		err = tx.QueryRowContext(ctx, `SELECT i.id, i.project_id FROM issues i WHERE i.organization_id = ? AND i.native_id = ?
AND (`+condition+`)`, args...).Scan(&blockerID, &blockerProject)
		if err != nil {
			return nil, err
		}
		if blockerID == dependentID {
			return nil, nativeInvalid("Dependencies cannot form a cycle")
		}
		if request.Operation == "add" {
			var cycle int
			err := tx.QueryRowContext(ctx, `WITH RECURSIVE reachable(id) AS (SELECT dependent_issue_id FROM issue_dependencies WHERE blocker_issue_id = ? UNION SELECT d.dependent_issue_id FROM issue_dependencies d JOIN reachable r ON d.blocker_issue_id = r.id) SELECT count(*) FROM reachable WHERE id = ?`, dependentID, blockerID).Scan(&cycle)
			if err != nil {
				return nil, err
			}
			if cycle != 0 {
				return nil, nativeInvalid("Dependencies cannot form a cycle")
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO issue_dependencies (blocker_issue_id, dependent_issue_id, provenance, created_at, updated_at) VALUES (?, ?, 'native', ?, ?) ON CONFLICT DO NOTHING", blockerID, dependentID, formatHubTime(now), formatHubTime(now))
			if err != nil {
				return nil, err
			}
			if !slices.Contains(issue.Dependencies, request.RelatedWorkItemID) {
				issue.Dependencies = append(issue.Dependencies, request.RelatedWorkItemID)
				slices.Sort(issue.Dependencies)
			}
		} else {
			if _, err := tx.ExecContext(ctx, "DELETE FROM issue_dependencies WHERE blocker_issue_id = ? AND dependent_issue_id = ?", blockerID, dependentID); err != nil {
				return nil, err
			}
			issue.Dependencies = slices.DeleteFunc(issue.Dependencies, func(id tracker.NativeWorkItemID) bool { return id == request.RelatedWorkItemID })
		}
		return persistNativeIssue(ctx, tx, scope, issue, "dependency.changed", tracker.CollaborationData{RelatedWorkItemID: request.RelatedWorkItemID, Operation: request.Operation}, now)
	})
}

func (s *Service) createNativeCommentCommand(ctx context.Context, scope nativeScope, item string, request tracker.CreateComment) (json.RawMessage, error) {
	options := nativeCommandOptions{OperationID: "POST /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items" + "/" + item + "/comments", Item: item, RequireLease: true, Feature: "collaboration"}
	return s.executeNativeMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		issue, _, err := readNativeIssue(ctx, tx, scope, item)
		if err != nil {
			return nil, err
		}
		if issue.Profile != "native" {
			return nil, nativeInvalid("Compatibility project discussion is externally owned")
		}
		if strings.TrimSpace(request.Body) == "" || len(request.Body) > 64<<10 {
			return nil, nativeInvalid("Comment body must contain 1 byte to 64 KiB")
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
	options := nativeCommandOptions{OperationID: "PATCH /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items" + "/" + item + "/comments/" + comment, Item: item, RequireLease: true, Feature: "collaboration"}
	return s.executeNativeMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		comment, err := readNativeComment(ctx, tx, scope, item, comment)
		if err != nil {
			return nil, err
		}
		if comment.Actor.PrincipalID != scope.credential.ID && scope.credential.Scope == apiScopeWorker {
			return nil, nativeNotFound()
		}
		if request.ExpectedRevision <= 0 {
			return nil, nativeInvalid("Expected revision must be positive")
		}
		if comment.Revision != request.ExpectedRevision {
			return nil, nativeConflict(comment.Revision)
		}
		if strings.TrimSpace(request.Body) == "" || len(request.Body) > 64<<10 {
			return nil, nativeInvalid("Comment body must contain 1 byte to 64 KiB")
		}
		comment.Body = request.Body
		comment.Revision++
		comment.UpdatedAt = now
		editor := scope.actor()
		comment.EditedBy = &editor
		editedBy, err := marshalNative(editor)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, "UPDATE native_comments SET body = ?, revision = ?, edited_by_json = ?, updated_at = ? WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND id = ?", comment.Body, comment.Revision, editedBy, formatHubTime(now), scope.organization, scope.project, comment.WorkItemID, comment.ID)
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
	return s.executeNativeIssueMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		issue, id, err := readNativeIssue(ctx, tx, scope, item)
		if err != nil {
			return nil, err
		}
		if err := requireNativeEdit(issue, request.ExpectedRevision); err != nil {
			return nil, err
		}
		if scope.credential.Scope == apiScopeWorker {
			return nil, nativeInvalid("Archive and restore require an operator")
		}
		if issue.Archived == archived {
			return issue, nil
		}
		if archived {
			lease, found, err := readUnreleasedLease(ctx, tx, id)
			if err != nil {
				return nil, err
			}
			if found && lease.session.ExpiresAt.After(now) {
				return nil, fmt.Errorf("%w: finish or stop the active work before archiving", tracker.ErrLeaseConflict)
			}
		}
		issue.Archived = archived
		operation := "restore"
		if archived {
			operation = "archive"
		}
		return persistNativeIssue(ctx, tx, scope, issue, "issue.edited", tracker.CollaborationData{Fields: []string{"archived"}, Operation: operation}, now)
	})
}

func (s *Service) nativeWorkObservation(ctx context.Context, scope nativeScope, item string) (tracker.NativeIssue, error) {
	issue, _, err := readNativeIssue(ctx, s.database.db, scope, item)
	return s.nativeIssueResponse(issue), err
}

func (s *Service) nativeWorkComments(ctx context.Context, scope nativeScope, item string, limit int, cursor string) (tracker.Page[tracker.NativeComment], error) {
	params := url.Values{"limit": {strconv.Itoa(limit)}, "cursor": {cursor}}
	return s.readComments(ctx, scope, item, params)
}

// operatorCreateNativeWork uses the native application command. Hub-only
// deployments have no operator confirmation surface, so terminal creation is
// unavailable there; a daemon deployment uses its existing approval service.
func (s *Service) operatorCreateNativeWork(ctx context.Context, scope nativeScope, request tracker.CreateIssue) (json.RawMessage, error) {
	project, err := readNativeProject(ctx, s.database.db, scope)
	if err != nil {
		return nil, err
	}
	if request.State == "" && len(project.States) > 0 {
		request.State = project.States[0].Name
	}
	for _, state := range project.States {
		if state.Name == request.State && state.Terminal {
			return nil, nativeInvalid("operator approval service is unavailable")
		}
	}
	return s.createNativeIssueCommand(ctx, scope, request)
}

func (s *Service) operatorNativeWork(ctx context.Context, scope nativeScope, name string, request operatortool.WorkArguments) (json.RawMessage, error) {
	command := tracker.MutationForContext(ctx, "")
	if request.Target == "pr" {
		return nil, nativeInvalid("application service is unavailable")
	}
	current, err := s.nativeWorkObservation(ctx, scope, request.Identifier)
	if err != nil {
		return nil, err
	}
	if current.Profile == "github_compatible" {
		return s.operatorCompatibilityWork(ctx, scope, name, request, command)
	}
	switch name {
	case operatortool.EditItem:
		current, err := s.nativeWorkObservation(ctx, scope, request.Identifier)
		if err != nil {
			return nil, err
		}
		metadata, approved := mutation.FromContext(ctx)
		approved = approved && metadata.Source == "chat" && metadata.Confirmation == "approved"
		if !approved && request.Body != nil && strings.TrimSpace(*request.Body) == "" && current.Body != "" {
			return nil, nativeInvalid("operator approval service is unavailable")
		}
		if request.Labels != nil {
			for _, label := range current.Labels {
				if !approved && !slices.Contains(*request.Labels, label) {
					return nil, nativeInvalid("operator approval service is unavailable")
				}
			}
		}
		return s.updateNativeIssueCommand(ctx, scope, request.Identifier, tracker.UpdateIssue{Mutation: command, ExpectedRevision: tracker.Revision(request.ExpectedRevision), Title: request.Title, Body: request.Body, Labels: request.Labels, Priority: tracker.SetPriority(request.Priority)})
	case operatortool.AddComment:
		return s.createNativeCommentCommand(ctx, scope, request.Identifier, tracker.CreateComment{Mutation: command, Body: *request.Body})
	case operatortool.EditComment:
		if request.ExpectedRevision <= 0 {
			return nil, nativeInvalid("expected revision is required")
		}
		return s.updateNativeCommentCommand(ctx, scope, request.Identifier, request.CommentID, tracker.UpdateComment{Mutation: command, ExpectedRevision: tracker.Revision(request.ExpectedRevision), Body: *request.Body})
	case operatortool.SetDependency:
		if request.ExpectedRevision <= 0 {
			return nil, nativeInvalid("expected revision is required")
		}
		return s.changeNativeDependencyCommand(ctx, scope, request.Identifier, tracker.DependencyMutation{Mutation: command, ExpectedRevision: tracker.Revision(request.ExpectedRevision), RelatedWorkItemID: tracker.NativeWorkItemID(request.Related), Operation: request.Operation})
	case operatortool.RestoreItem:
		return s.setNativeArchiveCommand(ctx, scope, request.Identifier, archiveIssueRequest{Mutation: command, ExpectedRevision: tracker.Revision(request.ExpectedRevision)}, false)
	default:
		return nil, nativeInvalid("application service is unavailable")
	}
}

func (s *Service) operatorCompatibilityWork(ctx context.Context, scope nativeScope, name string, request operatortool.WorkArguments, command tracker.Mutation) (json.RawMessage, error) {
	_, id, err := readNativeIssue(ctx, s.database.db, scope, request.Identifier)
	if err != nil {
		return nil, err
	}
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
				_, relatedID, err := readNativeIssue(ctx, tx, scope, request.Related)
				if err != nil {
					return nil, err
				}
				if relatedID == id {
					return nil, nativeInvalid("dependency must name another item")
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
		return nil, nativeInvalid("application service is unavailable")
	}
}
