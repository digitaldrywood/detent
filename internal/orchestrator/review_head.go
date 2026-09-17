package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
)

func (o *Orchestrator) requestAutomatedReview(ctx context.Context, issue connector.Issue) {
	if !requiresAutomatedReview(o.cfg) || issue.PullRequest == nil || strings.TrimSpace(issue.PullRequest.HeadSHA) == "" || issue.PullRequest.Draft {
		return
	}
	if err := o.publishAutomatedReviewRequest(ctx, issue); err != nil && o.logger != nil {
		o.logger.Warn("request current-head automated review", "issue_id", issue.ID, "error", err)
	}
}

func (o *Orchestrator) publishAutomatedReviewRequest(ctx context.Context, issue connector.Issue) error {
	reader, ok := o.connector.(connector.PullRequestCommentReader)
	if !ok {
		return errors.New("pull request comment reader unavailable")
	}
	commenter, ok := o.connector.(connector.PullRequestCommenter)
	if !ok {
		return errors.New("pull request commenter unavailable")
	}
	repository, number := pullRequestRepository(issue), pullRequestNumber(issue)
	marker := "<!-- detent:automated-review-head:" + strings.TrimSpace(issue.PullRequest.HeadSHA) + " -->"
	comments, err := reader.FetchPullRequestComments(ctx, repository, number)
	if err != nil {
		return fmt.Errorf("read automated review requests: %w", err)
	}
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) {
			return nil
		}
	}
	if err := commenter.CreatePullRequestComment(ctx, repository, number, "@codex review\n\n"+marker); err != nil {
		return fmt.Errorf("request automated review: %w", err)
	}
	return nil
}

func requiresAutomatedReview(cfg Config) bool {
	required := gate.Effective(cfg.AutoPromote.Gate).RequireAutomatedReview
	return required != nil && *required
}
