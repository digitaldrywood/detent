package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) nativeIssueJSON(ctx context.Context, scope hubscope.NativeScope, body json.RawMessage) (json.RawMessage, error) {
	var challenge duplicateChallenge
	if err := json.Unmarshal(body, &challenge); err != nil {
		return nil, err
	}
	if challenge.ChallengeToken != "" {
		if scope.Credential.HostedRole == "reporter" {
			visible := []hubscope.PossibleDuplicate{}
			for _, match := range challenge.PossibleDuplicates {
				if err := s.requireReporterWorkItemRecord(ctx, scope, string(match.WorkItemID)); errors.Is(err, sql.ErrNoRows) || hubapi.IsNativeNotFound(err) {
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
			path, err := s.nativeIssueRecordBrowserPath(ctx, scope, string(match.WorkItemID))
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
	if !scope.ReporterOwns(issue.Actor) {
		return nil, hubapi.NativeNotFound()
	}
	path, err := s.nativeIssueRecordBrowserPath(ctx, scope, string(issue.WorkItemID))
	if err != nil {
		issue.WebURL = ""
		issue.OmittedFields = append(issue.OmittedFields, "web_url")
	} else {
		issue.WebURL = path
	}
	return json.Marshal(s.nativeIssueResponse(scope.ReporterIssue(issue)))
}

func nativeActorDisplayName(principal, organization string) string {
	return `COALESCE((SELECT COALESCE(
 (SELECT m.email FROM hosted_members m JOIN hosted_tenant h ON h.organization_id = ` + organization + ` WHERE m.principal_id = t.id), t.name)
 FROM api_tokens t WHERE t.id = ` + principal + `), '')`
}

func readNativeIssueProjection(ctx context.Context, query hubapi.NativeQueryer, scope hubscope.NativeScope, id string, compact bool) (tracker.NativeIssue, tracker.WorkItemID, error) {
	var issue tracker.NativeIssue
	var internalID tracker.WorkItemID
	var labels, assignees, actor, displayName, created, updated, activity, externalID, contractJSON string
	var sourceAuthor, sourceCreated, sourceUpdated, sourceObserved string
	var repositoryOwner, repositoryName, importedURL, organizationSlug, projectSlug string
	var sourceNumber int
	var provenance, closedAt, issueType, reportContext, verificationState, verifiedBy, verifiedAt sql.NullString
	var listChangeID, listBranch, listHead, pullRequestID, pullRequestURL sql.NullString
	var landingPending bool
	var priority sql.NullInt64
	var actorKind, actorPrincipal sql.NullString
	var legacyChange bool
	bodyColumn := "i.body"
	if compact {
		bodyColumn = "''"
	}
	visibility, args := scope.IssueVisibilitySQL("i")
	if scope.Credential.HostedRole == "reporter" {
		visibility = "(" + visibility + " OR (i.actor_principal_id IS NULL AND ?<>''))"
		args = append(args, scope.Actor().PrincipalID)
	}
	args = append([]any{scope.Organization, scope.Project, id}, args...)
	err := query.QueryRowContext(ctx, `SELECT o.slug, p.slug, i.id, i.native_id, i.organization_id, i.project_id, i.number, i.revision, p.profile,
 i.issue_type, i.report_context_json, i.verification_state, i.verified_by_json, i.verified_at, i.title, `+bodyColumn+`, COALESCE(ws.detent_state, ''), COALESCE(ws.terminal, 0), q.priority_override, i.labels_json, i.assignees_json,
 i.actor_json,i.actor_kind,i.actor_principal_id, `+nativeActorDisplayName(hubscope.NativeIssuePrincipal("i"), "i.organization_id")+`, i.issue_contract_json, i.provenance_json, i.native_created_at, i.native_updated_at, i.last_activity_at, COALESCE(i.github_node_id, ''),
 i.author_login, i.created_at, i.source_updated_at, i.synchronized_at, p.require_dependencies = 0, i.archived,
 COALESCE(r.github_owner, ''), COALESCE(r.github_name, ''), COALESCE(i.github_number, 0), i.url,
 CASE WHEN ws.terminal = 1 THEN i.terminal_entered_at ELSE NULL END,
 c.id, CASE WHEN c.landed_version_id = c.claim_version_id THEN COALESCE(c.landed_base_ref, '') ELSE '' END,
 CASE WHEN c.landed_version_id = c.claim_version_id THEN COALESCE(c.landed_merge_sha, '') ELSE COALESCE(v.head_sha, '') END,
 COALESCE((`+nativePendingLandingClause+`),0), pr.external_id, pr.external_url,
 EXISTS (SELECT 1 FROM change_requests c JOIN change_issue_links cl ON cl.change_id=c.id LEFT JOIN change_versions cv ON cv.change_id=c.id WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND cl.work_item_id=i.native_id AND (c.claim_version_id IS NULL OR c.created_at IS NULL OR (cv.id IS NOT NULL AND cv.head_sha IS NULL)))
FROM issues i JOIN organizations o ON o.id=i.organization_id JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
LEFT JOIN repositories r ON r.id = i.repository_id
LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
LEFT JOIN queue_entries q ON q.id = (SELECT id FROM queue_entries WHERE issue_id = i.id ORDER BY id LIMIT 1)
LEFT JOIN change_requests c ON c.id=(SELECT c.id FROM change_requests c JOIN change_issue_links l ON l.change_id=c.id WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND l.work_item_id=i.native_id ORDER BY c.rowid DESC LIMIT 1)
LEFT JOIN change_versions v ON v.id=c.claim_version_id AND v.change_id=c.id
LEFT JOIN change_versions pr ON pr.id=(SELECT latest.id FROM change_versions latest JOIN change_requests pc ON pc.id=latest.change_id JOIN change_issue_links l ON l.change_id=pc.id WHERE pc.organization_id=i.organization_id AND pc.project_id=i.project_id AND l.work_item_id=i.native_id AND latest.external_provider='github' AND latest.external_url<>'' ORDER BY pc.rowid DESC,latest.number DESC LIMIT 1)
WHERE i.organization_id = ? AND i.project_id = ? AND i.native_id = ? AND (`+visibility+`)`, args...).Scan(
		&organizationSlug, &projectSlug, &internalID, &issue.WorkItemID, &issue.OrganizationID, &issue.ProjectID, &issue.Number, &issue.Revision, &issue.Profile,
		&issueType, &reportContext, &verificationState, &verifiedBy, &verifiedAt, &issue.Title, &issue.Body, &issue.State, &issue.Terminal, &priority, &labels, &assignees, &actor, &actorKind, &actorPrincipal, &displayName, &contractJSON, &provenance, &created, &updated, &activity, &externalID,
		&sourceAuthor, &sourceCreated, &sourceUpdated, &sourceObserved, &issue.IgnoreDependencies, &issue.Archived,
		&repositoryOwner, &repositoryName, &sourceNumber, &importedURL, &closedAt, &listChangeID, &listBranch, &listHead, &landingPending, &pullRequestID, &pullRequestURL, &legacyChange)
	if err != nil {
		return issue, 0, err
	}
	if err := json.Unmarshal([]byte(actor), &issue.Actor); err != nil {
		return issue, 0, err
	}
	if actorKind.Valid {
		issue.Actor.Kind = actorKind.String
	}
	if actorPrincipal.Valid {
		issue.Actor.PrincipalID = actorPrincipal.String
	}
	if !scope.ReporterOwns(issue.Actor) {
		return tracker.NativeIssue{}, 0, sql.ErrNoRows
	}
	if !actorPrincipal.Valid {
		if err := query.QueryRowContext(ctx, "SELECT "+nativeActorDisplayName("?", "?"), scope.Organization, issue.Actor.PrincipalID).Scan(&displayName); err != nil {
			return issue, 0, err
		}
	}
	issue.WebURL = hubrecords.IssueBrowserPath(organizationSlug, projectSlug, issue.Number)
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
			at, err := hubapi.ParseTimeValue(verifiedAt.String)
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

	issue = scope.ReporterIssue(issue)
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
		if issue.Provenance.CreatedAt, err = hubapi.ParseTimeValue(sourceCreated); err != nil {
			return issue, 0, err
		}
		if issue.Provenance.UpdatedAt, err = hubapi.ParseTimeValue(sourceUpdated); err != nil {
			return issue, 0, err
		}
		if issue.Provenance.ObservedAt, err = hubapi.ParseTimeValue(sourceObserved); err != nil {
			return issue, 0, err
		}
	}
	if issue.CreatedAt, err = hubapi.ParseTimeValue(created); err != nil {
		return issue, 0, err
	}
	if issue.UpdatedAt, err = hubapi.ParseTimeValue(updated); err != nil {
		return issue, 0, err
	}
	if issue.LastActivityAt, err = hubapi.ParseTimeValue(activity); err != nil {
		return issue, 0, err
	}
	if closedAt.Valid {
		at, err := hubapi.ParseTimeValue(closedAt.String)
		if err != nil {
			return issue, 0, err
		}
		issue.ClosedAt = &at
	}
	if listChangeID.Valid {
		issue.ListChange = &tracker.NativeListChange{Branch: listBranch.String, HeadSHA: listHead.String, LandingPending: landingPending}
	}

	if pullRequestURL.Valid {
		reference := tracker.ChangeExternalReference{Provider: "github", ID: pullRequestID.String, URL: pullRequestURL.String}
		if number := externalPullRequestNumber(reference); number > 0 {
			issue.PullRequest = &tracker.NativePullRequest{Number: number, URL: reference.URL}
		}
	}
	if legacyChange {
		if err := readLegacyIssueChangeSideData(ctx, query, scope, id, &issue); err != nil {
			return issue, 0, err
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
	params, err := nativeReadQuery(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if params.Get("view") == "recovery" {
		return s.getNativeRecoveryTenant(c, params)
	}
	issue, err := s.workItemResource(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"), params)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, issue)
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
			return false, hubapi.NativePublicInvalid("include supports change")
		}
	}
	return included, nil
}

func validateNativeProvenance(scope hubscope.NativeScope, provenance *tracker.Provenance) error {
	if provenance == nil {
		return nil
	}
	if scope.SourceActor != nil && scope.SourceActor.Agent == "progress-check" && provenance.Provider == "progress-check" && strings.TrimSpace(provenance.ExternalID) != "" && len(provenance.ExternalID) <= 200 {
		return nil
	}
	if scope.Credential.Scope == hubapi.APIScopeWorker {
		return hubapi.NativePublicInvalid("Imported provenance requires an operator")
	}
	if provenance.Provider != "github" && provenance.Provider != "local_sqlite" && provenance.Provider != "github_local" {
		return hubapi.NativePublicInvalid("Import source is invalid")
	}
	return validateSourceProvenance(provenance)
}

func validateImportProvenance(provenance *tracker.Provenance) error {
	if provenance.Provider != "github" {
		return hubapi.NativePublicInvalid("GitHub import source is invalid")
	}
	return validateSourceProvenance(provenance)
}

func validateSourceProvenance(provenance *tracker.Provenance) error {
	if strings.TrimSpace(provenance.ExternalID) == "" || len(provenance.ExternalID) > 200 || strings.TrimSpace(provenance.AuthorID) == "" || len(provenance.AuthorID) > 200 || len(provenance.AuthorDisplayName) > 200 {
		return hubapi.NativePublicInvalid("Import source and author are invalid")
	}
	if provenance.CreatedAt.IsZero() || provenance.UpdatedAt.Before(provenance.CreatedAt) || provenance.ObservedAt.Before(provenance.UpdatedAt) {
		return hubapi.NativePublicInvalid("Import timestamps are invalid")
	}
	return nil
}

func (s *Service) createNativeIssue(c echo.Context) error {
	var request tracker.CreateIssue
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	result, err := s.createNativeIssueCommand(c.Request().Context(), hubscope.NativeRequestScope(c), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var challenge duplicateChallenge
	if err := json.Unmarshal(result, &challenge); err != nil {
		return s.nativeAPIError(c, err)
	}
	if challenge.ChallengeToken != "" {
		return s.refusedAPIError(c, http.StatusConflict, "possible_duplicates", "Possible duplicate issues found; use file_issue to confirm whether this is new", struct {
			hubapi.APIErrorResponse
			duplicateChallenge
		}{APIErrorResponse: hubapi.APIErrorResponse{Code: "possible_duplicates", Message: "Possible duplicate issues found; use file_issue to confirm whether this is new"}, duplicateChallenge: challenge})
	}
	return c.JSONBlob(http.StatusOK, result)
}

func validateNativeReportContext(report *tracker.ReportContext) error {
	if report == nil || report.Validate() == nil {
		return nil
	}
	return &hubapi.NativeError{Code: "invalid_request", Message: tracker.ErrInvalidReportContext.Error(), Status: http.StatusUnprocessableEntity, PublicMessage: true}
}

func reporterSectionsMissing(missing []string, report *tracker.ReportContext) []string {
	if !report.DescribesEnvironment() {
		return missing
	}
	return slices.DeleteFunc(missing, func(section string) bool {
		return strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(section), ":"), "environment")
	})
}

func defaultNativeIssueState(states []tracker.NativeState) string {
	for _, state := range states {
		if state.Name != "Triage" {
			return state.Name
		}
	}
	return ""
}

func validateNativeIssueState(scope hubscope.NativeScope, project tracker.NativeProject, state string, machineIntake bool) error {
	for _, candidate := range project.States {
		if candidate.Name == state {
			if candidate.OperatorOnly && scope.Credential.Scope == hubapi.APIScopeWorker && !machineIntake {
				return hubapi.NativePublicInvalid("Workflow target requires an operator")
			}
			return nil
		}
	}
	return hubapi.NativePublicInvalid("Workflow state does not exist")
}

func nativeDoneState(state string) bool {
	return strings.EqualFold(state, "Done")
}

func nativeHumanFiled(issue tracker.NativeIssue) bool {
	_, machineOrigin := issueorigin.Parse(issue.Body)
	return issue.Actor.Kind == "human" && issue.Actor.PrincipalID != "" && issue.Provenance == nil && !machineOrigin
}

func (s *Service) updateNativeIssue(c echo.Context) error {
	var request tracker.UpdateIssue
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	result, err := s.updateNativeIssueCommand(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func (s *Service) transitionNativeIssue(c echo.Context) error {
	var request tracker.Transition
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	result, err := s.transitionNativeIssueCommand(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func (s *Service) transitionNativeIssueCommand(ctx context.Context, scope hubscope.NativeScope, item string, request tracker.Transition) (json.RawMessage, error) {
	return s.transitionNativeIssueCommandWithChat(ctx, scope, item, request, "")
}

func (s *Service) transitionNativeIssueCommandWithChat(ctx context.Context, scope hubscope.NativeScope, item string, request tracker.Transition, conversationID string) (json.RawMessage, error) {
	return s.transitionNativeIssueTenantFromChat(ctx, scope, item, request, conversationID)
}

func (s *Service) changeNativeDependency(c echo.Context) error {
	var request tracker.DependencyMutation
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	result, err := s.changeNativeDependencyCommand(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func (s *Service) requireReporterWorkItemRecord(ctx context.Context, scope hubscope.NativeScope, item string) error {
	row, _, err := s.readWorkItemRecord(ctx, scope, item)
	if err != nil {
		return err
	}
	if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
		return hubapi.NativeNotFound()
	}
	return nil
}

func (s *Service) nativeIssueRecordBrowserPath(ctx context.Context, scope hubscope.NativeScope, id string) (string, error) {
	var path string
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		item, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(id))
		if err != nil {
			return err
		}
		project, err := r.Project(ctx, item.ProjectID)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		organization, err := r.AccessOrganization(ctx)
		if err != nil {
			return err
		}
		path = hubrecords.IssueBrowserPath(organization.Slug, project.Slug, int(item.Number))
		return nil
	})
	return path, hubapi.ProjectRecordError(err)
}
