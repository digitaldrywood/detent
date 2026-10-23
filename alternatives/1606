package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) maintainGitHubBatches(ctx context.Context) error {
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	if s.config.ImportBackend == nil {
		return nil
	}
	rows, err := s.database.reader.QueryContext(readCtx, `SELECT p.organization_id,p.id FROM onboarding_issue_intake b JOIN projects p ON p.id=b.project_id
 WHERE p.deleted_at IS NULL AND json_extract(b.request_json,'$.hub_owned')=1 AND json_extract(b.request_json,'$.status') IN ('discovering','importing')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var scopes []nativeScope
	for rows.Next() {
		scope := nativeScope{sourceActor: nativeIntegrationActor("github")}
		scope.credential.Scope = apiScopeWorker
		if err := rows.Scan(&scope.organization, &scope.project); err != nil {
			return errors.Join(err, rows.Close())
		}
		scopes = append(scopes, scope)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, scope := range scopes {
		if err := s.advanceGitHubBatch(ctx, scope); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) advanceGitHubBatch(ctx context.Context, scope nativeScope) error {
	ctx = s.linkedSourceContext(ctx)
	for {
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
		tx, err := s.database.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		err = func() error {
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
		}()
		if err == nil {
			err = s.database.commit(ctx, tx)
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
