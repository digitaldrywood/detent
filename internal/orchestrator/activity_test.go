package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/activity"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestActivityUpdateHandlerPublishesIssueSessionContent(t *testing.T) {
	t.Parallel()

	broker := activity.NewBroker()
	orchestrator := &Orchestrator{
		cfg:      Config{Project: scheduler.ProjectCandidate{ID: "detent"}},
		activity: broker,
	}
	issue := connector.Issue{ID: "issue-1156", Identifier: "digitaldrywood/detent#1156"}
	key := activity.Key{ProjectID: "detent", IssueID: issue.ID}
	subscription := broker.Subscribe(context.Background(), key)
	t.Cleanup(subscription.Close)

	err := orchestrator.activityUpdateHandler(context.Background(), issue)(runpkg.AgentActivityUpdate{
		At:              time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC),
		DetentSessionID: 1156,
		Type:            runpkg.AgentUpdateToolOutput,
		Tool:            "exec_command",
		Content:         "ok package",
	})
	if err != nil {
		t.Fatalf("activityUpdateHandler() error = %v", err)
	}

	select {
	case event := <-subscription.C():
		if event.DetentSessionID != 1156 || event.Kind != "tool_output" || event.Title != "Tool output · exec_command" || event.Content != "ok package" {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for activity event")
	}
}
