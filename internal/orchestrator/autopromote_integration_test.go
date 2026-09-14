package orchestrator_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/orchestrator"
)

func TestRunAutoPromotesQuietHumanReviewIssueThroughMemoryConnector(t *testing.T) {
	t.Parallel()

	reviewedAt := time.Now().Add(-20 * time.Minute)
	issue := testIssue("issue-auto-promote", "digitaldrywood/detent#385", "Human Review")
	issue.Labels = []string{"bug"}
	issue.PullRequest = &connector.PullRequest{
		Number:                 385,
		URL:                    "https://github.com/digitaldrywood/detent/pull/385",
		State:                  "OPEN",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &reviewedAt,
	}

	events := make(chan memory.Event, 4)
	tracker := memory.New(memory.Config{
		Issues: []connector.Issue{issue},
		EventSink: func(event memory.Event) {
			events <- event
		},
	})
	orch, err := orchestrator.New(orchestrator.Config{
		PollInterval:        time.Hour,
		MaxConcurrentAgents: 1,
		AutoPromote: orchestrator.AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates: []string{"Human Review"},
		TerminalStates: []string{"Done", "Cancelled"},
	}, orchestrator.Dependencies{
		Connector: tracker,
		Runner:    &staticRunner{},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()

	var gotStateUpdate bool
	var gotComment bool
	deadline := time.After(time.Second)
	for !gotStateUpdate || !gotComment {
		select {
		case event := <-events:
			switch event.Kind {
			case memory.EventKindStateUpdate:
				if event.IssueID == issue.ID && event.State == "Merging" {
					gotStateUpdate = true
				}
			case memory.EventKindComment:
				if event.IssueID == issue.ID && strings.Contains(event.Body, "Auto-promoted this issue from Human Review to Merging.") {
					gotComment = true
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for auto-promote memory events; state_update=%v comment=%v", gotStateUpdate, gotComment)
		}
	}
}

func TestRunAutoPromoteUsesRuntimeQuietDurationUpdate(t *testing.T) {
	t.Parallel()

	reviewedAt := time.Now().Add(-5 * time.Minute)
	issue := testIssue("issue-auto-promote-hot-reload", "digitaldrywood/detent#386", "Human Review")
	issue.Labels = []string{"bug"}
	issue.PullRequest = &connector.PullRequest{
		Number:                 386,
		URL:                    "https://github.com/digitaldrywood/detent/pull/386",
		State:                  "OPEN",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &reviewedAt,
	}

	events := make(chan memory.Event, 4)
	tracker := memory.New(memory.Config{
		Issues: []connector.Issue{issue},
		EventSink: func(event memory.Event) {
			events <- event
		},
	})
	cfg := orchestrator.Config{
		PollInterval:        time.Hour,
		MaxConcurrentAgents: 1,
		AutoPromote: orchestrator.AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates: []string{"Human Review"},
		TerminalStates: []string{"Done", "Cancelled"},
	}
	orch, err := orchestrator.New(cfg, orchestrator.Dependencies{
		Connector: tracker,
		Runner:    &staticRunner{},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()

	waitForState(t, orch, func(state orchestrator.State) bool {
		return len(state.Pipeline) == 1 && state.Pipeline[0].ID == issue.ID
	})
	select {
	case event := <-events:
		t.Fatalf("unexpected auto-promote event before quiet duration update: %#v", event)
	default:
	}

	cfg.AutoPromote.QuietDuration = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := orch.UpdateConfig(ctx, cfg); err != nil {
		cancel()
		t.Fatalf("UpdateConfig() error = %v", err)
	}
	if _, err := orch.RequestRefresh(ctx); err != nil {
		cancel()
		t.Fatalf("RequestRefresh() error = %v", err)
	}
	cancel()

	deadline := time.After(time.Second)
	for {
		select {
		case event := <-events:
			if event.Kind == memory.EventKindStateUpdate && event.IssueID == issue.ID && event.State == "Merging" {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for auto-promote after quiet duration update")
		}
	}
}
