package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"maps"
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
		return s.invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	if err := localboard.Validate(request.Record); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	if err := validateNativeContent(request.Record.Title, request.Record.Body, nil, nil, request.Record.Priority); err != nil {
		return s.nativeAPIError(c, err)
	}
	plan, issue, err := planLocalBoardItem(c.Request().Context(), s.database.reader, scope, request.Record)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if plan.Action == "unchanged" {
		return c.JSON(http.StatusOK, plan)
	}
	if request.DryRun {
		if err := s.preflightLocalBoardAllowances(c.Request().Context(), scope, request.Record, plan, issue); err != nil {
			return s.nativeAPIError(c, err)
		}
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

func (s *Service) preflightLocalBoardAllowances(ctx context.Context, scope nativeScope, record localboard.Record, plan localboard.Result, issue tracker.NativeIssue) error {
	if s.database.plans() == nil {
		return nil
	}
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.config.now()
	if err := s.database.requireHostedFeature(ctx, tx, "collaboration", now); err != nil {
		return err
	}
	before, err := s.database.hostedConsumption(ctx, tx, now, hostedNativeMutationMetrics(localboard.Request{}, false)...)
	if err != nil {
		return err
	}
	after := maps.Clone(before)
	if record.Kind == "local_sqlite" {
		var terminal bool
		if err := tx.QueryRowContext(ctx, "SELECT terminal FROM workflow_states WHERE project_id=? AND detent_state=?", scope.project, plan.State).Scan(&terminal); err != nil {
			return err
		}
		if !terminal {
			after["unarchived_issues"]++
		}
		after["collaboration_bytes"] += int64(len(record.Title) + len(record.Body))
	}
	for _, comment := range record.Comments {
		after["collaboration_bytes"] += int64(len(comment.Body))
	}
	changes := int64(len(record.Comments))
	if record.Kind == "local_sqlite" || issue.State != plan.State {
		changes++
	}
	for _, key := range record.Dependencies {
		var id tracker.NativeWorkItemID
		err := tx.QueryRowContext(ctx, "SELECT work_item_id FROM local_board_imports WHERE organization_id=? AND project_id=? AND source_key=?", scope.organization, scope.project, key).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) || !slices.Contains(issue.Dependencies, id) {
			changes++
		}
	}
	if changes > 0 {
		after["collaboration_bytes"]++
		after["history_records"] += changes
		after["events_total"] += changes
	}
	return s.database.validateHostedConsumptionGrowth(ctx, tx, before, after, now, false)
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
	return "", false, nativePublicInvalid("Local board import requires a nondispatchable nonterminal fallback state")
}

func planLocalBoardItem(ctx context.Context, q nativeQueryer, scope nativeScope, record localboard.Record) (localboard.Result, tracker.NativeIssue, error) {
	result := localboard.Result{SourceID: record.SourceID, SourceState: record.State, Action: "created", Comments: len(record.Comments), Dependencies: len(record.Dependencies)}
	project, err := readNativeProject(ctx, q, scope)
	if err != nil {
		return result, tracker.NativeIssue{}, err
	}
	if project.Profile != "native" {
		return result, tracker.NativeIssue{}, nativePublicInvalid("Local board import requires a native project")
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
		return result, tracker.NativeIssue{}, nativePublicInvalid("GitHub local item requires exactly one existing onboarding intake match")
	}
	issue, _, err := readNativeIssue(ctx, q, scope, ids[0])
	if err != nil {
		return result, issue, err
	}
	if issue.Archived {
		return result, issue, nativePublicInvalid("Local board import cannot change an archived native item")
	}
	if issue.State != "Triage" && (!issue.Terminal || issue.State != result.State) {
		return result, issue, nativePublicInvalid("GitHub local lane restoration requires an intake item still in Triage")
	}
	result.Action, result.WorkItemID = "updated", issue.WorkItemID
	return result, issue, nil
}

func importLocalBoardItem(ctx context.Context, tx *sql.Tx, scope nativeScope, record localboard.Record, now time.Time) (localboard.Result, error) {
	scope.importedContent = true
	result, issue, err := planLocalBoardItem(ctx, tx, scope, record)
	if err != nil || result.Action == "unchanged" {
		return result, err
	}
	dependencies := []tracker.NativeWorkItemID{}
	for _, key := range record.Dependencies {
		var id tracker.NativeWorkItemID
		if err := tx.QueryRowContext(ctx, `SELECT work_item_id FROM local_board_imports WHERE organization_id=? AND project_id=? AND source_key=?`, scope.organization, scope.project, key).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return result, nativePublicInvalid("Import prerequisite items before their dependents")
			}
			return result, err
		}
		dependencies = append(dependencies, id)
	}
	provenance := &tracker.Provenance{Provider: record.Kind, ExternalID: record.Key, AuthorID: record.Author, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, ObservedAt: now}
	if record.Kind == "local_sqlite" {
		issue, err = createNativeIssueDraft(ctx, tx, scope, tracker.CreateIssue{Title: record.Title, Body: record.Body, Priority: record.Priority, State: result.State, Provenance: provenance}, now, false, true)
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
