package orchestrator_test

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
)

func TestCompletionRefillsProjectSlotWithoutRefresh(t *testing.T) {
	for _, tt := range []struct {
		name       string
		staleKind  string
		wantNextID string
	}{
		{name: "ready next candidate", wantNextID: "second"},
		{name: "changed lane skipped", staleKind: "lane", wantNextID: "third"},
		{name: "changed Workpad skipped", staleKind: "workpad", wantNextID: "third"},
		{name: "changed dependency skipped", staleKind: "dependency", wantNextID: "third"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			first := testIssue("first", "digitaldrywood/detent#10", "Todo")
			second := testIssue("second", "digitaldrywood/detent#11", "Todo")
			candidates := []connector.Issue{first, second}
			if tt.staleKind != "" {
				candidates = append(candidates, testIssue("third", "digitaldrywood/detent#12", "Todo"))
			}
			tracker := newFakeConnector(candidates...)
			runner := newBlockingRunner()
			orch := newTestOrchestratorWithPollInterval(t, tracker, runner, time.Hour)
			stop := runOrchestrator(t, orch)
			defer stop()

			select {
			case request := <-runner.started:
				if request.Issue.ID != first.ID {
					t.Fatalf("first run = %q, want %q", request.Issue.ID, first.ID)
				}
			case <-time.After(time.Second):
				t.Fatal("first run did not start")
			}
			tracker.mu.Lock()
			tracker.candidates[0].State = "Done"
			switch tt.staleKind {
			case "lane":
				tracker.candidates[1].State = "Done"
			case "dependency":
				tracker.candidates[1].DependencySource = connector.BlockedRefSourceNative
				tracker.candidates[1].BlockedBy = []connector.BlockedRef{{ID: "blocked", Identifier: "digitaldrywood/detent#99", State: "In Progress"}}
			}
			tracker.mu.Unlock()
			if tt.staleKind == "workpad" {
				tracker.setIssueComments(second.ID, []connector.IssueComment{{Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: needs operator input\n```"}})
			}
			close(runner.release)
			select {
			case request := <-runner.started:
				if request.Issue.ID != tt.wantNextID {
					t.Fatalf("next run = %q, want %q", request.Issue.ID, tt.wantNextID)
				}
			case <-time.After(300 * time.Millisecond):
				t.Fatal("freed project slot waited for next refresh")
			}
		})
	}
}

func TestCapacityIncreaseRefillsWithoutRefresh(t *testing.T) {
	first := testIssue("first", "digitaldrywood/detent#10", "Todo")
	second := testIssue("second", "digitaldrywood/detent#11", "Todo")
	tracker := newFakeConnector(first, second)
	runner := newBlockingRunner()
	orch := newTestOrchestratorWithPollInterval(t, tracker, runner, time.Hour)
	stop := runOrchestrator(t, orch)
	defer stop()
	defer close(runner.release)

	select {
	case request := <-runner.started:
		if request.Issue.ID != first.ID {
			t.Fatalf("first run = %q, want %q", request.Issue.ID, first.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("first run did not start")
	}
	if err := orch.UpdateConfig(context.Background(), orchestrator.Config{
		PollInterval: time.Hour, MaxConcurrentAgents: 2,
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done", "Cancelled", "Canceled", "Closed"},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-runner.started:
		if request.Issue.ID != second.ID {
			t.Fatalf("new slot run = %q, want %q", request.Issue.ID, second.ID)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("capacity increase waited for next refresh")
	}
}
