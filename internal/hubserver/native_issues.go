package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func (s *Service) nativeIssueResponse(issue tracker.NativeIssue) tracker.NativeIssue {
	issue.WebURL = ""
	if s.config.Hosted != nil {
		issue.WebURL = s.config.Hosted.PublicURL + s.hostedPath("/work/i/"+url.PathEscape(string(issue.WorkItemID)))
	}
	return issue
}

func (s *Service) nativeIssueJSON(body json.RawMessage) (json.RawMessage, error) {
	var issue tracker.NativeIssue
	if err := json.Unmarshal(body, &issue); err != nil {
		return nil, err
	}
	return json.Marshal(s.nativeIssueResponse(issue))
}

func (s *Service) executeNativeIssueMutation(ctx context.Context, scope nativeScope, options nativeCommandOptions, command tracker.Mutation, input any, operation func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error)) (json.RawMessage, error) {
	result, err := s.executeNativeMutation(ctx, scope, options, command, input, operation)
	if err != nil {
		return nil, err
	}
	return s.nativeIssueJSON(result)
}

func readNativeIssue(ctx context.Context, query nativeQueryer, scope nativeScope, id string) (tracker.NativeIssue, tracker.WorkItemID, error) {
	return readNativeIssueProjection(ctx, query, scope, id, false)
}

func readNativeIssueProjection(ctx context.Context, query nativeQueryer, scope nativeScope, id string, compact bool) (tracker.NativeIssue, tracker.WorkItemID, error) {
	var issue tracker.NativeIssue
	var internalID tracker.WorkItemID
	var labels, assignees, actor, created, updated, activity, externalID string
	var sourceAuthor, sourceCreated, sourceUpdated, sourceObserved string
	var repositoryOwner, repositoryName, importedURL string
	var sourceNumber int
	var provenance sql.NullString
	var priority sql.NullInt64
	bodyColumn := "i.body"
	if compact {
		bodyColumn = "''"
	}
	err := query.QueryRowContext(ctx, `SELECT i.id, i.native_id, i.organization_id, i.project_id, i.number, i.revision, p.profile,
 i.title, `+bodyColumn+`, COALESCE(ws.detent_state, ''), COALESCE(ws.terminal, 0), q.priority_override, i.labels_json, i.assignees_json,
 i.actor_json, i.provenance_json, i.native_created_at, i.native_updated_at, i.last_activity_at, COALESCE(i.github_node_id, ''),
 i.author_login, i.created_at, i.source_updated_at, i.synchronized_at, p.require_dependencies = 0, i.archived,
 COALESCE(r.github_owner, ''), COALESCE(r.github_name, ''), COALESCE(i.github_number, 0), i.url
FROM issues i JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
LEFT JOIN repositories r ON r.id = i.repository_id
LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
LEFT JOIN queue_entries q ON q.id = (SELECT id FROM queue_entries WHERE issue_id = i.id ORDER BY id LIMIT 1)
WHERE i.organization_id = ? AND i.project_id = ? AND i.native_id = ?`, scope.organization, scope.project, id).Scan(
		&internalID, &issue.WorkItemID, &issue.OrganizationID, &issue.ProjectID, &issue.Number, &issue.Revision, &issue.Profile,
		&issue.Title, &issue.Body, &issue.State, &issue.Terminal, &priority, &labels, &assignees, &actor, &provenance, &created, &updated, &activity, &externalID,
		&sourceAuthor, &sourceCreated, &sourceUpdated, &sourceObserved, &issue.IgnoreDependencies, &issue.Archived,
		&repositoryOwner, &repositoryName, &sourceNumber, &importedURL)
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
	issue.LinkedSource, err = readLinkedIssueSourceProjection(ctx, query, id, compact)
	if err != nil {
		return issue, 0, err
	}
	var sourceURL string
	if issue.LinkedSource != nil {
		sourceURL = issue.LinkedSource.URL
	}
	issue.ExternalReferences = nativeSourceReferences(externalID, repositoryOwner, repositoryName, sourceNumber, sourceURL, importedURL, issue.Provenance)
	if externalID != "" && issue.Provenance == nil {
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
	}
	if issue.CreatedAt, err = parseTimeValue(created); err != nil {
		return issue, 0, err
	}
	if issue.UpdatedAt, err = parseTimeValue(updated); err != nil {
		return issue, 0, err
	}
	if issue.LastActivityAt, err = parseTimeValue(activity); err != nil {
		return issue, 0, err
	}
	if priority.Valid {
		value := int(priority.Int64)
		issue.Priority = &value
	}
	issue.Dependencies = []tracker.NativeWorkItemID{}
	issue.Blockers = []tracker.NativeDependency{}
	condition, grantArgs := scope.credential.projectGrantSQL("i.organization_id", "i.project_id")
	args := append([]any{internalID, scope.organization}, grantArgs...)
	rows, err := query.QueryContext(ctx, `SELECT i.native_id, i.project_id, i.project_id || '#' || i.number, COALESCE(ws.detent_state, ''), COALESCE(ws.terminal, 0) FROM issue_dependencies d JOIN issues i ON i.id = d.blocker_issue_id
LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE d.dependent_issue_id = ? AND i.organization_id = ?
AND (`+condition+`)
ORDER BY i.native_id`, args...)
	if err != nil {
		return issue, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var dependency tracker.NativeDependency
		if err := rows.Scan(&dependency.ID, &dependency.ProjectID, &dependency.Identifier, &dependency.State, &dependency.Terminal); err != nil {
			return issue, 0, err
		}
		issue.Dependencies = append(issue.Dependencies, dependency.ID)
		issue.Blockers = append(issue.Blockers, dependency)
	}
	return issue, internalID, rows.Err()
}

func (s *Service) getNativeIssue(c echo.Context) error {
	ctx := c.Request().Context()
	scope := nativeRequestScope(c)
	issue, _, err := readNativeIssue(ctx, s.database.db, scope, c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	// The change review surface is absent unless the caller asked for it, so
	// the default resource is byte-for-byte what it was and only a caller
	// that needs it pays for the extra reads.
	included, err := nativeIssueChangeIncluded(c.QueryParam("include"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if included {
		if issue.Change, err = readNativeIssueChange(ctx, s.database.db, scope, string(issue.WorkItemID)); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return c.JSON(http.StatusOK, s.nativeIssueResponse(issue))
}

// nativeIssueChangeIncluded reads the work item resource's include query. The
// resource supports one member, and an unknown one is refused rather than
// ignored so a client typo is visible.
func nativeIssueChangeIncluded(value string) (bool, error) {
	included := false
	for name := range strings.SplitSeq(value, ",") {
		switch name = strings.TrimSpace(name); name {
		case "":
		case "change":
			included = true
		default:
			return false, nativeInvalid("include supports change")
		}
	}
	return included, nil
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
	return validateImportProvenance(provenance)
}

func validateImportProvenance(provenance *tracker.Provenance) error {
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
	result, err := s.createNativeIssueCommand(c.Request().Context(), nativeRequestScope(c), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func validateNativeIssueDraft(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue) (tracker.NativeProject, error) {
	if err := validateNativeContent(request.Title, request.Body, request.Labels, request.Assignees, request.Priority); err != nil {
		return tracker.NativeProject{}, err
	}
	if err := requireUnreservedLabels(ctx, request.Labels); err != nil {
		return tracker.NativeProject{}, err
	}
	if err := validateNativeProvenance(scope, request.Provenance); err != nil {
		return tracker.NativeProject{}, err
	}
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return tracker.NativeProject{}, err
	}
	if project.Profile != "native" {
		return tracker.NativeProject{}, nativeInvalid("Compatibility project content is externally owned")
	}
	return project, nil
}

func createNativeIssueTx(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, now time.Time) (tracker.NativeIssue, error) {
	return createNativeIssue(ctx, tx, scope, request, now, false)
}

func createNativeIssue(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, now time.Time, machineIntake bool) (tracker.NativeIssue, error) {
	if request.GitHubIssueURL != "" {
		return createLinkedIssueTx(ctx, tx, scope, request, now)
	}
	project, err := validateNativeIssueDraft(ctx, tx, scope, request)
	if err != nil {
		return tracker.NativeIssue{}, err
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
			return tracker.NativeIssue{}, err
		}
	}
	var workflowID int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = ?", scope.project, request.State).Scan(&workflowID); err != nil {
		return tracker.NativeIssue{}, nativeInvalid("Workflow state does not exist")
	}
	issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: tracker.NativeWorkItemID(newNativeID("wi")), Revision: 1, Profile: "native"},
		Title: request.Title, Body: request.Body, State: request.State, Priority: request.Priority, Labels: request.Labels, Assignees: request.Assignees,
		Actor: scope.actor(), Provenance: request.Provenance, CreatedAt: now, UpdatedAt: now, LastActivityAt: now, Dependencies: []tracker.NativeWorkItemID{}}
	issue.Blockers = []tracker.NativeDependency{}
	issue.IgnoreDependencies = !project.RequireDependencies
	var importedURL string
	var sourceNumber sql.NullInt64
	if issue.Provenance != nil && issue.Provenance.Provider == "github" {
		importedURL = githubMigrationSourceURL(issue.Body)
		if importedURL != "" {
			_, _, number, err := tracker.ParseGitHubIssueURL(importedURL)
			if err != nil {
				return tracker.NativeIssue{}, err
			}
			sourceNumber = sql.NullInt64{Int64: int64(number), Valid: true}
		}
	}
	issue.ExternalReferences = nativeSourceReferences("", "", "", 0, "", importedURL, issue.Provenance)
	for _, state := range project.States {
		if state.Name == issue.State {
			if state.OperatorOnly && scope.credential.Scope == apiScopeWorker && !machineIntake {
				return tracker.NativeIssue{}, nativeInvalid("Workflow target requires an operator")
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
		return tracker.NativeIssue{}, err
	}
	labels, err := marshalNative(issue.Labels)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	assignees, err := marshalNative(issue.Assignees)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	actor, err := marshalNative(issue.Actor)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	provenance, err := marshalNative(issue.Provenance)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	author := issue.Actor.PrincipalID
	if issue.Provenance != nil {
		author = issue.Provenance.AuthorID
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO issues (native_id, organization_id, project_id, number, workflow_state_id, title, body, url, github_number, github_state, labels_json, assignees_json, source_version, source_updated_at, synchronized_at, created_at, updated_at, author_login, actor_json, provenance_json, native_source_key, native_created_at, native_updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?, ?, '', '', '', ?, ?, ?, ?, ?, ?, ?, ?)`, issue.WorkItemID, scope.organization, scope.project, issue.Number, workflowID, issue.Title, issue.Body, importedURL, sourceNumber, labels, assignees, formatHubTime(now), formatHubTime(now), author, actor, provenance, sourceKey, formatHubTime(now), formatHubTime(now))
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO queue_entries (issue_id, workflow_state_id, scope, state, rank, priority_override, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", id, workflowID, scope.project, issue.State, string(issue.WorkItemID), issue.Priority, formatHubTime(now), formatHubTime(now)); err != nil {
		return tracker.NativeIssue{}, err
	}
	if err := bindCloudAttachmentReferences(ctx, tx, scope, string(issue.WorkItemID), "", issue.Body); err != nil {
		return tracker.NativeIssue{}, err
	}
	if err := recordNativeChange(ctx, tx, scope, issue, string(issue.WorkItemID), issue.Revision, "issue.created", tracker.CollaborationData{Revision: issue.Revision}, now); err != nil {
		return tracker.NativeIssue{}, err
	}
	return issue, nil
}

func recordNativeChange(ctx context.Context, tx *sql.Tx, scope nativeScope, record any, workItemID string, revision tracker.Revision, eventType string, data tracker.CollaborationData, now time.Time) error {
	switch value := record.(type) {
	case tracker.NativeIssue:
		if err := syncCloudAttachmentReferences(ctx, tx, scope, workItemID, "", value.Body); err != nil {
			return err
		}
	case tracker.NativeComment:
		if err := syncCloudAttachmentReferences(ctx, tx, scope, workItemID, value.ID, value.Body); err != nil {
			return err
		}
	}
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
	_, err = tx.ExecContext(ctx, `UPDATE issues SET title = ?, body = ?, labels_json = ?, assignees_json = ?, revision = ?, updated_at = ?, native_updated_at = ?, archived = ?,
workflow_state_id = (SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = ?)
WHERE organization_id = ? AND project_id = ? AND native_id = ?`, issue.Title, issue.Body, labels, assignees, issue.Revision, formatHubTime(now), formatHubTime(now), issue.Archived, scope.project, issue.State, scope.organization, scope.project, issue.WorkItemID)
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
	result, err := s.updateNativeIssueCommand(c.Request().Context(), nativeRequestScope(c), c.Param("item"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func (s *Service) transitionNativeIssue(c echo.Context) error {
	var request tracker.Transition
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	result, err := s.transitionNativeIssueCommand(c.Request().Context(), nativeRequestScope(c), c.Param("item"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func (s *Service) transitionNativeIssueCommand(ctx context.Context, scope nativeScope, item string, request tracker.Transition) (json.RawMessage, error) {
	recordedRecovery := request.Reason == "dependency_ready" && request.BlockerAttemptID != ""
	options := nativeCommandOptions{OperationID: nativeOperation(scope, "POST", "/work-items/"+item+"/workflow"), Item: item, RequireLease: !recordedRecovery, Feature: "collaboration"}
	result, err := s.executeNativeIssueMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		issue, _, err := readNativeIssue(ctx, tx, scope, item)
		if err != nil {
			return nil, err
		}
		if err := requireNativeEdit(issue, request.ExpectedRevision); err != nil {
			return nil, err
		}
		if recordedRecovery {
			if err := validateNativeRecordedRecovery(ctx, tx, scope, issue, request, now); err != nil {
				return nil, err
			}
		}
		if !slices.Contains([]string{"user_requested", "worker_progress", "dependency_ready"}, request.Reason) {
			return nil, nativeInvalid("Transition reason is invalid")
		}
		if len(request.ReasonDetail) > workpad.MaxFinalSummaryBytes || !utf8.ValidString(request.ReasonDetail) {
			return nil, nativeInvalid("Transition reason detail must be bounded UTF-8 text")
		}
		project, err := readNativeProject(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if err := validateNativeWorkflowTransition(project.States, issue.State, request.State, scope.credential.Scope); err != nil {
			return nil, err
		}
		from := issue.State
		issue.State = request.State
		return persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{BlockerAttemptID: request.BlockerAttemptID, FromState: from, ToState: issue.State, Reason: request.Reason, ReasonDetail: strings.TrimSpace(request.ReasonDetail)}, now)
	})
	if err == nil {
		s.wakeSpriteRunnersAfter(scope, result)
	}
	return result, err
}

func validateNativeWorkflowTransition(states []tracker.NativeState, current, target string, scope apiScope) error {
	allowed := false
	for _, state := range states {
		if state.Name == target && state.OperatorOnly && scope == apiScopeWorker {
			return nativeInvalid("Workflow target requires an operator")
		}
		if state.Name == current && slices.Contains(state.Transitions, target) {
			allowed = true
		}
	}
	if !allowed {
		return &nativeError{Code: "transition_not_allowed", Message: "Workflow transition is not allowed", status: http.StatusUnprocessableEntity}
	}
	return nil
}

func (s *Service) changeNativeDependency(c echo.Context) error {
	var request tracker.DependencyMutation
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	result, err := s.changeNativeDependencyCommand(c.Request().Context(), nativeRequestScope(c), c.Param("item"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}
