package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Scheduler) startGitHubBatch(ctx context.Context, client *NativeClient, task tracker.GitHubBatchTask) <-chan struct{} {
	s.mu.Lock()
	if s.githubBatchRunning {
		s.mu.Unlock()
		return nil
	}
	s.githubBatchRunning = true
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			s.mu.Lock()
			s.githubBatchRunning = false
			s.mu.Unlock()
		}()
		if err := s.processGitHubBatch(context.WithoutCancel(ctx), client, task); err != nil {
			slog.Default().Warn("github intake failed", "project_id", task.ProjectID, "batch_id", task.BatchID, "revision", task.Revision, "error", err)
		}
	}()
	return done
}

func (s *Scheduler) processGitHubBatch(ctx context.Context, client *NativeClient, task tracker.GitHubBatchTask) error {
	if task.ProjectID != client.project {
		return errors.New("intake request project does not match heartbeat")
	}
	for {
		receipt, err := s.processGitHubBatchPage(ctx, client, task)
		if err != nil {
			return err
		}
		if receipt.Next == nil {
			return nil
		}
		task = *receipt.Next
	}
}

func (s *Scheduler) processGitHubBatchPage(ctx context.Context, client *NativeClient, task tracker.GitHubBatchTask) (tracker.GitHubBatchReceipt, error) {
	receipt := tracker.GitHubBatchReceipt{}
	if task.ProjectID != client.project {
		return receipt, errors.New("intake request project does not match heartbeat")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
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
	case len(task.Items) > 0 && task.Discovery == nil:
		if len(task.Items) > 100 {
			return receipt, errors.New("intake page exceeds 100 issues")
		}
		for _, item := range task.Items {
			entry := tracker.GitHubBatchItemResult{Number: item.Number, Snapshot: item.Snapshot}
			var err error
			if entry.Snapshot == nil {
				if s.githubIntake == nil {
					err = errors.New("runner GitHub read access is not configured")
				} else {
					snapshot, readErr := s.githubIntake(ctx, item.URL)
					err = readErr
					if err == nil {
						entry.Snapshot = &snapshot
					}
				}
			}
			if err != nil {
				entry.Error, entry.RetryAt = githubBatchDiagnostic(err)
			}
			result.Results = append(result.Results, entry)
			if entry.RetryAt != "" {
				for _, remaining := range task.Items[len(result.Results):] {
					result.Results = append(result.Results, tracker.GitHubBatchItemResult{Number: remaining.Number, Error: entry.Error, RetryAt: entry.RetryAt})
				}
				break
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
		return receipt, errors.New("invalid onboarding source request")
	}
	if result.Snapshot != nil || len(result.Results) > 0 || result.Page != nil {
		payload, err := json.Marshal(result)
		if err != nil {
			return receipt, err
		}
		if len(payload) > 64<<20 {
			result.Snapshot, result.Results, result.Page = nil, nil, nil
			sourceErr = errors.New("snapshot exceeds Hub intake size limit")
		}
	}
	if sourceErr != nil {
		result.Error, result.RetryAt = githubBatchDiagnostic(sourceErr)
	}
	err := client.client.request(ctx, http.MethodPost, client.base()+"/onboarding/issue-intake/result", result, &receipt)
	return receipt, err
}

func githubBatchDiagnostic(sourceErr error) (string, string) {
	message := fmt.Sprintf("Source context incomplete: %v", sourceErr)
	if len(message) > 1000 {
		message = message[:1000]
	}
	retry := ""
	var status *githubconnector.StatusError
	if errors.As(sourceErr, &status) && (status.RetryAfter > 0 || !status.ResetAt.IsZero()) {
		deadline := time.Now().Add(status.RetryAfter)
		if status.ResetAt.After(deadline) {
			deadline = status.ResetAt
		}
		retry = deadline.UTC().Format(time.RFC3339Nano)
	} else if errors.Is(sourceErr, githubconnector.ErrRateLimited) {
		retry = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	}
	return message, retry
}
