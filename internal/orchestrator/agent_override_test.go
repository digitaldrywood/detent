package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestAgentOverrideRejectionHandlerCommentsOnce(t *testing.T) {
	t.Parallel()

	tracker := &budgetRefusalCommentConnector{}
	orchestrator := &Orchestrator{connector: tracker}
	issue := connector.Issue{ID: "issue-1124"}
	rejections := []runpkg.AgentOverrideRejection{{
		Field:  "model",
		Value:  "gpt-retired",
		Reason: "model is not available from the selected backend",
	}}

	if err := orchestrator.agentOverrideRejectionHandler(context.Background(), issue)(rejections); err != nil {
		t.Fatalf("handler error = %v", err)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one", tracker.comments)
	}
	comment := tracker.comments[0]
	for _, want := range []string{"detent-agent-rejection", "gpt-retired", "continued with the project defaults"} {
		if !strings.Contains(comment.body, want) {
			t.Fatalf("comment missing %q:\n%s", want, comment.body)
		}
	}

	issue.Comments = []connector.IssueComment{{Body: comment.body}}
	if err := orchestrator.agentOverrideRejectionHandler(context.Background(), issue)(rejections); err != nil {
		t.Fatalf("deduplicated handler error = %v", err)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments after duplicate = %#v, want one", tracker.comments)
	}
}
