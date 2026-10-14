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

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type githubBatchView struct {
	Progress githubBatchProgress   `json:"progress"`
	Batch    *tracker.GitHubBatch  `json:"batch"`
	Lanes    []tracker.NativeState `json:"lanes"`
}

type githubBatchProgress struct {
	Discovered int `json:"discovered"`
	Total      int `json:"total"`
	Pending    int `json:"pending"`
	Completed  int `json:"completed"`
	Failed     int `json:"failed"`
	Skipped    int `json:"skipped"`
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
	progress := githubBatchProgress{}
	if batch != nil {
		progress.Discovered, progress.Total = len(batch.Page.Issues), batch.Page.Total
		for _, item := range batch.Items {
			switch item.Status {
			case "pending":
				progress.Pending++
			case "completed":
				progress.Completed++
			case "failed":
				progress.Failed++
			case "skipped":
				progress.Skipped++
			}
		}
	}
	if batch != nil {
		copy := *batch
		copy.Page.Issues = slices.Clone(batch.Page.Issues)
		for i := range copy.Page.Issues {
			copy.Page.Issues[i].Body = ""
			copy.Page.Issues[i].Snapshot = nil
		}
		batch = &copy
	}
	return githubBatchView{Batch: batch, Lanes: project.States, Progress: progress}, err
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
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	batch, err := readGitHubBatch(ctx, s.database.reader, scope)
	if err != nil {
		return githubBatchView{}, err
	}
	return githubBatchResponse(ctx, s.database.reader, scope, batch)
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
		case "discover", "start":
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
			if s.config.ImportBackend == nil {
				if err := requireBatchRunner(ctx, tx, scope, request.RunnerID, repository, now); err != nil {
					return nil, err
				}
			}
			batch = &tracker.GitHubBatch{ID: newNativeID("intake"), Revision: revision, RunnerID: request.RunnerID, Discovery: tracker.GitHubDiscovery{Snapshots: true, Repository: strings.ToLower(repository), Labels: request.Labels, IncludeClosed: request.IncludeClosed}, Status: "discovering", Page: tracker.GitHubDiscoveryPage{Issues: []tracker.GitHubIssuePreview{}}, Items: []tracker.GitHubBatchItem{}, HubOwned: s.config.ImportBackend != nil}
			actor := scope.actor()
			batch.RequestedBy = &actor
			batch.Limit, err = s.githubBatchLimit(ctx, tx, now)
			if err != nil {
				return nil, err
			}
			if request.Action == "start" {
				batch.Automatic = true
				if err := configureBatchDestination(ctx, tx, scope, batch, request); err != nil {
					return nil, err
				}
			}
		case "more":
			if batch == nil || batch.Status != "preview" || batch.Page.NextCursor == "" || len(batch.Page.Issues) >= batchLimit(batch) {
				return nil, nativeInvalid("No further preview page is available (maximum 1000 issues per intake)")
			}
			batch.Discovery.Cursor = batch.Page.NextCursor
			batch.Status = "discovering"
		case "apply":
			if batch == nil || batch.Status != "preview" || len(request.Numbers) == 0 || len(request.Numbers) > batchLimit(batch) {
				return nil, nativeInvalid("Select issues from the completed preview")
			}
			if !batch.HubOwned {
				if err := requireBatchRunner(ctx, tx, scope, batch.RunnerID, repository, now); err != nil {
					return nil, err
				}
			}
			if err := configureBatchDestination(ctx, tx, scope, batch, request); err != nil {
				return nil, err
			}
			project, err := readNativeProject(ctx, tx, scope)
			if err != nil {
				return nil, err
			}

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
				destination, labels, err := batchIssueDestination(project.States, batch, preview.Labels)
				if err != nil {
					return nil, err
				}
				var existing string
				err = tx.QueryRowContext(ctx, `SELECT native_id FROM issues WHERE organization_id=? AND project_id=? AND
    (native_source_key IN (?,?) OR github_node_id=? OR (json_extract(provenance_json, '$.provider')='github' AND json_extract(provenance_json, '$.external_id')=?) OR (repository_id=? AND github_number=?))`, scope.organization, scope.project, "github:"+preview.URL, "github:"+preview.ID, preview.ID, preview.ID, integration.RepositoryID, number).Scan(&existing)
				if err == nil {
					batch.Items = append(batch.Items, tracker.GitHubBatchItem{Number: number, WorkItemID: tracker.NativeWorkItemID(existing), Status: "skipped"})
					continue
				}
				if !errors.Is(err, sql.ErrNoRows) {
					return nil, err
				}
				issue, err := createLinkedIssueInStateTx(ctx, tx, scope, tracker.CreateIssue{GitHubIssueURL: preview.URL, State: destination, Labels: labels}, now)
				if err != nil {
					return nil, err
				}
				batch.Items = append(batch.Items, tracker.GitHubBatchItem{Number: number, WorkItemID: issue.WorkItemID, Status: "pending"})
			}
			batch.Status = "importing"
			if !batchHasPending(batch) {
				batch.Status = "finished"
			}
		case "close_source":
			if batch == nil || batch.Status != "finished" {
				return nil, nativeInvalid("Finish importing before closing source issues")
			}
			if s.config.OutboxBackend == nil || s.database.linkedSourceBase == "" {
				return nil, nativeInvalid("Closing source issues requires configured GitHub transport and a public Cloud URL")
			}
			for index, item := range batch.Items {
				if item.Status != "completed" || item.SourceCloseRequested {
					continue
				}
				previewIndex := slices.IndexFunc(batch.Page.Issues, func(preview tracker.GitHubIssuePreview) bool { return preview.Number == item.Number })
				if previewIndex < 0 || batch.Page.Issues[previewIndex].Closed {
					continue
				}
				target := s.database.linkedSourceBase + "/work/i/" + url.PathEscape(string(item.WorkItemID))
				if err := enqueueNativeSummary(ctx, tx, scope, string(item.WorkItemID), "intake-close:"+batch.ID, "Tracked in Detent: "+target, true, now); err != nil {
					return nil, err
				}
				batch.Items[index].SourceCloseRequested = true
			}
		case "retry":
			if batch == nil || !batch.HubOwned && batchHasPending(batch) && (request.RunnerID == "" || request.RunnerID == batch.RunnerID) {
				return nil, nativeInvalid("Wait for current intake work to finish, or select another runner")
			}
			if request.RunnerID != "" {
				batch.RunnerID = request.RunnerID
			}
			if !batch.HubOwned {
				if err := requireBatchRunner(ctx, tx, scope, batch.RunnerID, repository, now); err != nil {
					return nil, err
				}
			}
			if request.AllowDispatch || request.Destination != "" || request.LabelLanes != nil {
				effective := request
				if effective.Destination == "" {
					effective.Destination = batch.Destination
				}
				if effective.LabelLanes == nil {
					effective.LabelLanes = batch.LabelLanes
				}
				effective.AllowDispatch = effective.AllowDispatch || batch.AllowDispatch
				if err := configureBatchDestination(ctx, tx, scope, batch, effective); err != nil {
					return nil, err
				}
				actor := scope.actor()
				batch.RequestedBy = &actor
			}
			batch.Limit, err = s.githubBatchLimit(ctx, tx, now)
			if err != nil {
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
						previewIndex := slices.IndexFunc(batch.Page.Issues, func(p tracker.GitHubIssuePreview) bool { return p.Number == item.Number })
						if previewIndex >= 0 {
							batch.Page.Issues[previewIndex].Snapshot = nil
						}
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
	if batch.HubOwned || batch.RunnerID != scope.credential.Runner.RunnerID || batch.Status != "importing" && batch.Status != "discovering" {
		return nil, nil //nolint:nilnil // This runner has no assigned task.
	}
	return nextGitHubBatchTask(ctx, tx, scope, batch)
}

func nextGitHubBatchTask(ctx context.Context, tx *sql.Tx, scope nativeScope, batch *tracker.GitHubBatch) (*tracker.GitHubBatchTask, error) {
	if batch.Status == "discovering" {
		return &tracker.GitHubBatchTask{ProjectID: scope.project, BatchID: batch.ID, Revision: batch.Revision, Discovery: &batch.Discovery}, nil
	}
	changed := false
	previews := []tracker.GitHubIssuePreview{}
	for i, item := range batch.Items {
		if item.Status == "pending" {
			// An explicitly dispatchable selection can complete the same one-time
			// intake through #3257 before its batch turn. Honor that existing snapshot.
			var complete bool
			if item.WorkItemID != "" {
				if err := tx.QueryRowContext(ctx, "SELECT snapshot_json IS NOT NULL FROM linked_issue_sources WHERE work_item_id=?", item.WorkItemID).Scan(&complete); err != nil {
					return nil, err
				}
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
			previews = append(previews, batch.Page.Issues[index])
			if len(previews) == 100 {
				break
			}
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
	if len(previews) == 0 {
		return nil, nil //nolint:nilnil // No pending preview means this batch has no task.
	}
	return &tracker.GitHubBatchTask{ProjectID: scope.project, BatchID: batch.ID, Revision: batch.Revision, Item: &previews[0], Items: previews}, nil
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
		if batch == nil || batch.HubOwned || batch.RunnerID != scope.credential.Runner.RunnerID || batch.ID != request.BatchID {
			return nil, nativeNotFound()
		}
		return s.applyGitHubBatchResult(ctx, tx, scope, batch, request, now)
	})
}

func (s *Service) applyGitHubBatchResult(ctx context.Context, tx *sql.Tx, scope nativeScope, batch *tracker.GitHubBatch, request tracker.GitHubBatchResult, now time.Time) (tracker.GitHubBatchReceipt, error) {
	receipt := tracker.GitHubBatchReceipt{}
	if batch.Revision != request.Revision {
		return receipt, nativeConflict(tracker.Revision(batch.Revision))
	}
	if err := validateBatchDiagnostic(request.Error, request.RetryAt); err != nil {
		return receipt, err
	}
	switch batch.Status {
	case "discovering":
		if request.Error != "" {
			batch.Status, batch.Error, batch.RetryAt = "discovery_failed", request.Error, request.RetryAt
		} else {
			if request.Page == nil || len(request.Page.Issues) > 100 || request.Page.Total < 0 {
				return receipt, nativeInvalid("Invalid source discovery page or plan import limit exceeded")
			}
			if batch.Automatic && request.Page.Total > batchLimit(batch) {
				batch.Status, batch.Error = "discovery_failed", "Repository exceeds the plan's per-job import limit; select a label filter or upgrade"
				batch.Page.Total = request.Page.Total
				if err := saveGitHubBatch(ctx, tx, scope, batch); err != nil {
					return receipt, err
				}
				return tracker.GitHubBatchReceipt{Revision: batch.Revision, Status: batch.Status}, nil
			}
			if len(batch.Page.Issues)+len(request.Page.Issues) > batchLimit(batch) {
				return receipt, nativeInvalid("Plan import limit exceeded")
			}
			seen, ids := map[int]bool{}, map[string]bool{}
			for _, p := range batch.Page.Issues {
				seen[p.Number], ids[p.ID] = true, true
			}
			for _, p := range request.Page.Issues {
				canonical, repository, number, err := tracker.ParseGitHubIssueURL(p.URL)
				if err != nil || repository != batch.Discovery.Repository || number != p.Number || canonical != p.URL || seen[number] || ids[p.ID] || p.ID == "" || p.Title == "" || len(p.Title) > 500 || len([]rune(p.Body)) > 1000 || p.Closed && !batch.Discovery.IncludeClosed {
					return receipt, nativeInvalid("Mismatched or duplicate source preview")
				}
				seen[number], ids[p.ID] = true, true
			}
			if request.Page.NextCursor != "" && (request.Page.NextCursor == batch.Discovery.Cursor || len(request.Page.Issues) == 0) {
				return receipt, nativeInvalid("Invalid source discovery cursor")
			}
			batch.Page.Issues = append(batch.Page.Issues, request.Page.Issues...)
			batch.Page.Total, batch.Page.NextCursor = request.Page.Total, request.Page.NextCursor
			batch.Status = "preview"
			if batch.Automatic {
				for _, preview := range request.Page.Issues {
					batch.Items = append(batch.Items, tracker.GitHubBatchItem{Number: preview.Number, Status: "pending"})
				}
				batch.Status = "importing"
			}
		}
	case "importing":
		results := request.Results
		if len(results) == 0 {
			if request.Number == 0 && request.Error != "" {
				for _, item := range batch.Items {
					if item.Status == "pending" {
						results = append(results, tracker.GitHubBatchItemResult{Number: item.Number, Error: request.Error, RetryAt: request.RetryAt})
					}
					if len(results) == 100 {
						break
					}
				}
			} else {
				results = []tracker.GitHubBatchItemResult{{Number: request.Number, Snapshot: request.Snapshot, Error: request.Error, RetryAt: request.RetryAt}}
			}
		}
		if len(results) > 100 {
			return receipt, nativeInvalid("At most 100 intake results per page")
		}
		seen := map[int]bool{}
		for _, result := range results {
			index := slices.IndexFunc(batch.Items, func(item tracker.GitHubBatchItem) bool {
				return item.Number == result.Number && item.Status == "pending"
			})
			if index < 0 || seen[result.Number] {
				return receipt, nativeInvalid("No pending selected issue")
			}
			seen[result.Number] = true
			if err := validateBatchDiagnostic(result.Error, result.RetryAt); err != nil {
				return receipt, err
			}
			item := &batch.Items[index]
			if result.Error != "" {
				item.Status, item.Error, item.RetryAt = "failed", result.Error, result.RetryAt
			} else {
				previewIndex := slices.IndexFunc(batch.Page.Issues, func(p tracker.GitHubIssuePreview) bool { return p.Number == item.Number })
				if previewIndex < 0 {
					return receipt, nativeInvalid("Intake item missing preview")
				}
				preview := batch.Page.Issues[previewIndex]
				if result.Snapshot == nil || result.Snapshot.Provenance.Provider != "github" || result.Snapshot.Provenance.ExternalID != preview.ID {
					return receipt, nativeInvalid("Snapshot stable source identity does not match preview")
				}
				if _, err := tx.ExecContext(ctx, "SAVEPOINT github_batch_item"); err != nil {
					return receipt, err
				}
				itemErr := s.importBatchItem(ctx, tx, scope, batch, item, preview, *result.Snapshot, now)
				if itemErr != nil {
					if _, err := tx.ExecContext(ctx, "ROLLBACK TO github_batch_item"); err != nil {
						return receipt, err
					}
					item.Status, item.Error = "failed", "Source issue could not be imported"
					var diagnostic *nativeError
					if errors.As(itemErr, &diagnostic) {
						item.Error = diagnostic.Message
					}
				} else {
					batch.Page.Issues[previewIndex].Snapshot = nil
				}
				if _, err := tx.ExecContext(ctx, "RELEASE github_batch_item"); err != nil {
					return receipt, err
				}
			}
			if result.RetryAt != "" {
				for i := range batch.Items {
					if batch.Items[i].Status == "pending" && !slices.ContainsFunc(results, func(r tracker.GitHubBatchItemResult) bool { return r.Number == batch.Items[i].Number }) {
						batch.Items[i].Status, batch.Items[i].Error, batch.Items[i].RetryAt = "failed", "Source intake stopped at rate limit", result.RetryAt
					}
				}
			}
		}
	default:
		return receipt, nativeInvalid("Intake has no outstanding source request")
	}
	if batch.Status == "importing" && !batchHasPending(batch) {
		if batch.Automatic && batch.Page.NextCursor != "" && !slices.ContainsFunc(batch.Items, func(item tracker.GitHubBatchItem) bool { return item.Status == "failed" && item.RetryAt != "" }) {
			batch.Discovery.Cursor = batch.Page.NextCursor
			batch.Status = "discovering"
		} else {
			batch.Status = "finished"
		}
	}
	if err := saveGitHubBatch(ctx, tx, scope, batch); err != nil {
		return receipt, err
	}
	receipt.Revision, receipt.Status = batch.Revision, batch.Status
	if batch.Status == "importing" || batch.Status == "discovering" {
		next, err := nextGitHubBatchTask(ctx, tx, scope, batch)
		if err != nil {
			return receipt, err
		}
		receipt.Next = next
	}
	receipt.Revision, receipt.Status = batch.Revision, batch.Status
	return receipt, nil
}

func validateBatchDiagnostic(message, retry string) error {
	if len(message) > 1000 {
		return nativeInvalid("Source diagnostic exceeds 1000 bytes")
	}
	if retry != "" {
		if _, err := time.Parse(time.RFC3339Nano, retry); err != nil {
			return nativeInvalid("Invalid source retry deadline")
		}
	}
	return nil
}

func batchLimit(batch *tracker.GitHubBatch) int {
	if batch.Limit > 0 {
		return batch.Limit
	}
	return 1000
}

func (s *Service) githubBatchLimit(ctx context.Context, query nativeQueryer, now time.Time) (int, error) {
	if s.database.plans() == nil {
		return 1000, nil
	}
	entitlement, err := s.database.hostedEntitlement(ctx, query, now)
	if err != nil {
		return 0, err
	}
	limit := int(min(entitlement.Allowances["unarchived_issues"], 50000))
	if limit <= 0 {
		return 0, nativeInvalid("The plan does not allow issue import")
	}
	return limit, nil
}

func configureBatchDestination(ctx context.Context, tx *sql.Tx, scope nativeScope, batch *tracker.GitHubBatch, request tracker.GitHubBatchCommand) error {
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return err
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
		return nativeInvalid("Select a destination and explicitly confirm any dispatchable lane")
	}
	if len(request.LabelLanes) > 100 {
		return nativeInvalid("At most 100 label lane mappings are allowed")
	}
	for label, name := range request.LabelLanes {
		lane, exists := findBatchLane(project.States, name)
		if label == "" || len(label) > 100 || !exists || lane.Dispatchable && !request.AllowDispatch {
			return nativeInvalid("Select a destination and explicitly confirm any dispatchable lane")
		}
	}
	batch.Destination, batch.AllowDispatch, batch.LabelLanes = destination, request.AllowDispatch, request.LabelLanes
	return nil
}

func batchIssueDestination(states []tracker.NativeState, batch *tracker.GitHubBatch, sourceLabels []string) (string, []string, error) {
	destination := batch.Destination
	labels := []string{}
	mapped := ""
	for _, label := range sourceLabels {
		name, explicit := batch.LabelLanes[label]
		laneLabel := explicit && strings.HasPrefix(strings.ToLower(label), "detent:")
		if strings.HasPrefix(strings.ToLower(label), "detent:") {
			for _, state := range states {
				if strings.EqualFold(label, "detent:"+state.Name) {
					laneLabel = true
					if !explicit {
						name = state.Name
					}
					break
				}
			}
		}
		if name != "" {
			if mapped != "" && mapped != name {
				return "", nil, nativeInvalid("Source labels map to conflicting lanes")
			}
			mapped = name
		}
		if !laneLabel {
			labels = append(labels, label)
		}
	}
	if mapped != "" {
		destination = mapped
	}
	lane, exists := findBatchLane(states, destination)
	if !exists || lane.Dispatchable && !batch.AllowDispatch {
		return "", nil, nativeInvalid("Select a destination and explicitly confirm any dispatchable lane")
	}
	return destination, labels, nil
}

func (s *Service) importBatchItem(ctx context.Context, tx *sql.Tx, scope nativeScope, batch *tracker.GitHubBatch, item *tracker.GitHubBatchItem, preview tracker.GitHubIssuePreview, snapshot tracker.GitHubIssueSnapshot, now time.Time) error {
	before, err := s.database.hostedConsumption(ctx, tx, now)
	if err != nil {
		return err
	}
	id := item.WorkItemID
	status := "completed"
	if id == "" {
		integration, err := readProjectIntegration(ctx, tx, scope)
		if err != nil {
			return err
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT native_id FROM issues WHERE organization_id=? AND project_id=? AND
   (native_source_key IN (?,?) OR github_node_id=? OR (json_extract(provenance_json, '$.provider')='github' AND json_extract(provenance_json, '$.external_id')=?) OR (repository_id=? AND github_number=?))`, scope.organization, scope.project, "github:"+preview.URL, "github:"+preview.ID, preview.ID, preview.ID, integration.RepositoryID, preview.Number).Scan(&existing)
		if err == nil {
			item.WorkItemID, item.Status = tracker.NativeWorkItemID(existing), "skipped"
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		project, err := readNativeProject(ctx, tx, scope)
		if err != nil {
			return err
		}
		destination, labels, err := batchIssueDestination(project.States, batch, preview.Labels)
		if err != nil {
			return err
		}
		creationScope := scope
		creationScope.credential.Scope = apiScopeOperator
		creationScope.sourceActor = batch.RequestedBy
		issue, err := createLinkedIssueInStateTx(ctx, tx, creationScope, tracker.CreateIssue{GitHubIssueURL: preview.URL, State: destination, Labels: labels}, now)
		if err != nil {
			return err
		}
		id = issue.WorkItemID
	}
	if _, err := intakeLinkedIssueTx(ctx, tx, scope, string(id), snapshot, now); err != nil {
		return err
	}
	if err := s.database.checkHostedGrowth(ctx, tx, before, now, false); err != nil {
		return err
	}
	item.WorkItemID, item.Status = id, status
	return nil
}
