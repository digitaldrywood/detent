package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func readLinkedIssueSourceProjection(ctx context.Context, query nativeQueryer, id string, compact bool) (*tracker.LinkedIssueSource, error) {
	var source tracker.LinkedIssueSource
	var raw sql.NullString
	snapshotColumn := "snapshot_json"
	if compact {
		snapshotColumn = "CASE WHEN snapshot_json IS NULL THEN NULL ELSE '{}' END"
	}
	err := query.QueryRowContext(ctx, "SELECT source_url, "+snapshotColumn+" FROM linked_issue_sources WHERE work_item_id = ?", id).Scan(&source.URL, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // No linked source is valid for an ordinary native issue.
	}
	if err != nil {
		return nil, err
	}
	source.Status = "pending"
	if raw.Valid {
		source.Status = "complete"
		if !compact {
			if err := json.Unmarshal([]byte(raw.String), &source.Snapshot); err != nil {
				return nil, err
			}
		}
	}
	return &source, nil
}

const linkedIssueRepositoryRequired = "Read get_project_integration for this project. An administrator must associate its repository through bind_native_repository before linking GitHub issues; use source runner_checkout with an enrolled runner reporting the matching checkout."
const linkedIssueRepositoryMismatch = "GitHub issue must belong to this native project's attached repository"

func createLinkedIssueTx(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, now time.Time) (tracker.NativeIssue, error) {
	canonical, repository, number, err := tracker.ParseGitHubIssueURL(strings.TrimSpace(request.GitHubIssueURL))
	if err != nil {
		return tracker.NativeIssue{}, nativeInvalid(err.Error())
	}
	if request.Provenance != nil {
		return tracker.NativeIssue{}, nativeInvalid("Linked issues obtain provenance during runner intake")
	}
	integration, err := readProjectIntegration(ctx, tx, scope)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	if integration.Profile == "native" && integration.Repository == "" && integration.CheckoutRepository == "" {
		return tracker.NativeIssue{}, nativeInvalid(linkedIssueRepositoryRequired)
	}
	if integration.Profile != "native" || (!strings.EqualFold(repository, integration.Repository) && !strings.EqualFold(repository, integration.CheckoutRepository)) {
		return tracker.NativeIssue{}, nativeInvalid(linkedIssueRepositoryMismatch)
	}
	key := "github:" + canonical
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT native_id FROM issues WHERE organization_id = ? AND project_id = ?
AND (native_source_key = ? OR (repository_id = ? AND github_number = ?))`, scope.organization, scope.project, key, integration.RepositoryID, number).Scan(&existing)
	if err == nil {
		issue, _, err := readNativeIssue(ctx, tx, scope, existing)
		return issue, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return tracker.NativeIssue{}, err
	}
	if err := ensureNativeTriage(ctx, tx, scope, now); err != nil {
		return tracker.NativeIssue{}, err
	}
	request.State = "Triage"
	titleSupplied, bodySupplied := request.Title != "", request.Body != ""
	if !titleSupplied {
		request.Title = fmt.Sprintf("GitHub issue %s#%d", repository, number)
	}
	request.GitHubIssueURL = ""
	issue, err := createNativeIssueTx(ctx, tx, scope, request, now)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE issues SET native_source_key = ? WHERE native_id = ?", key, issue.WorkItemID); err != nil {
		return tracker.NativeIssue{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO linked_issue_sources (work_item_id, source_url, title_supplied, body_supplied) VALUES (?, ?, ?, ?)", issue.WorkItemID, canonical, titleSupplied, bodySupplied); err != nil {
		return tracker.NativeIssue{}, err
	}
	issue.LinkedSource = &tracker.LinkedIssueSource{URL: canonical, Status: "pending"}
	issue.ExternalReferences = append(issue.ExternalReferences, tracker.GitHubIssueSourceReference(canonical, canonical))
	return issue, nil
}

func (s *Service) intakeLinkedIssue(c echo.Context) error {
	var request tracker.GitHubIntake
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		return intakeLinkedIssueTx(ctx, tx, scope, c.Param("item"), request.Snapshot, now)
	})
}

func intakeLinkedIssueTx(ctx context.Context, tx *sql.Tx, scope nativeScope, id string, snapshot tracker.GitHubIssueSnapshot, now time.Time) (any, error) {
	issue, _, err := readNativeIssue(ctx, tx, scope, id)
	if err != nil {
		return nil, err
	}
	if issue.Profile != "native" || issue.LinkedSource == nil {
		return nil, nativeInvalid("Issue has no linked source awaiting intake")
	}
	completed := issue.LinkedSource.Status == "complete"
	canonical, _, _, err := tracker.ParseGitHubIssueURL(snapshot.URL)
	if err != nil || canonical != issue.LinkedSource.URL {
		return nil, nativeInvalid("Intake source does not match the linked issue")
	}
	if err := validateNativeContent(snapshot.Title, snapshot.Body, nil, nil, nil); err != nil {
		return nil, err
	}
	if err := validateImportProvenance(&snapshot.Provenance); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(snapshot.Comments))
	for _, comment := range snapshot.Comments {
		if err := validateImportProvenance(&comment.Provenance); err != nil {
			return nil, err
		}
		if len(comment.Body) > 64<<10 || seen[comment.Provenance.ExternalID] {
			return nil, nativeInvalid("Intake discussion contains an oversized or duplicate comment")
		}
		seen[comment.Provenance.ExternalID] = true
	}
	// Native field ownership includes an explicit edit back to the initial
	// value. Comparing only strings would lose that human intent.
	var titleOwned, bodyOwned bool
	err = tx.QueryRowContext(ctx, `SELECT
title_supplied OR EXISTS (SELECT 1 FROM collaboration_events e, json_each(e.data_json, '$.fields') f WHERE e.work_item_id = s.work_item_id AND e.type = 'issue.edited' AND f.value = 'title'),
body_supplied OR EXISTS (SELECT 1 FROM collaboration_events e, json_each(e.data_json, '$.fields') f WHERE e.work_item_id = s.work_item_id AND e.type = 'issue.edited' AND f.value = 'body')
FROM linked_issue_sources s WHERE work_item_id = ?`, issue.WorkItemID).Scan(&titleOwned, &bodyOwned)
	if err != nil {
		return nil, err
	}
	stale := completed && !snapshot.Provenance.UpdatedAt.After(issue.LinkedSource.Snapshot.Provenance.UpdatedAt)
	if !titleOwned && !stale {
		issue.Title = snapshot.Title
	}
	if !bodyOwned && !stale {
		issue.Body = snapshot.Body
	}
	for _, comment := range snapshot.Comments {
		if err := importGitHubComment(ctx, tx, scope, string(issue.WorkItemID), GitHubImportRecord{Body: comment.Body, Provenance: comment.Provenance}, now); err != nil {
			return nil, err
		}
	}
	if stale {
		return readLinkedNativeIssue(ctx, tx, scope, id)
	}
	// Comment originals already live in append-only collaboration versions;
	// keep issue snapshot evidence without duplicating discussion on reads.
	snapshot.Comments = nil
	raw, err := marshalNative(snapshot)
	if err != nil {
		return nil, err
	}
	provenance, err := marshalNative(snapshot.Provenance)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE linked_issue_sources SET snapshot_json = ? WHERE work_item_id = ?", raw, issue.WorkItemID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE issues SET provenance_json = ?, author_login = ? WHERE native_id = ?", provenance, snapshot.Provenance.AuthorID, issue.WorkItemID); err != nil {
		return nil, err
	}
	issue, err = persistNativeIssue(ctx, tx, scope, issue, "github.imported", tracker.CollaborationData{Operation: "source_intake", Reason: canonical}, now)
	if err == nil && !completed {
		err = enqueueLinkedSourceSummary(ctx, tx, scope, issue, "intake", false, now)
	}
	return issue, err
}
