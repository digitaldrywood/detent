package operatortool

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestCatalogDefinesReadOnlySurface(t *testing.T) {
	t.Parallel()

	definitions := Catalog()
	wantNames := []string{BoardState, FleetHealth, TelemetryUsage, RecentActivity, ExplainItem}
	if len(definitions) != len(wantNames) {
		t.Fatalf("Catalog() length = %d, want %d", len(definitions), len(wantNames))
	}
	for index, definition := range definitions {
		if definition.Name != wantNames[index] {
			t.Fatalf("Catalog()[%d].Name = %q, want %q", index, definition.Name, wantNames[index])
		}
		var schema map[string]any
		if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
			t.Fatalf("Catalog()[%d] schema error = %v", index, err)
		}
		if strings.HasPrefix(definition.Name, "propose_") {
			t.Fatalf("Catalog() exposed mutation %q", definition.Name)
		}
	}
	if !strings.Contains(definitions[3].Description, "live-only") || !strings.Contains(definitions[3].Description, "not the durable") {
		t.Fatalf("recent_activity description = %q", definitions[3].Description)
	}
	var boardSchema struct {
		Properties struct {
			Limit struct {
				Maximum int `json:"maximum"`
			} `json:"limit"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(definitions[0].InputSchema, &boardSchema); err != nil {
		t.Fatalf("decode board schema: %v", err)
	}
	if boardSchema.Properties.Limit.Maximum != MaxItemLimit {
		t.Fatalf("board limit maximum = %d, want %d", boardSchema.Properties.Limit.Maximum, MaxItemLimit)
	}
}

func TestExecutorReturnsTypedResultsWithObservationTime(t *testing.T) {
	t.Parallel()

	observedAt := time.Date(2026, 8, 7, 20, 15, 0, 0, time.UTC)
	explanation := explain.IssueExplanation{
		Schema:     explain.SchemaVersion,
		Found:      true,
		ObservedAt: observedAt,
		Identity:   explain.Identity{ProjectID: "detent", IssueID: "issue-1", Identifier: "owner/repo#1"},
		CurrentLane: explain.Lane{
			Name:      "Rework",
			Freshness: explain.SourceLive,
		},
	}
	snapshot := telemetry.Snapshot{
		GeneratedAt: observedAt,
		BoardIssues: []telemetry.Issue{{ID: "issue-1", Identifier: "owner/repo#1", ProjectID: "detent", State: "Rework"}},
		Events:      []telemetry.ActivityEvent{{At: observedAt, Event: "agent finished"}},
		Completed:   []telemetry.Completed{{Issue: telemetry.Issue{ID: "issue-1", ProjectID: "detent"}, CompletedAt: observedAt}},
	}
	executor := newTestExecutor(snapshot, explanation)
	tests := []struct {
		name      string
		call      Call
		timestamp string
	}{
		{name: "board state", call: Call{Name: BoardState, Arguments: json.RawMessage(`{"project_id":"detent"}`)}, timestamp: "generated_at"},
		{name: "fleet health", call: Call{Name: FleetHealth, Arguments: json.RawMessage(`{}`)}, timestamp: "generated_at"},
		{name: "telemetry usage", call: Call{Name: TelemetryUsage, Arguments: json.RawMessage(`{}`)}, timestamp: "generated_at"},
		{name: "recent activity", call: Call{Name: RecentActivity, Arguments: json.RawMessage(`{"limit":1}`)}, timestamp: "generated_at"},
		{name: "explain item", call: Call{Name: ExplainItem, Arguments: json.RawMessage(`{"project_id":"detent","reference":"owner/repo#1"}`)}, timestamp: "observed_at"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result, err := executor.Execute(context.Background(), test.call)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			var payload any
			if err := json.Unmarshal(result.Content, &payload); err != nil {
				t.Fatalf("result JSON error = %v", err)
			}
			object, ok := payload.(map[string]any)
			if !ok {
				t.Fatalf("result payload = %T, want JSON object", payload)
			}
			if object[test.timestamp] != observedAt.Format(time.RFC3339) {
				t.Fatalf("result %s = %#v, want %q", test.timestamp, object[test.timestamp], observedAt.Format(time.RFC3339))
			}
		})
	}

	result, err := executor.Execute(context.Background(), Call{Name: ExplainItem, Arguments: json.RawMessage(`{"project_id":"detent","reference":"owner/repo#1"}`)})
	if err != nil {
		t.Fatalf("Execute(explain_item) error = %v", err)
	}
	var got explain.IssueExplanation
	if err := json.Unmarshal(result.Content, &got); err != nil {
		t.Fatalf("decode explain result: %v", err)
	}
	if !reflect.DeepEqual(got, explanation) {
		t.Fatalf("explain result = %#v, want read model %#v", got, explanation)
	}
}

func TestExecutorEnforcesDispatchAndBounds(t *testing.T) {
	t.Parallel()

	observedAt := time.Date(2026, 8, 7, 20, 15, 0, 0, time.UTC)
	issues := make([]telemetry.Issue, MaxItemLimit+5)
	for index := range issues {
		identifier := strconv.Itoa(index)
		issues[index] = telemetry.Issue{ID: "issue-" + identifier, Identifier: "owner/repo#" + identifier, ProjectID: "detent", State: "Todo"}
	}
	contributors := make([]telemetry.GraphQLCostContributor, MaxItemLimit+5)
	spendPoints := make([]telemetry.BudgetSpendPoint, MaxItemLimit+5)
	executor := newTestExecutor(telemetry.Snapshot{
		GeneratedAt: observedAt,
		BoardIssues: issues,
		RateLimits:  &telemetry.RateLimits{GraphQLCost: &telemetry.GraphQLCost{Contributors: contributors}},
		Budget:      telemetry.Budget{SpendPoints: spendPoints},
	}, explain.IssueExplanation{})
	unsupported := []string{"propose_move_item", "propose_set_priority", "unknown"}
	for _, name := range unsupported {
		t.Run("reject "+name, func(t *testing.T) {
			t.Parallel()
			_, err := executor.Execute(context.Background(), Call{Name: name})
			if err == nil || !strings.Contains(err.Error(), "unknown read-only operator tool") {
				t.Fatalf("Execute(%q) error = %v", name, err)
			}
		})
	}

	result, err := executor.Execute(context.Background(), Call{Name: BoardState, Arguments: json.RawMessage(`{"limit":999}`)})
	if err != nil {
		t.Fatalf("Execute(board_state) error = %v", err)
	}
	var board BoardStateResult
	if err := json.Unmarshal(result.Content, &board); err != nil {
		t.Fatalf("decode board result: %v", err)
	}
	if len(board.Items) != MaxItemLimit {
		t.Fatalf("board items = %d, want %d", len(board.Items), MaxItemLimit)
	}
	result, err = executor.Execute(context.Background(), Call{Name: FleetHealth})
	if err != nil {
		t.Fatalf("Execute(fleet_health) error = %v", err)
	}
	var fleet FleetHealthResult
	if err := json.Unmarshal(result.Content, &fleet); err != nil {
		t.Fatalf("decode fleet result: %v", err)
	}
	if len(fleet.RateLimits.GraphQLCost.Contributors) != MaxItemLimit {
		t.Fatalf("fleet contributors = %d, want %d", len(fleet.RateLimits.GraphQLCost.Contributors), MaxItemLimit)
	}
	result, err = executor.Execute(context.Background(), Call{Name: TelemetryUsage})
	if err != nil {
		t.Fatalf("Execute(telemetry_usage) error = %v", err)
	}
	var usage TelemetryUsageResult
	if err := json.Unmarshal(result.Content, &usage); err != nil {
		t.Fatalf("decode usage result: %v", err)
	}
	if len(usage.Budget.SpendPoints) != MaxItemLimit {
		t.Fatalf("budget spend points = %d, want %d", len(usage.Budget.SpendPoints), MaxItemLimit)
	}

	oversized := newTestExecutor(telemetry.Snapshot{
		GeneratedAt: observedAt,
		BoardIssues: []telemetry.Issue{{ID: "issue-1", ProjectID: "detent", Title: strings.Repeat("x", MaxResultBytes)}},
	}, explain.IssueExplanation{})
	_, err = oversized.Execute(context.Background(), Call{Name: BoardState})
	if err == nil || !strings.Contains(err.Error(), "result exceeds") {
		t.Fatalf("oversized Execute() error = %v", err)
	}
}

func TestExecutorAggregatesAndFiltersSnapshotResults(t *testing.T) {
	t.Parallel()

	observedAt := time.Date(2026, 8, 7, 20, 15, 0, 0, time.UTC)
	completedAt := observedAt.Add(-time.Minute)
	snapshot := telemetry.Snapshot{
		GeneratedAt: observedAt,
		BoardIssues: []telemetry.Issue{{ID: "board", Identifier: "owner/repo#1", ProjectID: "detent", State: "Todo"}},
		Pipeline:    []telemetry.Issue{{ID: "pipeline", Identifier: "owner/repo#2", ProjectID: "detent", State: "Todo"}},
		Running: []telemetry.Running{{
			Issue:   telemetry.Issue{ID: "running", Identifier: "owner/repo#3", ProjectID: "detent", State: "Todo"},
			Attempt: 2, WorkAttemptID: 3, DetentSessionID: 4, SessionID: "provider-5",
		}},
		Queue: []telemetry.Queued{{Issue: telemetry.Issue{ID: "queued", Identifier: "owner/repo#4", ProjectID: "detent", State: "Todo"}}},
		Blocked: []telemetry.Blocked{{
			Issue: telemetry.Issue{ID: "blocked", Identifier: "owner/repo#5", ProjectID: "detent", State: "Todo"},
			Error: "waiting for dependency", Source: telemetry.BlockedSourceDependency,
		}},
		Completed: []telemetry.Completed{
			{Issue: telemetry.Issue{ID: "completed", Identifier: "owner/repo#6", ProjectID: "detent", State: "Done"}, CompletedAt: completedAt},
			{Issue: telemetry.Issue{ID: "other", Identifier: "other/repo#1", ProjectID: "other", State: "Done"}, CompletedAt: completedAt},
		},
		Events: []telemetry.ActivityEvent{
			{At: observedAt.Add(-2 * time.Minute), Event: "old"},
			{At: observedAt.Add(-time.Minute), Event: "middle"},
			{At: observedAt, Event: "new"},
		},
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "detent"}},
			{Project: telemetry.Project{ID: "other"}},
		},
	}
	executor := newTestExecutor(snapshot, explain.IssueExplanation{})

	result, err := executor.Execute(context.Background(), Call{Name: BoardState, Arguments: json.RawMessage(`{"project_id":"detent"}`)})
	if err != nil {
		t.Fatalf("Execute(board_state) error = %v", err)
	}
	var board BoardStateResult
	if err := json.Unmarshal(result.Content, &board); err != nil {
		t.Fatalf("decode board result: %v", err)
	}
	if len(board.Items) != 6 || board.Items[2].Identifier != "owner/repo#3" || !board.Items[2].Active || board.Items[2].ProviderSessionID != "provider-5" {
		t.Fatalf("board items = %#v", board.Items)
	}
	if board.Items[4].State != "Blocked" || board.Items[4].BlockedReason != "waiting for dependency" {
		t.Fatalf("blocked item = %#v", board.Items[4])
	}
	if board.Items[5].CompletedAt == nil || !board.Items[5].CompletedAt.Equal(completedAt) {
		t.Fatalf("completed item = %#v", board.Items[5])
	}

	result, err = executor.Execute(context.Background(), Call{Name: TelemetryUsage, Arguments: json.RawMessage(`{"project_id":"detent"}`)})
	if err != nil {
		t.Fatalf("Execute(telemetry_usage) error = %v", err)
	}
	var usage TelemetryUsageResult
	if err := json.Unmarshal(result.Content, &usage); err != nil {
		t.Fatalf("decode usage result: %v", err)
	}
	if len(usage.Projects) != 1 || usage.Projects[0].Project.ID != "detent" {
		t.Fatalf("usage projects = %#v", usage.Projects)
	}

	result, err = executor.Execute(context.Background(), Call{Name: RecentActivity, Arguments: json.RawMessage(`{"project_id":"detent","limit":2}`)})
	if err != nil {
		t.Fatalf("Execute(recent_activity) error = %v", err)
	}
	var activity RecentActivityResult
	if err := json.Unmarshal(result.Content, &activity); err != nil {
		t.Fatalf("decode activity result: %v", err)
	}
	if len(activity.Events) != 2 || activity.Events[0].Event != "middle" || len(activity.Completed) != 1 || activity.Completed[0].ProjectID != "detent" {
		t.Fatalf("activity result = %#v", activity)
	}
}

func TestExecutorReportsInvalidInputsAndUnavailableDependencies(t *testing.T) {
	t.Parallel()

	dependencyErr := errors.New("dependency failed")
	tests := []struct {
		name     string
		executor *Executor
		call     Call
		want     string
	}{
		{name: "invalid board arguments", executor: newTestExecutor(telemetry.Snapshot{}, explain.IssueExplanation{}), call: Call{Name: BoardState, Arguments: json.RawMessage(`{"limit":`)}, want: "invalid tool arguments"},
		{name: "missing snapshot", executor: NewExecutor(Dependencies{}), call: Call{Name: BoardState}, want: "snapshot is unavailable"},
		{name: "snapshot failure", executor: NewExecutor(Dependencies{Snapshots: failingSnapshot{err: dependencyErr}}), call: Call{Name: FleetHealth}, want: dependencyErr.Error()},
		{name: "missing explainer", executor: NewExecutor(Dependencies{}), call: Call{Name: ExplainItem}, want: "explanation is unavailable"},
		{name: "explainer failure", executor: NewExecutor(Dependencies{Explainer: failingExplainer{err: dependencyErr}}), call: Call{Name: ExplainItem}, want: dependencyErr.Error()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := test.executor.Execute(context.Background(), test.call)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Execute() error = %v, want containing %q", err, test.want)
			}
		})
	}
	if _, err := encodeResult(math.Inf(1)); err == nil || !strings.Contains(err.Error(), "encode operator tool result") {
		t.Fatalf("encodeResult() error = %v", err)
	}
}

func TestExecutorConcurrentCallsAreSafe(t *testing.T) {
	t.Parallel()

	observedAt := time.Date(2026, 8, 7, 20, 15, 0, 0, time.UTC)
	executor := newTestExecutor(
		telemetry.Snapshot{GeneratedAt: observedAt, BoardIssues: []telemetry.Issue{{ID: "issue-1", ProjectID: "detent", State: "Todo"}}},
		explain.IssueExplanation{Schema: explain.SchemaVersion, Found: true, ObservedAt: observedAt},
	)
	calls := []Call{
		{Name: BoardState},
		{Name: FleetHealth},
		{Name: TelemetryUsage},
		{Name: RecentActivity},
		{Name: ExplainItem, Arguments: json.RawMessage(`{"project_id":"detent","reference":"issue-1"}`)},
	}
	const workers = 32
	errorsFound := make(chan error, workers*len(calls))
	var group sync.WaitGroup
	for range workers {
		for _, call := range calls {
			group.Add(1)
			go func() {
				defer group.Done()
				result, err := executor.Execute(context.Background(), call)
				if err != nil {
					errorsFound <- err
					return
				}
				if !json.Valid(result.Content) {
					errorsFound <- errors.New("result is not valid JSON")
				}
			}()
		}
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("concurrent Execute() error = %v", err)
	}
}

type staticExplainer struct {
	result explain.IssueExplanation
}

type failingSnapshot struct {
	err error
}

func (s failingSnapshot) Snapshot(context.Context) (telemetry.Snapshot, error) {
	return telemetry.Snapshot{}, s.err
}

type failingExplainer struct {
	err error
}

func (e failingExplainer) Explain(context.Context, explain.Query) (explain.IssueExplanation, error) {
	return explain.IssueExplanation{}, e.err
}

func (e staticExplainer) Explain(context.Context, explain.Query) (explain.IssueExplanation, error) {
	return e.result, nil
}

func newTestExecutor(snapshot telemetry.Snapshot, explanation explain.IssueExplanation) *Executor {
	return NewExecutor(Dependencies{
		Snapshots: SnapshotFunc(func(context.Context) (telemetry.Snapshot, error) { return snapshot, nil }),
		Explainer: staticExplainer{result: explanation},
	})
}
