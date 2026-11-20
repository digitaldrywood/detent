package orchestrator

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

// publishCompletedPlan reconciles the remote marker as well as the durable
// receipt, including a crash after publication but before saving that receipt.
func (o *Orchestrator) publishCompletedPlan(ctx context.Context, event runpkg.Completion, running *Running) error {
	if running.PlanArtifactPublished {
		return nil
	}
	identity := fmt.Sprintf("%s\n%s\n%s\n%d\n%d\n%s", event.Request.ProjectID, event.IssueID, running.WorkerHost, running.WorkAttemptID, running.Generation, running.StartedAt.UTC().Format(time.RFC3339Nano))
	marker := fmt.Sprintf("<!-- detent-plan-completion:%x -->", sha256.Sum256([]byte(identity)))
	body := planArtifactComment(running.Issue, event.Result.Output)
	var comments []connector.IssueComment
	if reader, ok := o.connector.(connector.IssueCommentReader); ok {
		var err error
		comments, err = reader.FetchIssueComments(ctx, running.Issue)
		if err != nil {
			return err
		}
	}
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) {
			running.PlanArtifactPublished = true
			return nil
		}
	}
	if err := o.connector.CreateComment(ctx, event.IssueID, body+"\n"+marker); err != nil {
		return err
	}
	running.PlanArtifactPublished = true
	return nil
}
