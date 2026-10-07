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

type githubBatchView struct {
	Batch *tracker.GitHubBatch  `json:"batch"`
	Lanes []tracker.NativeState `json:"lanes"`
}

func readGitHubBatch(ctx context.Context, query nativeQueryer, scope nativeScope) (*tracker.GitHubBatch, error) {
	var raw string
	err := query.QueryRowContext(ctx, "SELECT request_json FROM onboarding_issue_intake WHERE project_id=?", scope.project).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // No stored batch is a valid optional result.
	}
	if err != nil {
		return nil, err
	}
	var batch tracker.GitHubBatch
	return &batch, json.Unmarshal([]byte(raw), &batch)
}

func githubBatchResponse(ctx context.Context, query nativeQueryer, scope nativeScope, batch *tracker.GitHubBatch) (githubBatchView, error) {
	project, err := readNativeProject(ctx, query, scope)
	return githubBatchView{Batch: batch, Lanes: project.States}, err
}

func saveGitHubBatch(ctx context.Context, tx *sql.Tx, scope nativeScope, batch *tracker.GitHubBatch) error {
	batch.Revision++
	raw, err := marshalNative(batch)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO onboarding_issue_intake(project_id,request_json) VALUES(?,?) ON CONFLICT(project_id) DO UPDATE SET request_json=excluded.request_json`, scope.project, raw)
	return err
}

func (s *Service) getGitHubBatch(c echo.Context) error {
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	view, err := s.projectGitHubBatch(ctx, scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, view)
}

func (s *Service) projectGitHubBatch(ctx context.Context, scope nativeScope) (githubBatchView, error) {
	batch, err := readGitHubBatch(ctx, s.database.db, scope)
	if err != nil {
		return githubBatchView{}, err
	}
	return githubBatchResponse(ctx, s.database.db, scope, batch)
}

func batchHasPending(batch *tracker.GitHubBatch) bool {
	if batch.Status == "discovering" {
		return true
	}
	return slices.ContainsFunc(batch.Items, func(item tracker.GitHubBatchItem) bool { return item.Status == "pending" })
}

func requireBatchRunner(ctx context.Context, tx *sql.Tx, scope nativeScope, runner, repository string, now time.Time) error {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM runner_identities r
 JOIN api_tokens t ON t.id=r.token_id
 JOIN token_grants g ON g.token_id=t.id AND g.organization_id=r.organization_id AND g.project_id=?
 JOIN runner_checkout_repositories cr ON cr.runner_id=r.id AND cr.project_id=g.project_id
 WHERE r.id=? AND r.organization_id=? AND r.state='active' AND t.revoked_at IS NULL
 AND julianday(t.expires_at)>julianday(?) AND lower(cr.repository)=lower(?)
 AND julianday(cr.reported_at)>julianday(?)
 AND EXISTS(SELECT 1 FROM json_each(r.operations_json) WHERE value='heartbeat')`, scope.project, runner, scope.organization, formatHubTime(now), repository, formatHubTime(now.Add(-5*time.Minute))).Scan(&count)
	if err != nil {
		return err
	}
	if count == 0 {
		return nativeInvalid("Select an active enrolled runner with a recent matching checkout report")
	}
	return nil
}

func (s *Service) commandGitHubBatch(c echo.Context) error {
	var request tracker.GitHubBatchCommand
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, s.commandGitHubBatchOperation(request))
}

func (s *Service) commandGitHubBatchOperation(request tracker.GitHubBatchCommand) func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error) {
	return func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		batch, err := readGitHubBatch(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		revision := int64(0)
		if batch != nil {
			revision = batch.Revision
		}
		if revision != request.Revision {
			return nil, nativeConflict(tracker.Revision(revision))
		}
		integration, err := readProjectIntegration(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		repository := integration.CheckoutRepository
		if repository == "" {
			repository = integration.Repository
		}
		if integration.Profile != "native" || repository == "" {
			return nil, nativeInvalid("Attach the native project's runner checkout first")
		}
		if !integration.ManualImportEnabled {
			return nil, nativeInvalid("Manual import of existing GitHub issues is disabled")
		}
		switch request.Action {
		case "discover":
			if batch != nil && (batchHasPending(batch) || slices.ContainsFunc(batch.Items, func(item tracker.GitHubBatchItem) bool { return item.Status == "failed" })) {
				return nil, nativeInvalid("Finish the current intake before starting another preview")
			}
			if len(request.Labels) > 20 {
				return nil, nativeInvalid("At most 20 label filters are allowed")
			}
			for _, label := range request.Labels {
				if strings.TrimSpace(label) == "" || len(label) > 100 {
					return nil, nativeInvalid("Label filters must be nonempty and at most 100 bytes")
				}
			}
			if err := requireBatchRunner(ctx, tx, scope, request.RunnerID, repository, now); err != nil {
				return nil, err
			}
			batch = &tracker.GitHubBatch{ID: newNativeID("intake"), Revision: revision, RunnerID: request.RunnerID, Discovery: tracker.GitHubDiscovery{Repository: strings.ToLower(repository), Labels: request.Labels, IncludeClosed: request.IncludeClosed}, Status: "discovering", Page: tracker.GitHubDiscoveryPage{Issues: []tracker.GitHubIssuePreview{}}, Items: []tracker.GitHubBatchItem{}}
		case "more":
			if batch == nil || batch.Status != "preview" || batch.Page.NextCursor == "" || len(batch.Page.Issues) >= 1000 {
				return nil, nativeInvalid("No further preview page is available (maximum 1000 issues per intake)")
			}
			batch.Discovery.Cursor = batch.Page.NextCursor
			batch.Status = "discovering"
		case "apply":
			if batch == nil || batch.Status != "preview" || len(request.Numbers) == 0 || len(request.Numbers) > 1000 {
				return nil, nativeInvalid("Select issues from the completed preview")
			}
			if err := requireBatchRunner(ctx, tx, scope, batch.RunnerID, repository, now); err != nil {
				return nil, err
			}
			project, err := readNativeProject(ctx, tx, scope)
			if err != nil {
				return nil, err
			}
			destination := request.Destination
			if destination == "" {
				for _, state := range project.States {
					if !state.Dispatchable && !state.Terminal {
						destination = state.Name
						break
					}
				}
			}
			lane, exists := findBatchLane(project.States, destination)
			if !exists || lane.Dispatchable && !request.AllowDispatch {
				return nil, nativeInvalid("Select a destination and explicitly confirm any dispatchable lane")
			}
			batch.Destination = destination
			seen := map[int]bool{}
			for _, number := range request.Numbers {
				if seen[number] {
					continue
				}
				seen[number] = true
				index := slices.IndexFunc(batch.Page.Issues, func(preview tracker.GitHubIssuePreview) bool { return preview.Number == number })
				if index < 0 {
					return nil, nativeInvalid("Selected issue is absent from the preview")
				}
				preview := batch.Page.Issues[index]
				var existing string
				err := tx.QueryRowContext(ctx, `SELECT native_id FROM issues WHERE organization_id=? AND project_id=? AND
    (native_source_key IN (?,?) OR github_node_id=? OR (json_extract(provenance_json, '$.provider')='github' AND json_extract(provenance_json, '$.external_id')=?) OR (repository_id=? AND github_number=?))`, scope.organization, scope.project, "github:"+preview.URL, "github:"+preview.ID, preview.ID, preview.ID, integration.RepositoryID, number).Scan(&existing)
				if err == nil {
					batch.Items = append(batch.Items, tracker.GitHubBatchItem{Number: number, WorkItemID: tracker.NativeWorkItemID(existing), Status: "skipped"})
					continue
				}
				if !errors.Is(err, sql.ErrNoRows) {
					return nil, err
				}
				issue, err := createLinkedIssueTx(ctx, tx, scope, tracker.CreateIssue{GitHubIssueURL: preview.URL, State: destination}, now)
				if err != nil {
					return nil, err
				}
				batch.Items = append(batch.Items, tracker.GitHubBatchItem{Number: number, WorkItemID: issue.WorkItemID, Status: "pending"})
			}
			batch.Status = "importing"
			if !batchHasPending(batch) {
				batch.Status = "finished"
			}
		case "retry":
			if batch == nil || batchHasPending(batch) && (request.RunnerID == "" || request.RunnerID == batch.RunnerID) {
				return nil, nativeInvalid("Wait for current intake work to finish, or select another runner")
			}
			if request.RunnerID != "" {
				batch.RunnerID = request.RunnerID
			}
			if err := requireBatchRunner(ctx, tx, scope, batch.RunnerID, repository, now); err != nil {
				return nil, err
			}
			if batch.Status == "discovery_failed" || batch.Status == "discovering" {
				if err := batchRetryAllowed(batch.RetryAt, now); err != nil {
					return nil, err
				}
				batch.Status = "discovering"
				batch.Error = ""
				batch.RetryAt = ""
			} else {
				for index, item := range batch.Items {
					if item.Status == "failed" {
						if err := batchRetryAllowed(item.RetryAt, now); err != nil {
							return nil, err
						}
						batch.Items[index].Status = "pending"
						batch.Items[index].Error = ""
						batch.Items[index].RetryAt = ""
					}
				}
				batch.Status = "importing"
				if !batchHasPending(batch) {
					batch.Status = "finished"
				}
			}
		default:
			return nil, nativeInvalid("Unknown intake action")
		}
		if err := saveGitHubBatch(ctx, tx, scope, batch); err != nil {
			return nil, err
		}
		return githubBatchResponse(ctx, tx, scope, batch)
	}
}

func findBatchLane(states []tracker.NativeState, name string) (tracker.NativeState, bool) {
	for _, state := range states {
		if state.Name == name {
			return state, true
		}
	}
	return tracker.NativeState{}, false
}
func batchRetryAllowed(raw string, now time.Time) error {
	if raw == "" {
		return nil
	}
	deadline, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nativeInvalid("Invalid source retry deadline")
	}
	if now.Before(deadline) {
		return nativeInvalid("Source rate limit has not reset; retry after " + raw)
	}
	return nil
}

func readGitHubBatchTask(ctx context.Context, tx *sql.Tx, scope nativeScope) (*tracker.GitHubBatchTask, error) {
	batch, err := readGitHubBatch(ctx, tx, scope)
	if err != nil || batch == nil {
		return nil, err
	}
	if batch.RunnerID != scope.credential.Runner.RunnerID {
		return nil, nil //nolint:nilnil // This runner has no assigned task.
	}
	if batch.Status == "discovering" {
		return &tracker.GitHubBatchTask{ProjectID: scope.project, BatchID: batch.ID, Revision: batch.Revision, Discovery: &batch.Discovery}, nil
	}
	changed := false
	var preview *tracker.GitHubIssuePreview
	for i, item := range batch.Items {
		if item.Status == "pending" {
			// An explicitly dispatchable selection can complete the same one-time
			// intake through #3257 before its batch turn. Honor that existing snapshot.
			var complete bool
			if err := tx.QueryRowContext(ctx, "SELECT snapshot_json IS NOT NULL FROM linked_issue_sources WHERE work_item_id=?", item.WorkItemID).Scan(&complete); err != nil {
				return nil, err
			}
			if complete {
				batch.Items[i].Status = "completed"
				changed = true
				continue
			}
			index := slices.IndexFunc(batch.Page.Issues, func(p tracker.GitHubIssuePreview) bool { return p.Number == item.Number })
			if index < 0 {
				return nil, errors.New("intake item missing preview")
			}
			preview = &batch.Page.Issues[index]
			break
		}
	}
	if changed {
		if !batchHasPending(batch) {
			batch.Status = "finished"
		}
		if err := saveGitHubBatch(ctx, tx, scope, batch); err != nil {
			return nil, err
		}
	}
	if preview == nil {
		return nil, nil //nolint:nilnil // No pending preview means this batch has no task.
	}
	return &tracker.GitHubBatchTask{ProjectID: scope.project, BatchID: batch.ID, Revision: batch.Revision, Item: preview}, nil
}

func (s *Service) reportGitHubBatch(c echo.Context) error {
	var request tracker.GitHubBatchResult
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		batch, err := readGitHubBatch(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if batch == nil || batch.RunnerID != scope.credential.Runner.RunnerID || batch.ID != request.BatchID {
			return nil, nativeNotFound()
		}
		if batch.Revision != request.Revision {
			return nil, nativeConflict(tracker.Revision(batch.Revision))
		}
		if len(request.Error) > 1000 {
			return nil, nativeInvalid("Source diagnostic exceeds 1000 bytes")
		}
		if request.RetryAt != "" {
			if _, err := time.Parse(time.RFC3339Nano, request.RetryAt); err != nil {
				return nil, nativeInvalid("Invalid source retry deadline")
			}
		}
		switch batch.Status {
		case "discovering":
			if request.Error != "" {
				batch.Status = "discovery_failed"
				batch.Error = request.Error
				batch.RetryAt = request.RetryAt
			} else {
				if request.Page == nil || len(request.Page.Issues) > 100 || len(batch.Page.Issues)+len(request.Page.Issues) > 1000 || request.Page.Total < 0 {
					return nil, nativeInvalid("Invalid source discovery page")
				}
				seen := map[int]bool{}
				ids := map[string]bool{}
				for _, p := range batch.Page.Issues {
					seen[p.Number] = true
					ids[p.ID] = true
				}
				for _, p := range request.Page.Issues {
					canonical, repository, number, err := tracker.ParseGitHubIssueURL(p.URL)
					if err != nil || repository != batch.Discovery.Repository || number != p.Number || canonical != p.URL || seen[number] || ids[p.ID] || p.ID == "" || p.Title == "" || len(p.Title) > 500 || len([]rune(p.Body)) > 1000 || p.Closed && !batch.Discovery.IncludeClosed {
						return nil, nativeInvalid("Mismatched or duplicate source preview")
					}
					seen[number] = true
					ids[p.ID] = true
				}
				if request.Page.NextCursor != "" && (request.Page.NextCursor == batch.Discovery.Cursor || len(request.Page.Issues) == 0) {
					return nil, nativeInvalid("Invalid source discovery cursor")
				}
				batch.Page.Issues = append(batch.Page.Issues, request.Page.Issues...)
				batch.Page.Total = request.Page.Total
				batch.Page.NextCursor = request.Page.NextCursor
				batch.Status = "preview"
			}
		case "importing":
			index := slices.IndexFunc(batch.Items, func(item tracker.GitHubBatchItem) bool {
				return item.Number == request.Number && item.Status == "pending"
			})
			if index < 0 {
				return nil, nativeInvalid("No pending selected issue")
			}
			item := &batch.Items[index]
			if request.Error != "" {
				item.Status = "failed"
				item.Error = request.Error
				item.RetryAt = request.RetryAt
				// A source throttle stops this explicit intake. The operator resumes it
				// after the reported deadline; there is no automatic source retry loop.
				if request.RetryAt != "" {
					for i := range batch.Items {
						if batch.Items[i].Status == "pending" {
							batch.Items[i].Status = "failed"
							batch.Items[i].Error = "Source intake stopped at rate limit"
							batch.Items[i].RetryAt = request.RetryAt
						}
					}
				}
			} else {
				preview := batch.Page.Issues[slices.IndexFunc(batch.Page.Issues, func(p tracker.GitHubIssuePreview) bool { return p.Number == item.Number })]
				if request.Snapshot == nil || request.Snapshot.Provenance.Provider != "github" || request.Snapshot.Provenance.ExternalID != preview.ID {
					return nil, nativeInvalid("Snapshot stable source identity does not match preview")
				}
				if _, err := intakeLinkedIssueTx(ctx, tx, scope, string(item.WorkItemID), *request.Snapshot, now); err != nil {
					return nil, err
				}
				item.Status = "completed"
			}
			if !batchHasPending(batch) {
				batch.Status = "finished"
			}
		default:
			return nil, nativeInvalid("Intake has no outstanding source request")
		}
		if err := saveGitHubBatch(ctx, tx, scope, batch); err != nil {
			return nil, err
		}
		// Keep per-item receipts small: replaying the entire discovery preview
		// for every checkpoint would duplicate backlog content in native_commands.
		return struct {
			Revision int64  `json:"revision"`
			Status   string `json:"status"`
		}{batch.Revision, batch.Status}, nil
	})
}
