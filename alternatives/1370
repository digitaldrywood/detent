package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const githubBatchMaintenanceLimit = 100

func (s *Service) maintainGitHubBatches(ctx context.Context) error {
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	rows, err := s.database.reader.QueryContext(readCtx, `SELECT p.organization_id,p.id,b.hub_owned,b.batch_status,CASE WHEN b.hub_owned IS NULL OR b.batch_status IS NULL THEN b.request_json END FROM onboarding_issue_intake b JOIN projects p ON p.id=b.project_id
 WHERE p.deleted_at IS NULL AND b.project_id>? AND (b.batch_status IN ('discovering','importing') OR b.batch_status IS NULL) ORDER BY b.project_id LIMIT ?`, s.githubBatchCursor, githubBatchMaintenanceLimit)
	if err != nil {
		return err
	}
	defer rows.Close()
	var scopes []nativeScope
	var runnerScopes []nativeScope
	var failures []error
	visited := 0
	for rows.Next() {
		scope := nativeScope{sourceActor: nativeIntegrationActor("github")}
		scope.credential.Scope = apiScopeWorker
		var owned sql.NullBool
		var status, raw sql.NullString
		if err := rows.Scan(&scope.organization, &scope.project, &owned, &status, &raw); err != nil {
			return errors.Join(err, rows.Close())
		}
		visited++
		s.githubBatchCursor = scope.project
		if !owned.Valid || !status.Valid {
			var batch tracker.GitHubBatch
			if err := json.Unmarshal([]byte(raw.String), &batch); err != nil {
				failures = append(failures, err)
				continue
			}
			if !owned.Valid {
				owned.Bool = batch.HubOwned
			}
			if !status.Valid {
				status.String = batch.Status
			}
		}
		if !owned.Bool && status.String == "importing" {
			runnerScopes = append(runnerScopes, scope)
		} else if s.config.ImportBackend != nil && owned.Bool && (status.String == "discovering" || status.String == "importing") {
			scopes = append(scopes, scope)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if visited < githubBatchMaintenanceLimit {
		s.githubBatchCursor = ""
	}
	for _, scope := range runnerScopes {
		if err := s.completeRunnerGitHubBatch(ctx, scope); err != nil {
			failures = append(failures, err)
		}
	}
	for _, scope := range scopes {
		if err := s.advanceGitHubBatch(ctx, scope); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Service) completeRunnerGitHubBatch(ctx context.Context, scope nativeScope) error {
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	var raw string
	err := s.database.reader.QueryRowContext(readCtx, "SELECT request_json FROM onboarding_issue_intake WHERE project_id=?", scope.project).Scan(&raw)
	cancelRead()
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("read runner intake batch: %w", err)
	}
	var batch tracker.GitHubBatch
	if err := json.Unmarshal([]byte(raw), &batch); err != nil {
		return fmt.Errorf("decode runner intake batch: %w", err)
	}
	if batch.HubOwned || batch.Status != "importing" {
		return nil
	}
	visited := 0
	for i, item := range batch.Items {
		if item.Status != "pending" {
			continue
		}
		if visited == githubBatchMaintenanceLimit {
			break
		}
		visited++
		if item.WorkItemID == "" {
			continue
		}
		var complete bool
		readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
		err := s.database.reader.QueryRowContext(readCtx, "SELECT snapshot_json IS NOT NULL FROM linked_issue_sources WHERE work_item_id=?", item.WorkItemID).Scan(&complete)
		cancelRead()
		if err != nil {
			return fmt.Errorf("read intake completion: %w", err)
		}
		if !complete {
			continue
		}
		batch.Items[i].Status = "completed"
		if !batchHasPending(&batch) {
			batch.Status = "finished"
		}
		batch.Revision++
		next, err := marshalNative(batch)
		if err != nil {
			return fmt.Errorf("encode runner intake batch: %w", err)
		}
		result, err := s.database.db.ExecContext(ctx, `UPDATE onboarding_issue_intake SET request_json=?,hub_owned=?,batch_status=? WHERE project_id=? AND request_json=?`, next, batch.HubOwned, batch.Status, scope.project, raw)
		if err != nil {
			return fmt.Errorf("complete runner intake item: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read intake completion result: %w", err)
		}
		if changed == 0 {
			return nil
		}
		raw = next
	}
	return nil
}

func (s *Service) advanceGitHubBatch(ctx context.Context, scope nativeScope) error {
	ctx = s.linkedSourceContext(ctx)
	for range githubBatchMaintenanceLimit {
		// Each iteration reads back the batch state written by the previous iteration.
		batch, err := readGitHubBatch(ctx, s.database.db, scope)
		if err != nil || batch == nil {
			return err
		}
		if !batch.HubOwned || !batchHasPending(batch) {
			return nil
		}
		readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
		integration, err := readProjectIntegration(readCtx, s.database.reader, scope)
		cancelRead()
		if err != nil {
			return err
		}
		if !integration.ManualImportEnabled {
			return nil
		}
		request := tracker.GitHubBatchResult{BatchID: batch.ID, Revision: batch.Revision}
		var sourceErr error
		fallback := false
		if batch.Status == "discovering" {
			page, err := s.config.ImportBackend.FetchImportPage(ctx, GitHubImportRequest{Profile: "native", Repository: batch.Discovery.Repository, Stage: "batch", Discovery: &batch.Discovery})
			sourceErr = err
			request.Page = page.BatchPage
			if sourceErr == nil && request.Page == nil {
				sourceErr = errors.New("GitHub App returned no issue page")
			}
			if sourceErr != nil {
				fallback = githubBatchAccessDenied(sourceErr)
				request.Error = "GitHub App import transport is unavailable"
				request.RetryAt = batchSourceRetry(sourceErr)
				if !page.RetryAt.IsZero() {
					request.RetryAt = page.RetryAt.UTC().Format(time.RFC3339Nano)
				}
			}
		} else {
			for _, item := range batch.Items {
				if item.Status != "pending" {
					continue
				}
				var snapshot *tracker.GitHubIssueSnapshot
				for _, preview := range batch.Page.Issues {
					if preview.Number == item.Number {
						snapshot = preview.Snapshot
						break
					}
				}
				result := tracker.GitHubBatchItemResult{Number: item.Number, Snapshot: snapshot}
				if snapshot == nil {
					var url string
					for _, preview := range batch.Page.Issues {
						if preview.Number == item.Number {
							url = preview.URL
							break
						}
					}
					snapshot, err = s.fetchLinkedSnapshot(ctx, scope, url)
					result.Snapshot = snapshot
					if err != nil {
						result.Error = "GitHub App import transport is unavailable"
						result.RetryAt = batchSourceRetry(err)
						fallback = githubBatchAccessDenied(err)
					}
				}
				request.Results = append(request.Results, result)
				if len(request.Results) == 100 || result.RetryAt != "" || fallback {
					break
				}
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		mutationCtx, err := s.prepareHostedImportMutation(ctx, scope, "", request)
		if err != nil {
			return err
		}
		tx, err := s.database.db.BeginTx(mutationCtx, nil)
		if err != nil {
			return err
		}
		err = func(ctx context.Context) error {
			latest, err := readGitHubBatch(ctx, tx, scope)
			if err != nil {
				return err
			}
			if latest == nil || latest.ID != batch.ID || latest.Revision != batch.Revision {
				return nativeConflict(tracker.Revision(batch.Revision))
			}
			active, err := readProjectIntegration(ctx, tx, scope)
			if err != nil {
				return err
			}
			if !active.ManualImportEnabled || active.Revision != integration.Revision {
				return nativeConflict(active.Revision)
			}
			if err := s.database.requireHostedFeature(ctx, tx, "github_integration", s.config.now()); err != nil {
				return err
			}
			if fallback && latest.RunnerID == "" {
				latest.RunnerID, err = findBatchRunner(ctx, tx, scope, batch.Discovery.Repository, s.config.now())
				if err != nil {
					return err
				}
			}
			if fallback && latest.RunnerID != "" && requireBatchRunner(ctx, tx, scope, latest.RunnerID, batch.Discovery.Repository, s.config.now()) == nil {
				latest.HubOwned = false
				return saveGitHubBatch(ctx, tx, scope, latest)
			}
			_, err = s.applyGitHubBatchResult(ctx, tx, scope, latest, request, s.config.now())
			return err
		}(mutationCtx)
		if err == nil {
			err = s.database.commit(mutationCtx, tx)
		}
		rollbackErr := tx.Rollback()
		if !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
		if err != nil {
			return err
		}
		s.notifyNativeDispatch(ctx, scope, request)
	}
	return nil
}

func githubBatchAccessDenied(err error) bool {
	if errors.Is(err, githubconnector.ErrNotFound) || errors.Is(err, githubconnector.ErrAuthenticationFailed) {
		return true
	}
	var status *githubconnector.StatusError
	return errors.As(err, &status) && status != nil && (status.StatusCode == http.StatusNotFound || status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusForbidden && status.RetryAfter == 0 && status.ResetAt.IsZero() && !errors.Is(err, githubconnector.ErrRateLimited))
}

func batchSourceRetry(err error) string {
	var status *githubconnector.StatusError
	if errors.As(err, &status) && status != nil && (status.RetryAfter > 0 || !status.ResetAt.IsZero() || errors.Is(err, githubconnector.ErrRateLimited)) {
		deadline := time.Now().Add(max(time.Minute, status.RetryAfter))
		if status.ResetAt.After(deadline) {
			deadline = status.ResetAt
		}
		return deadline.UTC().Format(time.RFC3339Nano)
	}
	if errors.Is(err, githubconnector.ErrRateLimited) {
		return time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	}
	return ""
}

func findBatchRunner(ctx context.Context, tx *sql.Tx, scope nativeScope, repository string, now time.Time) (string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT runner_id FROM runner_checkout_repositories WHERE project_id=? AND lower(repository)=lower(?) ORDER BY reported_at DESC", scope.project, repository)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var runners []string
	for rows.Next() {
		var runner string
		if err := rows.Scan(&runner); err != nil {
			return "", errors.Join(err, rows.Close())
		}
		runners = append(runners, runner)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", err
	}
	for _, runner := range runners {
		if requireBatchRunner(ctx, tx, scope, runner, repository, now) == nil {
			return runner, nil
		}
	}
	return "", nil
}
