package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func (s *Service) nativeIssueResponse(issue tracker.NativeIssue) tracker.NativeIssue {
	if s.config.Hosted != nil && strings.HasPrefix(issue.WebURL, "/") {
		issue.WebURL = strings.TrimRight(s.config.Hosted.PublicURL, "/") + issue.WebURL
	}
	if s.config.Hosted == nil {
		issue.WebURL = ""
	}
	return issue
}

func (s *Service) nativeIssueJSON(ctx context.Context, scope nativeScope, body json.RawMessage) (json.RawMessage, error) {
	var challenge duplicateChallenge
	if err := json.Unmarshal(body, &challenge); err != nil {
		return nil, err
	}
	if challenge.ChallengeToken != "" {
		if scope.credential.HostedRole == "reporter" {
			visible := []possibleDuplicate{}
			for _, match := range challenge.PossibleDuplicates {
				if err := requireReporterItem(ctx, s.database.reader, scope, string(match.WorkItemID)); errors.Is(err, sql.ErrNoRows) || isNativeNotFound(err) {
					continue
				} else if err != nil {
					return nil, err
				}
				visible = append(visible, match)
			}
			challenge.PossibleDuplicates = visible
		}
		for i := range challenge.PossibleDuplicates {
			match := &challenge.PossibleDuplicates[i]
			path, err := readIssueBrowserPath(ctx, s.database.reader, string(match.WorkItemID))
			if err != nil {
				return nil, err
			}
			issue := tracker.NativeIssue{WebURL: path}
			match.WebURL = s.nativeIssueResponse(issue).WebURL
		}
		return json.Marshal(challenge)
	}
	var issue tracker.NativeIssue
	if err := json.Unmarshal(body, &issue); err != nil {
		return nil, err
	}
	if !scope.reporterOwns(issue.Actor) {
		return nil, nativeNotFound()
	}
	path, err := readIssueBrowserPath(ctx, s.database.db, string(issue.WorkItemID))
	if err != nil {
		return nil, err
	}
	issue.WebURL = path
	return json.Marshal(s.nativeIssueResponse(scope.reporterIssue(issue)))
}

func (s *Service) executeNativeIssueMutation(ctx context.Context, scope nativeScope, options nativeCommandOptions, command tracker.Mutation, input any, operation func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error)) (json.RawMessage, error) {
	result, err := s.executeNativeMutation(ctx, scope, options, command, input, operation)
	if err != nil {
		return nil, err
	}
	return s.nativeIssueJSON(ctx, scope, result)
}

func nativeActorDisplayName(principal, organization string) string {
	return `COALESCE((SELECT COALESCE(
 (SELECT m.email FROM hosted_members m JOIN hosted_tenant h ON h.organization_id = ` + organization + ` WHERE m.principal_id = t.id), t.name)
 FROM api_tokens t WHERE t.id = ` + principal + `), '')`
}

func readNativeIssue(ctx context.Context, query nativeQueryer, scope nativeScope, id string) (tracker.NativeIssue, tracker.WorkItemID, error) {
	issue, internalID, err := readNativeIssueProjection(ctx, query, scope, id, false)
	if err != nil || scope.credential.Runner.RunnerID == "" || !slices.Contains(scope.credential.Runner.Operations, runnerauth.Claim) {
		return issue, internalID, err
	}
	var eligible bool
	err = query.QueryRowContext(ctx, `SELECT i.archived = 0 AND ws.dispatchable = 1 AND ws.terminal = 0 AND `+nativeCandidateDependenciesSatisfied+`
FROM issues i JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE i.id = ? AND i.organization_id = ? AND i.project_id = ?`, internalID, scope.organization, scope.project).Scan(&eligible)
	if err == nil {
		issue.SchedulingEligible = &eligible
	}
	return issue, internalID, err
}

func readNativeIssueProjection(ctx context.Context, query nativeQueryer, scope nativeScope, id string, compact bool) (tracker.NativeIssue, tracker.WorkItemID, error) {
	var issue tracker.NativeIssue
	var internalID tracker.WorkItemID
	var labels, assignees, actor, displayName, created, updated, activity, externalID, contractJSON string
	var sourceAuthor, sourceCreated, sourceUpdated, sourceObserved string
	var repositoryOwner, repositoryName, importedURL, organizationSlug, projectSlug string
	var sourceNumber int
	var provenance, closedAt, listChange, pullRequest, issueType, reportContext, verificationState, verifiedBy, verifiedAt sql.NullString
	var priority sql.NullInt64
	bodyColumn := "i.body"
	if compact {
		bodyColumn = "''"
	}
	visibility, args := scope.issueVisibilitySQL("i")
	args = append([]any{scope.organization, scope.project, id}, args...)
	err := query.QueryRowContext(ctx, `SELECT o.slug, p.slug, i.id, i.native_id, i.organization_id, i.project_id, i.number, i.revision, p.profile,
 i.issue_type, i.report_context_json, i.verification_state, i.verified_by_json, i.verified_at, i.title, `+bodyColumn+`, COALESCE(ws.detent_state, ''), COALESCE(ws.terminal, 0), q.priority_override, i.labels_json, i.assignees_json,
 i.actor_json, `+nativeActorDisplayName("json_extract(i.actor_json, '$.principal_id')", "i.organization_id")+`, i.issue_contract_json, i.provenance_json, i.native_created_at, i.native_updated_at, i.last_activity_at, COALESCE(i.github_node_id, ''),
 i.author_login, i.created_at, i.source_updated_at, i.synchronized_at, p.require_dependencies = 0, i.archived,
 COALESCE(r.github_owner, ''), COALESCE(r.github_name, ''), COALESCE(i.github_number, 0), i.url,
 CASE WHEN ws.terminal = 1 THEN `+nativeTerminalEnteredAt+` ELSE NULL END,
 (SELECT json_object('branch', CASE WHEN json_extract(c.record_json, '$.landed.version_id') = json_extract(c.record_json, '$.current_version_id') THEN COALESCE(json_extract(c.record_json, '$.landed.base_ref'), '') ELSE '' END,
 'head_sha', CASE WHEN json_extract(c.record_json, '$.landed.version_id') = json_extract(c.record_json, '$.current_version_id') THEN COALESCE(json_extract(c.record_json, '$.landed.merge_sha'), '') ELSE COALESCE(json_extract(v.record_json, '$.head_sha'), '') END,
 'landing_pending', json(CASE WHEN (`+nativePendingLandingClause+`) THEN 'true' ELSE 'false' END))
 FROM change_requests c JOIN change_issue_links l ON l.change_id = c.id
 LEFT JOIN change_versions v ON v.id = json_extract(c.record_json, '$.current_version_id') AND v.change_id = c.id
 WHERE c.organization_id = i.organization_id AND c.project_id = i.project_id AND l.work_item_id = i.native_id
 ORDER BY c.rowid DESC LIMIT 1),
 (SELECT json_extract(v.record_json, '$.external')
 FROM change_requests c JOIN change_issue_links l ON l.change_id = c.id
 JOIN change_versions v ON v.change_id = c.id
 WHERE c.organization_id = i.organization_id AND c.project_id = i.project_id AND l.work_item_id = i.native_id
 AND json_extract(v.record_json, '$.external.provider') = 'github'
 AND COALESCE(json_extract(v.record_json, '$.external.url'), '') <> ''
 ORDER BY c.rowid DESC, v.number DESC LIMIT 1)
FROM issues i JOIN organizations o ON o.id=i.organization_id JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
LEFT JOIN repositories r ON r.id = i.repository_id
LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
LEFT JOIN queue_entries q ON q.id = (SELECT id FROM queue_entries WHERE issue_id = i.id ORDER BY id LIMIT 1)
WHERE i.organization_id = ? AND i.project_id = ? AND i.native_id = ? AND (`+visibility+`)`, args...).Scan(
		&organizationSlug, &projectSlug, &internalID, &issue.WorkItemID, &issue.OrganizationID, &issue.ProjectID, &issue.Number, &issue.Revision, &issue.Profile,
		&issueType, &reportContext, &verificationState, &verifiedBy, &verifiedAt, &issue.Title, &issue.Body, &issue.State, &issue.Terminal, &priority, &labels, &assignees, &actor, &displayName, &contractJSON, &provenance, &created, &updated, &activity, &externalID,
		&sourceAuthor, &sourceCreated, &sourceUpdated, &sourceObserved, &issue.IgnoreDependencies, &issue.Archived,
		&repositoryOwner, &repositoryName, &sourceNumber, &importedURL, &closedAt, &listChange, &pullRequest)
	if err != nil {
		return issue, 0, err
	}
	issue.WebURL = issueBrowserPath(organizationSlug, projectSlug, issue.Number)
	issue.Type = issueType.String
	if reportContext.Valid && !compact {
		if err := json.Unmarshal([]byte(reportContext.String), &issue.ReportContext); err != nil {
			return issue, 0, err
		}
	}
	if verificationState.Valid {
		issue.Verification = &tracker.NativeVerification{State: verificationState.String}
		if verifiedBy.Valid {
			issue.Verification.VerifiedBy = &tracker.Actor{}
			if err := json.Unmarshal([]byte(verifiedBy.String), issue.Verification.VerifiedBy); err != nil {
				return issue, 0, err
			}
		}
		if verifiedAt.Valid {
			at, err := parseTimeValue(verifiedAt.String)
			if err != nil {
				return issue, 0, err
			}
			issue.Verification.VerifiedAt = &at
		}
	}
	issue.IssueContract = &issuecontract.State{}
	if err := json.Unmarshal([]byte(contractJSON), issue.IssueContract); err != nil {
		return issue, 0, err
	}
	var legacyConfirmation struct {
		Body string `json:"confirmed_body"`
	}
	if err := json.Unmarshal([]byte(contractJSON), &legacyConfirmation); err != nil {
		return issue, 0, err
	}
	if legacyConfirmation.Body != "" {
		issue.IssueContract.ConfirmedSections = config.IssueContractSectionDigests(legacyConfirmation.Body)
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
	issue = scope.reporterIssue(issue)
	issue.Actor.DisplayName = displayName
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
	if closedAt.Valid {
		at, err := parseTimeValue(closedAt.String)
		if err != nil {
			return issue, 0, err
		}
		issue.ClosedAt = &at
	}
	if listChange.Valid {
		if err := json.Unmarshal([]byte(listChange.String), &issue.ListChange); err != nil {
			return issue, 0, err
		}
	}
	if pullRequest.Valid {
		var reference tracker.ChangeExternalReference
		if err := json.Unmarshal([]byte(pullRequest.String), &reference); err != nil {
			return issue, 0, err
		}
		if number := externalPullRequestNumber(reference); number > 0 {
			issue.PullRequest = &tracker.NativePullRequest{Number: number, URL: reference.URL}
		}
	}
	if priority.Valid {
		value := int(priority.Int64)
		issue.Priority = &value
	}
	if err := readNativeDependencyDetails(ctx, query, scope, &issue); err != nil {
		return issue, internalID, err
	}
	delivery, associated, err := readReleaseDelivery(ctx, query, scope, string(issue.WorkItemID))
	if associated {
		issue.ReleaseDelivery = &delivery
	}
	return issue, internalID, err
}

func (s *Service) getNativeIssue(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	if view := c.QueryParam("view"); view != "" {
		params, err := nativeReadQuery(c)
		if err != nil || view != "recovery" || validateNativeQuery(params, "view", "projection") != nil {
			return s.nativeAPIError(c, nativeInvalid("view supports recovery"))
		}
		if params.Has("projection") || c.Request().Header.Get("X-Detent-Recovery-Projection") != "" {
			if params.Get("projection") != "bounded" && c.Request().Header.Get("X-Detent-Recovery-Projection") != "bounded" || params.Has("limit") || params.Has("cursor") {
				return s.nativeAPIError(c, nativeInvalid("Recovery projection supports bounded"))
			}
			result, err := s.readNativeRecovery(ctx, scope, c.Param("item"), true)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			result.Issue = s.nativeIssueResponse(result.Issue)
			if err := s.fitNativeRecovery(ctx, scope, c.Request().URL.EscapedPath(), &result, c.Request().Header.Get("X-Detent-Recovery-Body") == "fragments"); err != nil {
				return s.nativeAPIError(c, err)
			}
			return c.JSON(http.StatusOK, result)
		}
		if params.Has("limit") || params.Has("cursor") {
			result, err := s.readNativeRecoveryPage(ctx, scope, c.Param("item"), c.Request().URL.EscapedPath(), params)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			return c.JSON(http.StatusOK, result)
		}
		result, err := s.readNativeRecovery(ctx, scope, c.Param("item"))
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		result.Issue = s.nativeIssueResponse(result.Issue)
		return c.JSON(http.StatusOK, result)
	}
	issue, _, err := readNativeIssue(ctx, s.database.reader, scope, c.Param("item"))
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
		if issue.Change, err = readNativeIssueChange(ctx, s.database.reader, scope, string(issue.WorkItemID)); err != nil {
			return s.nativeAPIError(c, err)
		}
		if issue.Change.Number > 0 && issue.Change.URL != "" {
			issue.PullRequest = &tracker.NativePullRequest{Number: issue.Change.Number, URL: issue.Change.URL}
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
		return nativePublicInvalid("Title must contain 1 to 500 bytes and body at most 256 KiB")
	}
	if priority != nil && (*priority < 0 || *priority > 3) {
		return nativePublicInvalid("Priority must be between 0 and 3")
	}
	for _, values := range [][]string{labels, assignees} {
		if len(values) > 100 {
			return nativePublicInvalid("At most 100 labels or assignees are supported")
		}
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 200 {
				return nativePublicInvalid("Labels and assignees must contain 1 to 200 bytes")
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
		return nativePublicInvalid("Imported provenance requires an operator")
	}
	if provenance.Provider != "github" && provenance.Provider != "local_sqlite" && provenance.Provider != "github_local" {
		return nativePublicInvalid("Import source is invalid")
	}
	return validateSourceProvenance(provenance)
}

func validateImportProvenance(provenance *tracker.Provenance) error {
	if provenance.Provider != "github" {
		return nativeInvalid("GitHub import source is invalid")
	}
	return validateSourceProvenance(provenance)
}

func validateSourceProvenance(provenance *tracker.Provenance) error {
	if strings.TrimSpace(provenance.ExternalID) == "" || len(provenance.ExternalID) > 200 || strings.TrimSpace(provenance.AuthorID) == "" || len(provenance.AuthorID) > 200 || len(provenance.AuthorDisplayName) > 200 {
		return nativePublicInvalid("Import source and author are invalid")
	}
	if provenance.CreatedAt.IsZero() || provenance.UpdatedAt.Before(provenance.CreatedAt) || provenance.ObservedAt.Before(provenance.UpdatedAt) {
		return nativePublicInvalid("Import timestamps are invalid")
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
	var challenge duplicateChallenge
	if err := json.Unmarshal(result, &challenge); err != nil {
		return s.nativeAPIError(c, err)
	}
	if challenge.ChallengeToken != "" {
		return c.JSON(http.StatusConflict, struct {
			apiErrorResponse
			duplicateChallenge
		}{apiErrorResponse: apiErrorResponse{Code: "possible_duplicates", Message: "Possible duplicate issues found; use file_issue to confirm whether this is new"}, duplicateChallenge: challenge})
	}
	return c.JSONBlob(http.StatusOK, result)
}

func validateNativeIssueType(value string, required bool) error {
	if value == "" && !required || value == "bug" || value == "change_request" {
		return nil
	}
	return &nativeError{Code: "invalid_request", Message: "type must be bug or change_request", status: http.StatusUnprocessableEntity, publicMessage: true}
}

func validateNativeReportContext(report *tracker.ReportContext) error {
	if report == nil || report.Validate() == nil {
		return nil
	}
	return &nativeError{Code: "invalid_request", Message: tracker.ErrInvalidReportContext.Error(), status: http.StatusUnprocessableEntity, publicMessage: true}
}

func reporterSectionsMissing(missing []string, report *tracker.ReportContext) []string {
	if !report.DescribesEnvironment() {
		return missing
	}
	return slices.DeleteFunc(missing, func(section string) bool {
		return strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(section), ":"), "environment")
	})
}

func validateNativeIssueDraft(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue) (tracker.NativeProject, error) {
	if err := validateNativeIssueType(request.Type, scope.credential.HostedRole == "reporter"); err != nil {
		return tracker.NativeProject{}, err
	}
	if err := validateNativeReportContext(request.ReportContext); err != nil {
		return tracker.NativeProject{}, err
	}
	if scope.credential.HostedRole == "reporter" {
		contracts, err := nativeReporterContracts(ctx, tx, scope)
		if err != nil {
			return tracker.NativeProject{}, err
		}
		contract := config.IssueContract{Sections: contracts[request.Type]}
		if missing := reporterSectionsMissing(contract.Evaluate(connector.Issue{Description: request.Body}).Missing, request.ReportContext); len(missing) > 0 {
			return tracker.NativeProject{}, &nativeError{Code: "invalid_request", Message: "Add or complete these required Reporter sections: " + strings.Join(missing, ", "), status: http.StatusUnprocessableEntity, publicMessage: true}
		}
	}
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
		return tracker.NativeProject{}, nativePublicInvalid("Compatibility project content is externally owned")
	}
	return project, nil
}

func createNativeIssueTx(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, now time.Time) (tracker.NativeIssue, error) {
	return createNativeIssue(ctx, tx, scope, request, now, false)
}

func defaultNativeIssueState(states []tracker.NativeState) string {
	for _, state := range states {
		if state.Name != "Triage" {
			return state.Name
		}
	}
	return ""
}

func validateNativeIssueIntake(ctx context.Context, q nativeQueryer, scope nativeScope, request tracker.CreateIssue) error {
	if request.State != "Triage" || request.GitHubIssueURL != "" || scope.credential.HostedRole == "reporter" {
		return nil
	}
	project, err := readNativeProject(ctx, q, scope)
	if err != nil {
		return err
	}
	for _, state := range project.States {
		if state.Name == "Backlog" && (!state.OperatorOnly || scope.credential.Scope != apiScopeWorker) {
			return nativePublicInvalid("Triage is reserved for linked GitHub issue intake; use Backlog for drafts")
		}
	}
	return nativePublicInvalid("Triage is reserved for linked GitHub issue intake")
}

func validateNativeIssueState(scope nativeScope, project tracker.NativeProject, state string, machineIntake bool) error {
	for _, candidate := range project.States {
		if candidate.Name == state {
			if candidate.OperatorOnly && scope.credential.Scope == apiScopeWorker && !machineIntake {
				return nativePublicInvalid("Workflow target requires an operator")
			}
			return nil
		}
	}
	return nativePublicInvalid("Workflow state does not exist")
}

func nativeIssueDraft(ctx context.Context, tx *sql.Tx, scope nativeScope, project tracker.NativeProject, request tracker.CreateIssue, now time.Time, machineIntake bool) tracker.NativeIssue {
	if request.State == "" {
		request.State = defaultNativeIssueState(project.States)
	}
	if scope.credential.HostedRole == "reporter" {
		request.State = "Triage"
	}
	issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: tracker.NativeWorkItemID(newNativeID("wi")), Revision: 1, Profile: "native"},
		Type: request.Type, ReportContext: request.ReportContext, Title: request.Title, Body: request.Body, State: request.State, Priority: request.Priority, Labels: request.Labels, Assignees: request.Assignees,
		Actor: scope.actor(), Provenance: request.Provenance, CreatedAt: now, UpdatedAt: now, LastActivityAt: now, Dependencies: []tracker.NativeWorkItemID{}}
	issue.IssueContract = &issuecontract.State{Exempt: workspaceLabelAllowed(ctx)}
	if (machineIntake || scope.scheduledRepair != nil && scope.scheduledRepair.importURL != "") && issue.Provenance == nil {
		applyScheduledRepairContract(ctx, tx, scope, &issue, now)
	}
	_, machineOrigin := issueorigin.Parse(issue.Body)
	if scope.credential.HostedRole != "reporter" && issue.Actor.Kind == "human" && !machineOrigin && !machineIntake && issue.Provenance == nil {
		issue.IssueContract.ConfirmedSections = config.IssueContractSectionDigests(issue.Body)
	}
	return issue
}

func validateNativeIssueFiling(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, now time.Time) error {
	if err := validateNativeIssueIntake(ctx, tx, scope, request); err != nil {
		return err
	}
	project, err := validateNativeIssueDraft(ctx, tx, scope, request)
	if err != nil {
		return err
	}
	issue := nativeIssueDraft(ctx, tx, scope, project, request, now, false)
	if err := validateNativeIssueState(scope, project, issue.State, false); err != nil {
		return err
	}
	return requireNativeIssueContractAtFiling(ctx, tx, scope, issue)
}

func createNativeIssue(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, now time.Time, machineIntake bool) (tracker.NativeIssue, error) {
	if err := validateNativeIssueIntake(ctx, tx, scope, request); err != nil {
		return tracker.NativeIssue{}, err
	}
	if request.GitHubIssueURL != "" {
		return createLinkedIssueTx(ctx, tx, scope, request, now)
	}
	if scope.credential.HostedRole == "reporter" {
		request.State = "Triage"
		return createNativeIssueDraft(ctx, tx, scope, request, now, machineIntake, false)
	}
	return createNativeIssueDraft(ctx, tx, scope, request, now, machineIntake, false)
}

func createNativeIssueDraft(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, now time.Time, machineIntake, linkedIntake bool) (tracker.NativeIssue, error) {
	project, err := validateNativeIssueDraft(ctx, tx, scope, request)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	if request.State == "" {
		request.State = defaultNativeIssueState(project.States)
	}
	var sourceKey any
	if request.Provenance != nil {
		sourceKey = request.Provenance.Provider + ":" + request.Provenance.ExternalID
		var existing string
		err := tx.QueryRowContext(ctx, "SELECT native_id FROM issues WHERE organization_id = ? AND project_id = ? AND native_source_key = ?", scope.organization, scope.project, sourceKey).Scan(&existing)
		if err == nil {
			issue, _, err := readNativeIssue(ctx, tx, scope, existing)
			if err != nil {
				return issue, err
			}
			return issue, requireNativeIssueContractAtFiling(ctx, tx, scope, issue)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return tracker.NativeIssue{}, err
		}
	}
	issue := nativeIssueDraft(ctx, tx, scope, project, request, now, machineIntake)
	if err := validateNativeIssueState(scope, project, issue.State, machineIntake); err != nil {
		return tracker.NativeIssue{}, err
	}
	var workflowID int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = ?", scope.project, issue.State).Scan(&workflowID); err != nil {
		return tracker.NativeIssue{}, err
	}
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
	if !linkedIntake {
		if err := requireNativeIssueContractAtFiling(ctx, tx, scope, issue); err != nil {
			return tracker.NativeIssue{}, err
		}
	}
	contractJSON, err := marshalNative(issue.IssueContract)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	var reportContext sql.NullString
	if issue.ReportContext != nil {
		if reportContext.String, err = marshalNative(issue.ReportContext); err != nil {
			return tracker.NativeIssue{}, err
		}
		reportContext.Valid = true
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO issues (native_id, organization_id, project_id, number, workflow_state_id, title, body, url, github_number, github_state, labels_json, assignees_json, source_version, source_updated_at, synchronized_at, created_at, updated_at, author_login, actor_json, provenance_json, native_source_key, native_created_at, native_updated_at, issue_contract_json, issue_type, report_context_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?, ?, '', '', '', ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?)`, issue.WorkItemID, scope.organization, scope.project, issue.Number, workflowID, issue.Title, issue.Body, importedURL, sourceNumber, labels, assignees, formatHubTime(now), formatHubTime(now), author, actor, provenance, sourceKey, formatHubTime(now), formatHubTime(now), contractJSON, issue.Type, reportContext)
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
		if !value.Deleted {
			if err := syncCloudAttachmentReferences(ctx, tx, scope, workItemID, value.ID, value.Body); err != nil {
				return err
			}
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
	if eventType == "workflow.transitioned" && data.FromState != issue.State || slices.Contains(data.Fields, "body") || eventType == "github.imported" {
		if err := requireNativeIssueContractAtFiling(ctx, tx, scope, issue); err != nil {
			return issue, err
		}
	}
	if err := tx.QueryRowContext(ctx, "SELECT terminal FROM workflow_states WHERE project_id = ? AND detent_state = ?", scope.project, issue.State).Scan(&issue.Terminal); err != nil {
		return issue, err
	}
	issue.Revision++
	issue.UpdatedAt = now
	if eventType == "workflow.transitioned" && data.FromState != issue.State {
		awaitsFiler := issue.Profile == "native" && nativeDoneState(issue.State) && nativeHumanFiled(issue)
		if awaitsFiler {
			workspace, err := nativeWorkspaceItem(ctx, tx, scope, issue.WorkItemID)
			if err != nil {
				return issue, err
			}
			awaitsFiler = !workspace
		}
		switch {
		case awaitsFiler:
			issue.Verification = &tracker.NativeVerification{State: tracker.VerificationPending}
			data.Verification = tracker.VerificationPending
		case nativeDoneState(data.FromState) && !nativeDoneState(issue.State):
			issue.Verification = nil
		}
	}
	verificationState, verifiedBy, verifiedAt := any(nil), any(nil), any(nil)
	if issue.Verification != nil {
		verificationState = issue.Verification.State
		if issue.Verification.VerifiedBy != nil {
			raw, err := marshalNative(issue.Verification.VerifiedBy)
			if err != nil {
				return issue, err
			}
			verifiedBy = raw
		}
		if issue.Verification.VerifiedAt != nil {
			verifiedAt = formatHubTime(*issue.Verification.VerifiedAt)
		}
	}
	labels, err := marshalNative(issue.Labels)
	if err != nil {
		return issue, err
	}
	assignees, err := marshalNative(issue.Assignees)
	if err != nil {
		return issue, err
	}
	contractJSON, err := marshalNative(issue.IssueContract)
	if err != nil {
		return issue, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE issues SET issue_type = NULLIF(?, ''), verification_state = ?, verified_by_json = ?, verified_at = ?, issue_contract_json = ?, title = ?, body = ?, labels_json = ?, assignees_json = ?, revision = ?, updated_at = ?, native_updated_at = ?, archived = ?,
workflow_state_id = (SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = ?)
WHERE organization_id = ? AND project_id = ? AND native_id = ?`, issue.Type, verificationState, verifiedBy, verifiedAt, contractJSON, issue.Title, issue.Body, labels, assignees, issue.Revision, formatHubTime(now), formatHubTime(now), issue.Archived, scope.project, issue.State, scope.organization, scope.project, issue.WorkItemID)
	if err != nil {
		return issue, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE queue_entries SET state = ?, priority_override = ?, updated_at = ? WHERE issue_id = (SELECT id FROM issues WHERE organization_id = ? AND project_id = ? AND native_id = ?)`, issue.State, issue.Priority, formatHubTime(now), scope.organization, scope.project, issue.WorkItemID); err != nil {
		return issue, err
	}
	if eventType == "workflow.transitioned" && data.FromState != issue.State && issue.State == "Cancelled" {
		if err := enqueueLinkedSourceSummary(ctx, tx, scope, issue, "cancelled:"+strconv.FormatInt(int64(issue.Revision), 10), true, now); err != nil {
			return issue, err
		}
	}
	data.Revision = issue.Revision
	if eventType == "workflow.transitioned" {
		if err := recordLandingTransition(ctx, tx, scope, issue, data.FromState); err != nil {
			return issue, err
		}
	}
	issue, _, err = readNativeIssue(ctx, tx, scope, string(issue.WorkItemID))
	if err != nil {
		return issue, err
	}
	err = recordNativeChange(ctx, tx, scope, issue, string(issue.WorkItemID), issue.Revision, eventType, data, now)
	return issue, err
}

func nativeDoneState(state string) bool {
	return strings.EqualFold(state, "Done")
}

func nativeWorkspaceItem(ctx context.Context, tx *sql.Tx, scope nativeScope, item tracker.NativeWorkItemID) (bool, error) {
	var found bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM workspace_items WHERE work_item_id = ? AND organization_id = ? AND project_id = ?)", item, scope.organization, scope.project).Scan(&found)
	return found, err
}

func nativeHumanFiled(issue tracker.NativeIssue) bool {
	_, machineOrigin := issueorigin.Parse(issue.Body)
	return issue.Actor.Kind == "human" && issue.Actor.PrincipalID != "" && issue.Provenance == nil && !machineOrigin
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
	scope.action = operatortool.MoveItem
	scope.actionItem, scope.actionTarget = item, request.State
	recordedRecovery := request.Reason == "dependency_ready" && request.BlockerAttemptID != ""
	signal, contractHold := workpad.SignalFromComment(request.ReasonDetail, "", "")
	contractHold = contractHold && signal != nil && signal.Invalid == nil && signal.Fields["issue_contract_return_state"] != ""
	contractRecovery := strings.ReplaceAll(request.ReasonDetail, " ", "_") == "recorded_blocker_recovery" && !recordedRecovery
	options := nativeCommandOptions{OperationID: nativeOperation(scope, "POST", "/work-items/"+item+"/workflow"), Item: item, RequireLease: !recordedRecovery && !contractHold && !contractRecovery, Feature: "collaboration"}
	var heldLease tracker.LeaseID
	result, err := s.executeNativeIssueMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		heldLease = ""
		issue, internalID, err := readNativeIssue(ctx, tx, scope, item)
		if err != nil {
			return nil, err
		}
		if err := requireNativeEdit(issue, request.ExpectedRevision); err != nil {
			return nil, err
		}
		if scope.actor().Kind == "human" && strings.EqualFold(request.State, "Blocked") && !strings.EqualFold(issue.State, "Blocked") {
			lease, found, err := readUnreleasedLease(ctx, tx, internalID)
			if err != nil {
				return nil, err
			}
			if found && lease.session.ExpiresAt.After(now) {
				if err := releaseLeaseRow(ctx, tx, lease, "cancelled", now); err != nil {
					return nil, err
				}
				if err := cancelNativeLeaseAttempt(ctx, tx, scope, lease, now); err != nil {
					return nil, err
				}
				heldLease = lease.session.ID
			}
		}
		if contractHold || contractRecovery {
			contract, err := nativeIssueContract(ctx, tx, scope)
			if err != nil {
				return nil, err
			}
			evaluation := contract.Evaluate(nativeContractIssue(issue))
			if contractRecovery {
				if issue.IssueContract.ReturnState != request.State || issue.IssueContract.HumanAction != "" || !evaluation.Satisfied() {
					return nil, nativeInvalid("Human confirmation is required to clear the issue contract hold")
				}
			} else {
				if issue.IssueContract.Exempt || evaluation.Satisfied() || signal.Status != workpad.StatusBlocked || signal.HumanAction == "" {
					return nil, nativeInvalid("Issue contract hold does not match the issue")
				}
				cfg, err := nativeIssueContractConfig(ctx, tx, scope)
				if err != nil {
					return nil, err
				}
				returnState := signal.Fields["issue_contract_return_state"]
				admissionTarget := cfg.BacklogAdmission.TargetState
				if admissionTarget == "" {
					admissionTarget = "Todo"
				}
				if returnState != admissionTarget || issue.State != admissionTarget && !slices.Contains(cfg.BacklogAdmission.Sources.States, issue.State) {
					return nil, nativeInvalid("Issue contract hold must return to the configured admission lane")
				}
				lease, found, err := readUnreleasedLease(ctx, tx, internalID)
				if err != nil {
					return nil, err
				}
				if found && lease.session.ExpiresAt.After(now) {
					return nil, tracker.ErrLeaseConflict
				}
				if request.State != cfg.Agent.StopRun.TargetState {
					return nil, nativeInvalid("Issue contract failure destination does not match the workflow")
				}
			}
		}
		if recordedRecovery {
			if err := validateNativeRecordedRecovery(ctx, tx, scope, issue, request, now); err != nil {
				return nil, err
			}
		}
		if !slices.Contains([]string{"user_requested", "worker_progress", "dependency_ready", string(gate.ReasonValidatorRework)}, request.Reason) {
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
		if signal, ok := workpad.SignalFromComment(request.ReasonDetail, "", ""); ok && signal != nil && signal.Invalid == nil && signal.Fields["issue_contract_return_state"] != "" {
			if signal.Status != workpad.StatusBlocked || signal.HumanAction == "" {
				return nil, nativeInvalid("Issue contract hold requires a human action")
			}
			issue.IssueContract.HumanAction = signal.HumanAction
			issue.IssueContract.ReturnState = signal.Fields["issue_contract_return_state"]
			issue.IssueContract.RecordedAt = now
		}
		if issue.IssueContract.ReturnState == request.State && issue.IssueContract.HumanAction == "" {
			issue.IssueContract.ReturnState = ""
		}
		issue.State = request.State
		return persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{BlockerAttemptID: request.BlockerAttemptID, FromState: from, ToState: issue.State, Reason: request.Reason, ReasonDetail: strings.TrimSpace(request.ReasonDetail)}, now)
	})
	if err == nil {
		s.wakeSpriteRunnersAfter(scope, result)
		if heldLease != "" && s.conversations != nil {
			s.conversations.leaseReleased(ctx, heldLease)
		}
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
