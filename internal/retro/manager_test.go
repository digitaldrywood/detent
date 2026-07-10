package retro

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/intake"
)

func TestManagerRoutesAndDeduplicatesRecurringFindings(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	telemetry := &recordingTelemetryStore{snapshot: routingSnapshot(now)}
	projectIssues := memory.New(memory.Config{Stateful: true})
	productIssues := memory.New(memory.Config{Stateful: true})
	manager, err := New(Settings{
		ProjectID: "example",
		Config: Config{
			Enabled: true,
		},
		ProjectIssues: projectIssues,
		ProductIssues: productIssues,
	}, telemetry, nil, func() time.Time { return now })
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	first, err := manager.RunOnce(context.Background(), TriggerDaily)
	if err != nil {
		t.Fatalf("RunOnce(first) error = %v", err)
	}
	if first.Filed != 2 || first.Updated != 0 {
		t.Fatalf("RunOnce(first) = %#v, want two filed", first)
	}
	assertSingleRetroIssue(t, projectIssues, PatternInvalidWorkpadStatus, "Proposed WORKFLOW.md change")
	assertSingleRetroIssue(t, productIssues, PatternCompletedRedispatch, "classification: product")

	telemetry.snapshot.PhaseEvents = append(telemetry.snapshot.PhaseEvents, PhaseEvent{Identifier: "issue-status-c", Reason: "workpad_status_invalid", StartedAt: now.Add(time.Minute)})
	second, err := manager.RunOnce(context.Background(), TriggerCompletion)
	if err != nil {
		t.Fatalf("RunOnce(second) error = %v", err)
	}
	if second.Filed != 0 || second.Updated != 1 {
		t.Fatalf("RunOnce(second) = %#v, want only the recurring finding updated", second)
	}
	assertSingleRetroIssue(t, projectIssues, PatternInvalidWorkpadStatus, "occurrences: 3")
	assertSingleRetroIssue(t, productIssues, PatternCompletedRedispatch, "classification: product")
}

func TestManagerPreservesReviewedOutcomeOnRecurrence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	telemetry := &recordingTelemetryStore{snapshot: routingSnapshot(now)}
	projectIssues := memory.New(memory.Config{Stateful: true})
	productIssues := memory.New(memory.Config{Stateful: true})
	manager, err := New(Settings{
		ProjectID: "example",
		Config: Config{
			Enabled: true,
		},
		ProjectIssues: projectIssues,
		ProductIssues: productIssues,
	}, telemetry, nil, func() time.Time { return now })
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := manager.RunOnce(context.Background(), TriggerDaily); err != nil {
		t.Fatalf("RunOnce(first) error = %v", err)
	}
	issues, err := projectIssues.FetchIssuesByStates(context.Background(), []string{"Backlog"})
	if err != nil || len(issues) != 1 {
		t.Fatalf("FetchIssuesByStates() issues=%#v error=%v, want one", issues, err)
	}
	reviewed := strings.Replace(issues[0].Description, "- status: pending", "- status: accepted", 1)
	if _, err := projectIssues.UpdateIntakeIssue(context.Background(), issues[0].ID, intake.IssueDraft{Title: issues[0].Title, Body: reviewed, Labels: issues[0].Labels}); err != nil {
		t.Fatalf("UpdateIntakeIssue() error = %v", err)
	}
	manager, err = New(Settings{
		ProjectID: "example",
		Config: Config{
			Enabled: true,
		},
		ProjectIssues: projectIssues,
		ProductIssues: productIssues,
	}, telemetry, nil, func() time.Time { return now })
	if err != nil {
		t.Fatalf("New(restarted) error = %v", err)
	}
	telemetry.snapshot.PhaseEvents = append(telemetry.snapshot.PhaseEvents, PhaseEvent{Identifier: "issue-status-c", Reason: "workpad_status_invalid", StartedAt: now.Add(time.Minute)})

	result, err := manager.RunOnce(context.Background(), TriggerCompletion)
	if err != nil {
		t.Fatalf("RunOnce(second) error = %v", err)
	}
	if result.Updated != 1 {
		t.Fatalf("RunOnce(second).Updated = %d, want only recurring evidence updated", result.Updated)
	}
	assertSingleRetroIssue(t, projectIssues, PatternInvalidWorkpadStatus, "- status: accepted")
}

func TestManagerEnforcesDailyCreationCap(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	telemetry := &recordingTelemetryStore{snapshot: routingSnapshot(now)}
	manager, err := New(Settings{
		ProjectID:     "example",
		Config:        Config{Enabled: true, DailyIssueCap: 1},
		ProjectIssues: memory.New(memory.Config{Stateful: true}),
		ProductIssues: memory.New(memory.Config{Stateful: true}),
	}, telemetry, nil, func() time.Time { return now })
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := manager.RunOnce(context.Background(), TriggerDaily)
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Filed != 1 || result.Capped != 1 {
		t.Fatalf("RunOnce() = %#v, want one filed and one capped", result)
	}
}

func TestManagerDisabledIsNoOp(t *testing.T) {
	t.Parallel()

	telemetry := &recordingTelemetryStore{}
	manager, err := New(Settings{ProjectID: "example", Config: Config{}}, telemetry, nil, time.Now)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	result, err := manager.RunOnce(context.Background(), TriggerDaily)
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(result.Findings) != 0 || telemetry.loads != 0 || len(telemetry.records) != 0 {
		t.Fatalf("disabled run = %#v loads=%d records=%#v, want no-op", result, telemetry.loads, telemetry.records)
	}
}

func routingSnapshot(now time.Time) Snapshot {
	return Snapshot{
		Attempts: []Attempt{
			{ID: 1, Identifier: "issue-redispatch", StartedAt: now.Add(-4 * time.Minute), CompletedAt: now.Add(-3 * time.Minute), TerminalState: "success", Phase: "waiting"},
			{ID: 2, Identifier: "issue-redispatch", StartedAt: now.Add(-2 * time.Minute), CompletedAt: now.Add(-time.Minute), TerminalState: "success"},
		},
		PhaseEvents: []PhaseEvent{
			{Identifier: "issue-status-a", Reason: "workpad_status_invalid", StartedAt: now.Add(-2 * time.Minute)},
			{Identifier: "issue-status-b", Reason: "workpad_status_invalid", StartedAt: now.Add(-time.Minute)},
		},
	}
}

func assertSingleRetroIssue(t *testing.T, issueStore *memory.Connector, pattern string, bodyContains string) {
	t.Helper()
	issues, err := issueStore.FetchIssuesByStates(context.Background(), []string{"Backlog"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues = %#v, want one", issues)
	}
	if !strings.Contains(issues[0].Description, "pattern: "+pattern) || !strings.Contains(issues[0].Description, bodyContains) {
		t.Fatalf("issue body missing pattern/body text:\n%s", issues[0].Description)
	}
}

type recordingTelemetryStore struct {
	snapshot Snapshot
	loads    int
	records  []RunRecord
}

func (s *recordingTelemetryStore) LoadRetroSnapshot(context.Context, string, time.Time) (Snapshot, error) {
	s.loads++
	return s.snapshot, nil
}

func (s *recordingTelemetryStore) RetroFiledOnDay(_ context.Context, _ string, day time.Time) (int, error) {
	count := 0
	for _, record := range s.records {
		if record.CompletedAt.UTC().Format(time.DateOnly) == day.UTC().Format(time.DateOnly) {
			count += record.Filed
		}
	}
	return count, nil
}

func (s *recordingTelemetryStore) RecordRetroRun(_ context.Context, record RunRecord) error {
	s.records = append(s.records, record)
	return nil
}
