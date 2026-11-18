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

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
