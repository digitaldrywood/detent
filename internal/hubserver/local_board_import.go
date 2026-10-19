package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/localboard"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) importLocalBoard(c echo.Context) error {
	var request localboard.Request
	if err := decodeAPIJSON(c, &request); err != nil {
		var oversized *http.MaxBytesError
		if !errors.As(err, &oversized) {
			err = errors.New("invalid local board request")
		}
		return invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	if err := localboard.Validate(request.Record); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	if err := validateNativeContent(request.Record.Title, request.Record.Body, nil, nil, nil); err != nil {
		return s.nativeAPIError(c, err)
	}
	plan, _, err := planLocalBoardItem(c.Request().Context(), s.database.reader, scope, request.Record)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if request.DryRun || plan.Action == "unchanged" {
		return c.JSON(http.StatusOK, plan)
	}
	options := nativeCommandOptions{OperationID: nativeOperation(scope, "POST", "/local-board-import"), Feature: "collaboration"}
	command := tracker.Mutation{IdempotencyKey: newNativeID("import")}
	result, err := s.executeNativeMutation(c.Request().Context(), scope, options, command, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		return importLocalBoardItem(ctx, tx, scope, request.Record, now)
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func localBoardState(states []tracker.NativeState, source string) (string, bool, error) {
	for _, state := range states {
		if strings.EqualFold(strings.TrimSpace(source), state.Name) {
			return state.Name, false, nil
		}
	}
	for _, name := range []string{"Backlog", "Triage", ""} {
		for _, state := range states {
			if !state.Dispatchable && !state.Terminal && (name == "" || state.Name == name) {
				return state.Name, true, nil
			}
		}
	}
	return "", false, nativeInvalid("Local board import requires a nondispatchable nonterminal fallback state")
}

func planLocalBoardItem(ctx context.Context, q nativeQueryer, scope nativeScope, record localboard.Record) (localboard.Result, tracker.NativeIssue, error) {
	result := localboard.Result{SourceID: record.SourceID, SourceState: record.State, Action: "created", Comments: len(record.Comments), Dependencies: len(record.Dependencies)}
	project, err := readNativeProject(ctx, q, scope)
	if err != nil {
		return result, tracker.NativeIssue{}, err
	}
	if project.Profile != "native" {
		return result, tracker.NativeIssue{}, nativeInvalid("Local board import requires a native project")
	}
	result.State, result.UnknownLane, err = localBoardState(project.States, record.State)
	if err != nil {
		return result, tracker.NativeIssue{}, err
	}
	var id string
	err = q.QueryRowContext(ctx, `SELECT work_item_id FROM local_board_imports WHERE organization_id=? AND project_id=? AND source_key=?`, scope.organization, scope.project, record.Key).Scan(&id)
	if err == nil {
		issue, _, err := readNativeIssue(ctx, q, scope, id)
		result.Action, result.WorkItemID, result.State, result.Comments, result.Dependencies = "unchanged", issue.WorkItemID, issue.State, 0, 0
		return result, issue, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, tracker.NativeIssue{}, err
	}
	if record.Kind == "local_sqlite" {
		return result, tracker.NativeIssue{}, nil
	}
	integration, err := readProjectIntegration(ctx, q, scope)
	if err != nil {
		return result, tracker.NativeIssue{}, err
	}
	if err := validateLinkedIssueRepository(integration, record.Repository); err != nil {
		return result, tracker.NativeIssue{}, err
	}
	source := "https://github.com/" + strings.ToLower(record.Repository) + "/issues/"
	rows, err := q.QueryContext(ctx, `SELECT native_id FROM issues WHERE organization_id=? AND project_id=? AND (lower(native_source_key)=lower(?) OR (repository_id=? AND github_number=?))`, scope.organization, scope.project, "github:"+source+strconv.Itoa(record.Number), integration.RepositoryID, record.Number)
	if err != nil {
		return result, tracker.NativeIssue{}, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return result, tracker.NativeIssue{}, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return result, tracker.NativeIssue{}, err
	}
	if len(ids) != 1 {
		return result, tracker.NativeIssue{}, nativeInvalid("GitHub local item requires exactly one existing onboarding intake match")
	}
	issue, _, err := readNativeIssue(ctx, q, scope, ids[0])
	if err != nil {
		return result, issue, err
	}
	if issue.Archived {
		return result, issue, nativeInvalid("Local board import cannot change an archived native item")
	}
	if issue.State != "Triage" && (!issue.Terminal || issue.State != result.State) {
		return result, issue, nativeInvalid("GitHub local lane restoration requires an intake item still in Triage")
	}
	result.Action, result.WorkItemID = "updated", issue.WorkItemID
	return result, issue, nil
}

func importLocalBoardItem(ctx context.Context, tx *sql.Tx, scope nativeScope, record localboard.Record, now time.Time) (localboard.Result, error) {
	result, issue, err := planLocalBoardItem(ctx, tx, scope, record)
	if err != nil || result.Action == "unchanged" {
		return result, err
	}
	dependencies := []tracker.NativeWorkItemID{}
	for _, key := range record.Dependencies {
		var id tracker.NativeWorkItemID
		if err := tx.QueryRowContext(ctx, `SELECT work_item_id FROM local_board_imports WHERE organization_id=? AND project_id=? AND source_key=?`, scope.organization, scope.project, key).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return result, nativeInvalid("Import prerequisite items before their dependents")
			}
			return result, err
		}
		dependencies = append(dependencies, id)
	}
	provenance := &tracker.Provenance{Provider: record.Kind, ExternalID: record.Key, AuthorID: record.Author, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, ObservedAt: now}
	if record.Kind == "local_sqlite" {
		issue, err = createNativeIssueDraft(ctx, tx, scope, tracker.CreateIssue{Title: record.Title, Body: record.Body, State: result.State, Provenance: provenance}, now, false, true)
		if err != nil {
			return result, err
		}
	} else if issue.State != result.State {
		from := issue.State
		issue.State = result.State
		issue, err = persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: issue.State, Reason: "user_requested"}, now)
		if err != nil {
			return result, err
		}
	}
	for _, comment := range record.Comments {
		source := *provenance
		source.ExternalID = localboard.SourceKey(record.Key, "comment", record.Kind, comment.ID)
		source.CreatedAt, source.UpdatedAt = comment.CreatedAt, comment.UpdatedAt
		source.AuthorID = "detent"
		if _, err := insertNativeComment(ctx, tx, scope, issue, comment.Body, &source, now); err != nil {
			return result, err
		}
	}
	for _, id := range dependencies {
		if slices.Contains(issue.Dependencies, id) {
			continue
		}
		issue, err = changeNativeDependencyTx(ctx, tx, scope, string(issue.WorkItemID), tracker.DependencyMutation{ExpectedRevision: issue.Revision, RelatedWorkItemID: id, Operation: "add"}, now)
		if err != nil {
			return result, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO local_board_imports (organization_id,project_id,source_key,work_item_id,created_at) VALUES (?,?,?,?,?)`, scope.organization, scope.project, record.Key, issue.WorkItemID, formatHubTime(now)); err != nil {
		return result, err
	}
	result.WorkItemID = issue.WorkItemID
	return result, nil
}
