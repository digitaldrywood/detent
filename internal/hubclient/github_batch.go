package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Scheduler) processGitHubBatch(ctx context.Context, client *NativeClient, task tracker.GitHubBatchTask) error {
	if task.ProjectID != client.project {
		return errors.New("intake request project does not match heartbeat")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result := tracker.GitHubBatchResult{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("%s-%d", task.BatchID, task.Revision)}, BatchID: task.BatchID, Revision: task.Revision}
	var sourceErr error
	switch {
	case task.Discovery != nil && task.Item == nil:
		if s.githubDiscovery == nil {
			sourceErr = errors.New("runner GitHub read access is not configured")
		} else {
			page, err := s.githubDiscovery(ctx, *task.Discovery)
			sourceErr = err
			if err == nil {
				result.Page = &page
			}
		}
	case task.Item != nil && task.Discovery == nil:
		result.Number = task.Item.Number
		if s.githubIntake == nil {
			sourceErr = errors.New("runner GitHub read access is not configured")
		} else {
			snapshot, err := s.githubIntake(ctx, task.Item.URL)
			sourceErr = err
			if err == nil {
				result.Snapshot = &snapshot
			}
		}
	default:
		return errors.New("invalid onboarding source request")
	}
	if result.Snapshot != nil {
		payload, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if len(payload) > 64<<20 {
			result.Snapshot = nil
			sourceErr = errors.New("snapshot exceeds Hub intake size limit")
		}
	}
	if sourceErr != nil {
		result.Error = fmt.Sprintf("Source context incomplete: %v", sourceErr)
		if len(result.Error) > 1000 {
			result.Error = result.Error[:1000]
		}
		var status *githubconnector.StatusError
		if errors.As(sourceErr, &status) && (status.RetryAfter > 0 || !status.ResetAt.IsZero()) {
			deadline := time.Now().Add(status.RetryAfter)
			if status.ResetAt.After(deadline) {
				deadline = status.ResetAt
			}
			result.RetryAt = deadline.UTC().Format(time.RFC3339Nano)
		} else if errors.Is(sourceErr, githubconnector.ErrRateLimited) {
			result.RetryAt = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
		}
	}
	return client.client.request(ctx, http.MethodPost, client.base()+"/onboarding/issue-intake/result", result, nil)
}
