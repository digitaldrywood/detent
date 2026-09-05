package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func readNativeIssue(ctx context.Context, query nativeQueryer, scope nativeScope, id string) (tracker.NativeIssue, tracker.WorkItemID, error) {
	var issue tracker.NativeIssue
	var internalID tracker.WorkItemID
	var labels, assignees, actor, created, updated, externalID string
	var sourceAuthor, sourceCreated, sourceUpdated, sourceObserved string
	var provenance sql.NullString
	var priority sql.NullInt64
	err := query.QueryRowContext(ctx, `SELECT i.id, i.native_id, i.organization_id, i.project_id, i.number, i.revision, p.profile,
 i.title, i.body, COALESCE(ws.detent_state, ''), COALESCE(ws.terminal, 0), q.priority_override, i.labels_json, i.assignees_json,
 i.actor_json, i.provenance_json, i.native_created_at, i.native_updated_at, COALESCE(i.github_node_id, ''),
 i.author_login, i.created_at, i.source_updated_at, i.synchronized_at, p.require_dependencies = 0
FROM issues i JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
LEFT JOIN queue_entries q ON q.id = (SELECT id FROM queue_entries WHERE issue_id = i.id ORDER BY id LIMIT 1)
WHERE i.organization_id = ? AND i.project_id = ? AND i.native_id = ?`, scope.organization, scope.project, id).Scan(
		&internalID, &issue.WorkItemID, &issue.OrganizationID, &issue.ProjectID, &issue.Number, &issue.Revision, &issue.Profile,
		&issue.Title, &issue.Body, &issue.State, &issue.Terminal, &priority, &labels, &assignees, &actor, &provenance, &created, &updated, &externalID,
		&sourceAuthor, &sourceCreated, &sourceUpdated, &sourceObserved, &issue.IgnoreDependencies)
	if err != nil {
		return issue, 0, err
	}
	if err := json.Unmarshal([]byte(labels), &issue.Labels); err != nil {
		return issue, 0, err
	}
	if err := json.Unmarshal([]byte(assignees), &issue.Assignees); err != nil {
		return issue, 0, err
	}
	if err := json.Unmarshal([]byte(actor), &issue.Actor); err != nil {
		return issue, 0, err
	}
	if provenance.Valid {
		if err := json.Unmarshal([]byte(provenance.String), &issue.Provenance); err != nil {
			return issue, 0, err
		}
	}
	issue.ExternalReferences = []tracker.ExternalReference{}
	if externalID != "" {
		issue.Provenance = &tracker.Provenance{Provider: "github", ExternalID: externalID, AuthorID: sourceAuthor}
		if issue.Provenance.CreatedAt, err = parseTimeValue(sourceCreated); err != nil {
			return issue, 0, err
		}
		if issue.Provenance.UpdatedAt, err = parseTimeValue(sourceUpdated); err != nil {
			return issue, 0, err
		}
		if issue.Provenance.ObservedAt, err = parseTimeValue(sourceObserved); err != nil {
			return issue, 0, err
		}
		issue.ExternalReferences = append(issue.ExternalReferences, tracker.ExternalReference{Provider: "github", Kind: "issue", ID: externalID})
	} else if issue.Provenance != nil {
		issue.ExternalReferences = append(issue.ExternalReferences, tracker.ExternalReference{Provider: issue.Provenance.Provider, Kind: "issue", ID: issue.Provenance.ExternalID})
	}
	if issue.CreatedAt, err = parseTimeValue(created); err != nil {
		return issue, 0, err
	}
	if issue.UpdatedAt, err = parseTimeValue(updated); err != nil {
		return issue, 0, err
	}
	if priority.Valid {
		value := int(priority.Int64)
		issue.Priority = &value
	}
	issue.Dependencies = []tracker.NativeWorkItemID{}
	issue.Blockers = []tracker.NativeDependency{}
	rows, err := query.QueryContext(ctx, `SELECT i.native_id, i.project_id, COALESCE(ws.detent_state, ''), COALESCE(ws.terminal, 0) FROM issue_dependencies d JOIN issues i ON i.id = d.blocker_issue_id
LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE d.dependent_issue_id = ? AND i.organization_id = ?
AND (? = 1 OR EXISTS (SELECT 1 FROM token_grants g WHERE g.token_id = ? AND g.organization_id = i.organization_id AND g.project_id = i.project_id))
ORDER BY i.native_id`, internalID, scope.organization, scope.credential.Scope == apiScopeAdmin && !scope.credential.NativeOnly, scope.credential.ID)
	if err != nil {
		return issue, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var dependency tracker.NativeDependency
		if err := rows.Scan(&dependency.ID, &dependency.ProjectID, &dependency.State, &dependency.Terminal); err != nil {
			return issue, 0, err
		}
		issue.Dependencies = append(issue.Dependencies, dependency.ID)
		issue.Blockers = append(issue.Blockers, dependency)
	}
	return issue, internalID, rows.Err()
}

func (s *Service) getNativeIssue(c echo.Context) error {
	issue, _, err := readNativeIssue(c.Request().Context(), s.database.db, nativeRequestScope(c), c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, issue)
}

func validateNativeContent(title, body string, labels, assignees []string, priority *int) error {
	if strings.TrimSpace(title) == "" || len(title) > 500 || len(body) > 256<<10 {
		return nativeInvalid("Title must contain 1 to 500 bytes and body at most 256 KiB")
	}
	if priority != nil && (*priority < 0 || *priority > 3) {
		return nativeInvalid("Priority must be between 0 and 3")
	}
	for _, values := range [][]string{labels, assignees} {
		if len(values) > 100 {
			return nativeInvalid("At most 100 labels or assignees are supported")
		}
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 200 {
				return nativeInvalid("Labels and assignees must contain 1 to 200 bytes")
			}
		}
	}
	return nil
}

func validateNativeProvenance(scope nativeScope, provenance *tracker.Provenance) error {
	if provenance == nil {
		return nil
	}
	if scope.credential.Scope == apiScopeWorker {
		return nativeInvalid("Imported provenance requires an operator")
	}
	if provenance.Provider != "github" || strings.TrimSpace(provenance.ExternalID) == "" || len(provenance.ExternalID) > 200 || strings.TrimSpace(provenance.AuthorID) == "" || len(provenance.AuthorID) > 200 || len(provenance.AuthorDisplayName) > 200 {
		return nativeInvalid("Import source and author are invalid")
	}
	if provenance.CreatedAt.IsZero() || provenance.UpdatedAt.Before(provenance.CreatedAt) || provenance.ObservedAt.Before(provenance.UpdatedAt) {
		return nativeInvalid("Import timestamps are invalid")
	}
	return nil
}

func (s *Service) createNativeIssue(c echo.Context) error {
	var request tracker.CreateIssue
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		if err := validateNativeContent(request.Title, request.Body, request.Labels, request.Assignees, request.Priority); err != nil {
			return nil, err
		}
		if err := validateNativeProvenance(scope, request.Provenance); err != nil {
			return nil, err
		}
		project, err := readNativeProject(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if project.Profile != "native" {
			return nil, nativeInvalid("Compatibility project content is externally owned")
		}
		var sourceKey any
		if request.Provenance != nil {
			sourceKey = request.Provenance.Provider + ":" + request.Provenance.ExternalID
			var existing string
			err := tx.QueryRowContext(ctx, "SELECT native_id FROM issues WHERE organization_id = ? AND project_id = ? AND native_source_key = ?", scope.organization, scope.project, sourceKey).Scan(&existing)
			if err == nil {
				issue, _, err := readNativeIssue(ctx, tx, scope, existing)
				return issue, err
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		var workflowID int64
		if err := tx.QueryRowContext(ctx, "SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = ?", scope.project, request.State).Scan(&workflowID); err != nil {
			return nil, nativeInvalid("Workflow state does not exist")
		}
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: tracker.NativeWorkItemID(newNativeID("wi")), Revision: 1, Profile: "native"},
			Title: request.Title, Body: request.Body, State: request.State, Priority: request.Priority, Labels: request.Labels, Assignees: request.Assignees,
			Actor: scope.actor(), Provenance: request.Provenance, CreatedAt: now, UpdatedAt: now, Dependencies: []tracker.NativeWorkItemID{}}
		issue.Blockers = []tracker.NativeDependency{}
		issue.IgnoreDependencies = !project.RequireDependencies
		issue.ExternalReferences = []tracker.ExternalReference{}
		if issue.Provenance != nil {
			issue.ExternalReferences = append(issue.ExternalReferences, tracker.ExternalReference{Provider: issue.Provenance.Provider, Kind: "issue", ID: issue.Provenance.ExternalID})
		}
		for _, state := range project.States {
			if state.Name == issue.State {
				if state.OperatorOnly && scope.credential.Scope == apiScopeWorker {
					return nil, nativeInvalid("Workflow target requires an operator")
				}
				issue.Terminal = state.Terminal
			}
		}
		if issue.Labels == nil {
			issue.Labels = []string{}
		}
		if issue.Assignees == nil {
			issue.Assignees = []string{}
		}
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(number), 0) + 1 FROM issues WHERE organization_id = ? AND project_id = ?", scope.organization, scope.project).Scan(&issue.Number); err != nil {
			return nil, err
		}
		labels, err := marshalNative(issue.Labels)
		if err != nil {
			return nil, err
		}
		assignees, err := marshalNative(issue.Assignees)
		if err != nil {
			return nil, err
		}
		actor, err := marshalNative(issue.Actor)
		if err != nil {
			return nil, err
		}
		provenance, err := marshalNative(issue.Provenance)
		if err != nil {
			return nil, err
		}
		author := issue.Actor.PrincipalID
		if issue.Provenance != nil {
			author = issue.Provenance.AuthorID
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO issues (native_id, organization_id, project_id, number, workflow_state_id, title, body, url, github_state, labels_json, assignees_json, source_version, source_updated_at, synchronized_at, created_at, updated_at, author_login, actor_json, provenance_json, native_source_key, native_created_at, native_updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, '', 'open', ?, ?, '', '', '', ?, ?, ?, ?, ?, ?, ?, ?)`, issue.WorkItemID, scope.organization, scope.project, issue.Number, workflowID, issue.Title, issue.Body, labels, assignees, formatHubTime(now), formatHubTime(now), author, actor, provenance, sourceKey, formatHubTime(now), formatHubTime(now))
		if err != nil {
			return nil, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO queue_entries (issue_id, workflow_state_id, scope, state, rank, priority_override, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", id, workflowID, scope.project, issue.State, string(issue.WorkItemID), issue.Priority, formatHubTime(now), formatHubTime(now)); err != nil {
			return nil, err
		}
		if err := recordNativeChange(ctx, tx, scope, issue, string(issue.WorkItemID), issue.Revision, "issue.created", tracker.CollaborationData{Revision: issue.Revision}, now); err != nil {
			return nil, err
		}
		return issue, nil
	})
}

func recordNativeChange(ctx context.Context, tx *sql.Tx, scope nativeScope, record any, workItemID string, revision tracker.Revision, eventType string, data tracker.CollaborationData, now time.Time) error {
	recordID := workItemID
	if data.CommentID != "" {
		recordID = data.CommentID
	}
	encoded, err := marshalNative(record)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO collaboration_versions (organization_id, project_id, work_item_id, record_id, revision, record_json) VALUES (?, ?, ?, ?, ?, ?)", scope.organization, scope.project, workItemID, recordID, revision, encoded); err != nil {
		return err
	}
	return appendNativeHistory(ctx, tx, scope, workItemID, eventType, data, now)
}

func appendNativeHistory(ctx context.Context, tx *sql.Tx, scope nativeScope, workItemID string, eventType string, data tracker.CollaborationData, now time.Time) error {
	var sequence int64
	if err := tx.QueryRowContext(ctx, "UPDATE issues SET event_sequence = event_sequence + 1 WHERE organization_id = ? AND project_id = ? AND native_id = ? RETURNING event_sequence", scope.organization, scope.project, workItemID).Scan(&sequence); err != nil {
		return err
	}
	actor, err := marshalNative(scope.actor())
	if err != nil {
		return err
	}
	payload, err := marshalNative(data)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO collaboration_events (id, organization_id, project_id, work_item_id, sequence, type, schema_version, actor_json, data_json, recorded_at) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)", newNativeID("evt"), scope.organization, scope.project, workItemID, sequence, eventType, actor, payload, formatHubTime(now))
	return err
}

func requireNativeEdit(issue tracker.NativeIssue, expected tracker.Revision) error {
	if issue.Profile != "native" {
		return nativeInvalid("Compatibility project content is externally owned")
	}
	if expected <= 0 {
		return nativeInvalid("Expected revision must be positive")
	}
	if expected != issue.Revision {
		return nativeConflict(issue.Revision)
	}
	return nil
}

func persistNativeIssue(ctx context.Context, tx *sql.Tx, scope nativeScope, issue tracker.NativeIssue, eventType string, data tracker.CollaborationData, now time.Time) (tracker.NativeIssue, error) {
	if err := tx.QueryRowContext(ctx, "SELECT terminal FROM workflow_states WHERE project_id = ? AND detent_state = ?", scope.project, issue.State).Scan(&issue.Terminal); err != nil {
		return issue, err
	}
	issue.Revision++
	issue.UpdatedAt = now
	labels, err := marshalNative(issue.Labels)
	if err != nil {
		return issue, err
	}
	assignees, err := marshalNative(issue.Assignees)
	if err != nil {
		return issue, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE issues SET title = ?, body = ?, labels_json = ?, assignees_json = ?, revision = ?, updated_at = ?, native_updated_at = ?,
workflow_state_id = (SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = ?)
WHERE organization_id = ? AND project_id = ? AND native_id = ?`, issue.Title, issue.Body, labels, assignees, issue.Revision, formatHubTime(now), formatHubTime(now), scope.project, issue.State, scope.organization, scope.project, issue.WorkItemID)
	if err != nil {
		return issue, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE queue_entries SET state = ?, priority_override = ?, updated_at = ? WHERE issue_id = (SELECT id FROM issues WHERE organization_id = ? AND project_id = ? AND native_id = ?)`, issue.State, issue.Priority, formatHubTime(now), scope.organization, scope.project, issue.WorkItemID); err != nil {
		return issue, err
	}
	data.Revision = issue.Revision
	issue, _, err = readNativeIssue(ctx, tx, scope, string(issue.WorkItemID))
	if err != nil {
		return issue, err
	}
	err = recordNativeChange(ctx, tx, scope, issue, string(issue.WorkItemID), issue.Revision, eventType, data, now)
	return issue, err
}

func (s *Service) updateNativeIssue(c echo.Context) error {
	var request tracker.UpdateIssue
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		issue, _, err := readNativeIssue(ctx, tx, scope, c.Param("item"))
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
			issue.Labels = *request.Labels
			fields = append(fields, "labels")
		}
		if request.Assignees != nil {
			issue.Assignees = *request.Assignees
			fields = append(fields, "assignees")
		}
		if request.Priority != nil {
			issue.Priority = request.Priority
			fields = append(fields, "priority")
		}
		if len(fields) == 0 {
			return nil, nativeInvalid("At least one field must be supplied")
		}
		if err := validateNativeContent(issue.Title, issue.Body, issue.Labels, issue.Assignees, issue.Priority); err != nil {
			return nil, err
		}
		return persistNativeIssue(ctx, tx, scope, issue, "issue.edited", tracker.CollaborationData{Fields: fields}, now)
	})
}

func (s *Service) transitionNativeIssue(c echo.Context) error {
	var request tracker.Transition
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		issue, _, err := readNativeIssue(ctx, tx, scope, c.Param("item"))
		if err != nil {
			return nil, err
		}
		if err := requireNativeEdit(issue, request.ExpectedRevision); err != nil {
			return nil, err
		}
		if !slices.Contains([]string{"user_requested", "worker_progress", "dependency_ready"}, request.Reason) {
			return nil, nativeInvalid("Transition reason is invalid")
		}
		project, err := readNativeProject(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		allowed := false
		for _, state := range project.States {
			if state.Name == request.State && state.OperatorOnly && scope.credential.Scope == apiScopeWorker {
				return nil, nativeInvalid("Workflow target requires an operator")
			}
			if state.Name == issue.State && slices.Contains(state.Transitions, request.State) {
				allowed = true
			}
		}
		if !allowed {
			return nil, nativeInvalid("Workflow transition is not allowed")
		}
		from := issue.State
		issue.State = request.State
		return persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: issue.State, Reason: request.Reason}, now)
	})
}

func (s *Service) changeNativeDependency(c echo.Context) error {
	var request tracker.DependencyMutation
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		issue, dependentID, err := readNativeIssue(ctx, tx, scope, c.Param("item"))
		if err != nil {
			return nil, err
		}
		if err := requireNativeEdit(issue, request.ExpectedRevision); err != nil {
			return nil, err
		}
		if request.Operation != "add" && request.Operation != "remove" {
			return nil, nativeInvalid("Dependency operation must be add or remove")
		}
		var blockerID tracker.WorkItemID
		var blockerProject tracker.ProjectID
		err = tx.QueryRowContext(ctx, `SELECT i.id, i.project_id FROM issues i WHERE i.organization_id = ? AND i.native_id = ?
AND (? = 1 OR EXISTS (SELECT 1 FROM token_grants g WHERE g.token_id = ? AND g.organization_id = i.organization_id AND g.project_id = i.project_id))`, scope.organization, request.RelatedWorkItemID, scope.credential.Scope == apiScopeAdmin && !scope.credential.NativeOnly, scope.credential.ID).Scan(&blockerID, &blockerProject)
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
