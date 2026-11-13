package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readLinkedIssueSourceProjection(ctx context.Context, query hubapi.NativeQueryer, id string, compact bool) (*tracker.LinkedIssueSource, error) {
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

func validateLinkedIssueRepository(integration ProjectIntegration, repository string) error {
	if integration.Profile == "native" && integration.Repository == "" && integration.CheckoutRepository == "" {
		return hubapi.NativePublicInvalid(linkedIssueRepositoryRequired)
	}
	if integration.Profile != "native" || (!strings.EqualFold(repository, integration.Repository) && !strings.EqualFold(repository, integration.CheckoutRepository)) {
		return hubapi.NativePublicInvalid(linkedIssueRepositoryMismatch)
	}
	return nil
}

func (s *Service) intakeLinkedIssue(c echo.Context) error {
	var request tracker.GitHubIntake
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	return s.intakeLinkedIssueRecord(c, request)
}
