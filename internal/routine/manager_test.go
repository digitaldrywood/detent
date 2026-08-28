package routine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/schedulehealth"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestDue(t *testing.T) {
	t.Parallel()
	last := time.Date(2026, time.July, 17, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		schedule string
		last     time.Time
		now      time.Time
		want     bool
		wantErr  bool
	}{
		{name: "before next occurrence", schedule: "0 * * * *", last: last, now: last.Add(59 * time.Minute)},
		{name: "at next occurrence", schedule: "0 * * * *", last: last, now: last.Add(time.Hour), want: true},
		{name: "after next occurrence", schedule: "0 * * * *", last: last, now: last.Add(2 * time.Hour), want: true},
		{name: "never run", schedule: "0 * * * *", now: last.Add(time.Hour)},
		{name: "invalid schedule", schedule: "hourly", last: last, now: last.Add(time.Hour), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Due(tt.schedule, tt.last, tt.now)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Due() error = %v, wantErr %t", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("Due() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestManagerNextScheduledUsesPersistedRun(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, time.July, 17, 9, 15, 0, 0, time.UTC)
	store := &fakeStore{records: []RunRecord{{
		ProjectID: "detent", RoutineName: "frequent", ScheduledFor: base.Add(5 * time.Minute), StartedAt: base.Add(6 * time.Minute), CompletedAt: base.Add(7 * time.Minute),
	}}}
	manager, err := New(Settings{
		ProjectID: "detent",
		Definitions: []config.Routine{
			{Name: "daily", Schedule: "0 12 * * *", Prompt: "Inspect daily."},
			{Name: "frequent", Schedule: "*/20 * * * *", Prompt: "Inspect frequently."},
		},
		Runner: fakeRunner{}, Issues: &fakeIssueStore{},
	}, store, nil, func() time.Time { return base })
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	next, name, scheduled, err := manager.nextScheduled(context.Background())
	if err != nil {
		t.Fatalf("nextScheduled() error = %v", err)
	}
	if !scheduled || name != "frequent" || !next.Equal(time.Date(2026, time.July, 17, 9, 40, 0, 0, time.UTC)) {
		t.Fatalf("nextScheduled() = %s, %q, %t", next, name, scheduled)
	}
}

func TestManagerNextScheduledDoesNotReplayMissedRunAfterRestart(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 17, 9, 15, 0, 0, time.UTC)
	store := &fakeStore{records: []RunRecord{{
		ProjectID: "detent", RoutineName: "hourly", ScheduledFor: time.Date(2026, time.July, 17, 7, 0, 0, 0, time.UTC),
		StartedAt: time.Date(2026, time.July, 17, 7, 1, 0, 0, time.UTC), CompletedAt: time.Date(2026, time.July, 17, 7, 2, 0, 0, time.UTC),
	}}}
	manager, err := New(Settings{
		ProjectID: "detent", Definitions: []config.Routine{{Name: "hourly", Schedule: "0 * * * *", Prompt: "Inspect."}},
		Runner: fakeRunner{}, Issues: &fakeIssueStore{},
	}, store, nil, func() time.Time { return now })
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	next, name, scheduled, err := manager.nextScheduled(context.Background())
	if err != nil {
		t.Fatalf("nextScheduled() error = %v", err)
	}
	want := time.Date(2026, time.July, 17, 10, 0, 0, 0, time.UTC)
	if !scheduled || name != "hourly" || !next.Equal(want) {
		t.Fatalf("nextScheduled() = %s, %q, %t; want %s, hourly, true", next, name, scheduled, want)
	}
}

func TestManagerNextScheduledUsesRuntimeTimezoneAfterRestart(t *testing.T) {
	t.Parallel()
	location := time.FixedZone("operator", -5*60*60)
	now := time.Date(2026, time.July, 17, 9, 15, 0, 0, location)
	store := &fakeStore{records: []RunRecord{{
		ProjectID: "detent", RoutineName: "hourly", ScheduledFor: time.Date(2026, time.July, 17, 14, 0, 0, 0, time.UTC),
		StartedAt: time.Date(2026, time.July, 17, 14, 1, 0, 0, time.UTC), CompletedAt: time.Date(2026, time.July, 17, 14, 2, 0, 0, time.UTC),
	}}}
	manager, err := New(Settings{
		ProjectID: "detent", Definitions: []config.Routine{{Name: "hourly", Schedule: "0 * * * *", Prompt: "Inspect."}},
		Runner: fakeRunner{}, Issues: &fakeIssueStore{},
	}, store, nil, func() time.Time { return now })
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	next, name, scheduled, err := manager.nextScheduled(context.Background())
	if err != nil {
		t.Fatalf("nextScheduled() error = %v", err)
	}
	want := time.Date(2026, time.July, 17, 10, 0, 0, 0, location)
	if !scheduled || name != "hourly" || !next.Equal(want) || next.Location() != location {
		t.Fatalf("nextScheduled() = %s (%s), %q, %t; want %s (%s)", next, next.Location(), name, scheduled, want, location)
	}
}

func TestManagerScheduledRunRecordsLiveness(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)
	recorder := &routineScheduleRecorder{}
	manager, err := New(Settings{
		ProjectID: "detent",
		Definitions: []config.Routine{{
			Name: "audit", Schedule: "* * * * *", Prompt: "Inspect.",
		}},
		Runner: fakeRunner{}, Issues: &fakeIssueStore{}, ScheduleRuns: recorder,
	}, &fakeStore{}, nil, func() time.Time { return now })
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scheduledFor := now.Add(time.Minute)
	if _, err := manager.runNamed(t.Context(), "audit", scheduledFor, true); err != nil {
		t.Fatalf("runNamed() error = %v", err)
	}
	if len(recorder.runs) != 1 || recorder.runs[0].ScheduleID != schedulehealth.RoutineID("audit") || !recorder.runs[0].ScheduledFor.Equal(scheduledFor) {
		t.Fatalf("scheduled runs = %#v, want audit run at %s", recorder.runs, scheduledFor)
	}
}

func TestManagerScheduledRunSkipsStaleTimerAfterScheduleUpdate(t *testing.T) {
	t.Parallel()
	current := time.Date(2026, time.July, 17, 9, 15, 0, 0, time.UTC)
	runs := 0
	backend := fakeRunner{onRun: func(runner.RunRequest) { runs++ }}
	store := &fakeStore{}
	issues := &fakeIssueStore{}
	manager, err := New(Settings{
		ProjectID: "detent",
		Definitions: []config.Routine{{
			Name: "maintenance", Schedule: "0 * * * *", Prompt: "Inspect.",
		}},
		Runner: backend, Issues: issues,
	}, store, nil, func() time.Time { return current })
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stale, _, _, err := manager.nextScheduled(context.Background())
	if err != nil {
		t.Fatalf("nextScheduled() error = %v", err)
	}
	current = current.Add(5 * time.Minute)
	if err := manager.Update(Settings{
		ProjectID: "detent",
		Definitions: []config.Routine{{
			Name: "maintenance", Schedule: "30 * * * *", Prompt: "Inspect.",
		}},
		Runner: backend, Issues: issues,
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := manager.runNamed(context.Background(), "maintenance", stale, true); err != nil {
		t.Fatalf("runNamed() error = %v", err)
	}
	if runs != 0 || len(store.records) != 0 {
		t.Fatalf("runs = %d, records = %d; want stale occurrence skipped", runs, len(store.records))
	}
	next, _, _, err := manager.nextScheduled(context.Background())
	if err != nil {
		t.Fatalf("nextScheduled() after update error = %v", err)
	}
	want := time.Date(2026, time.July, 17, 9, 30, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("nextScheduled() after update = %s, want %s", next, want)
	}
}

func TestManagerRunOnceFilesAndDeduplicatesOpenIssue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 17, 10, 0, 0, 0, time.UTC)
	store := &fakeStore{}
	issues := &fakeIssueStore{}
	backend := fakeRunner{proposals: []Proposal{{DedupKey: "lint/deadcode", Title: "Remove dead code", Body: "The unused path is reproducible."}}}
	manager := newTestManager(t, backend, issues, store, now)

	first, err := manager.RunOnce(context.Background(), "maintenance")
	if err != nil {
		t.Fatalf("first RunOnce() error = %v", err)
	}
	if len(first.Filed) != 1 || first.Deduplicated != 0 {
		t.Fatalf("first result = %#v", first)
	}
	if len(issues.drafts) != 1 || len(issues.drafts[0].Labels) != 1 || issues.drafts[0].Labels[0] != IssueLabel {
		t.Fatalf("filed draft = %#v", issues.drafts)
	}
	if !strings.Contains(issues.drafts[0].Body, "<!-- detent:routine fingerprint=") {
		t.Fatalf("draft body missing marker: %q", issues.drafts[0].Body)
	}
	if len(issues.states) != 1 || issues.states[0] != IssueState {
		t.Fatalf("states = %#v, want %q", issues.states, IssueState)
	}

	second, err := manager.RunOnce(context.Background(), "maintenance")
	if err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if len(second.Filed) != 0 || second.Deduplicated != 1 || len(issues.drafts) != 1 {
		t.Fatalf("second result = %#v, drafts = %d", second, len(issues.drafts))
	}
	if len(store.records) != 2 || store.records[0].Filed != 1 || store.records[1].Deduplicated != 1 {
		t.Fatalf("run records = %#v", store.records)
	}
}

func TestManagerRoutineProposalLabelAllowlist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		allowed  []string
		proposed []string
		want     []string
	}{
		{name: "configured label is applied", allowed: []string{"bug"}, proposed: []string{"Bug"}, want: []string{IssueLabel, "bug"}},
		{name: "label outside allowlist is dropped", allowed: []string{"bug"}, proposed: []string{"security"}, want: []string{IssueLabel}},
		{name: "allowed subset is applied", allowed: []string{"bug"}, proposed: []string{"bug", "security"}, want: []string{IssueLabel, "bug"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issues := &fakeIssueStore{}
			manager, err := New(Settings{
				ProjectID: "detent",
				Definitions: []config.Routine{{
					Name: "maintenance", Schedule: "0 * * * *", Prompt: "Inspect.", Labels: tt.allowed,
				}},
				SearchStates: []string{"Backlog", "Todo", "Done"},
				Runner: fakeRunner{proposals: []Proposal{{
					DedupKey: "finding", Title: "Finding", Body: "Evidence.", Labels: tt.proposed,
				}}},
				Issues: issues,
			}, &fakeStore{}, nil, nil)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if _, err := manager.RunOnce(t.Context(), "maintenance"); err != nil {
				t.Fatalf("RunOnce() error = %v", err)
			}
			if len(issues.drafts) != 1 || !reflect.DeepEqual(issues.drafts[0].Labels, tt.want) {
				t.Fatalf("draft labels = %#v, want %#v", issues.drafts, tt.want)
			}
		})
	}
}

func TestManagerRoutineOptoutLabelPreventsAutoPromote(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	workflow := config.Default()
	workflow.Agent.AutoPromote.Enabled = true
	optoutLabel := workflow.Agent.AutoPromote.OptoutLabel
	tracker := memory.New(memory.Config{Stateful: true, Now: func() time.Time { return now }})
	manager, err := New(Settings{
		ProjectID: "detent",
		Definitions: []config.Routine{{
			Name: "feature-sweep", Schedule: "0 * * * *", Prompt: "Inspect product intent.",
			TargetState: "Backlog", Labels: []string{optoutLabel},
		}},
		SearchStates: workflow.KanbanStateNames(),
		Runner: fakeRunner{proposals: []Proposal{{
			DedupKey: "feature", Title: "Propose feature", Body: "Product evidence.", Labels: []string{optoutLabel},
		}}},
		Issues: tracker,
	}, &fakeStore{}, nil, func() time.Time { return now })
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	result, err := manager.RunOnce(t.Context(), "feature-sweep")
	if err != nil || len(result.Filed) != 1 {
		t.Fatalf("RunOnce() = %#v, %v", result, err)
	}
	issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{result.Filed[0].ID})
	if err != nil || len(issues) != 1 {
		t.Fatalf("FetchIssueStatesByIDs() = %#v, %v", issues, err)
	}
	if issues[0].State != "Backlog" || !containsFold(issues[0].Labels, optoutLabel) {
		t.Fatalf("routine issue = %#v", issues[0])
	}
	decision := orchestrator.EvaluateAutoPromote(
		issues[0],
		orchestrator.AutoPromoteSummary{},
		orchestrator.ConfigFromWorkflow(workflow).AutoPromote,
		now,
	)
	if decision.Action != orchestrator.AutoPromoteActionAwaitReview || decision.Reason != orchestrator.AutoPromoteReasonOptoutLabel {
		t.Fatalf("EvaluateAutoPromote() = %#v", decision)
	}
}

func TestManagerRunOnceRecordsRoutineOrigin(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 17, 10, 0, 0, 0, time.UTC)
	metrics := &routineMetricsRecorder{}
	manager, err := New(Settings{
		ProjectID:    "detent",
		Definitions:  []config.Routine{{Name: "maintenance", Schedule: "0 * * * *", Prompt: "Inspect."}},
		SearchStates: []string{"Backlog", "Todo"},
		Runner:       fakeRunner{proposals: []Proposal{{DedupKey: "lint/deadcode", Title: "Remove dead code", Body: "Reproducible."}}},
		Issues:       &fakeIssueStore{},
		Metrics:      metrics,
	}, &fakeStore{}, nil, func() time.Time { return now })
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := manager.RunOnce(context.Background(), "maintenance"); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(metrics.events) != 1 {
		t.Fatalf("events = %#v, want one lane entry", metrics.events)
	}
	metadata, ok := provenance.Parse(metrics.events[0].MetadataJSON)
	if metrics.events[0].PhaseName != IssueState || metrics.events[0].Status != "entered" ||
		!ok || metadata.Provenance.Origin != provenance.OriginRoutine {
		t.Fatalf("event = %#v, metadata = %#v", metrics.events[0], metadata)
	}
}

func TestManagerRunOnceRefilesClosedIssue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 17, 10, 0, 0, 0, time.UTC)
	store := &fakeStore{}
	issues := &fakeIssueStore{}
	backend := fakeRunner{output: `{"issues":[{"dedup_key":"coverage/parser","title":"Cover parser errors","body":"The parser error branch is uncovered."}]}`}
	manager := newTestManager(t, backend, issues, store, now)
	if _, err := manager.RunOnce(context.Background(), "maintenance"); err != nil {
		t.Fatalf("first RunOnce() error = %v", err)
	}
	issues.issues[0].Closed = true
	result, err := manager.RunOnce(context.Background(), "maintenance")
	if err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if len(result.Filed) != 1 || result.Deduplicated != 0 || len(issues.drafts) != 2 {
		t.Fatalf("result = %#v, drafts = %d", result, len(issues.drafts))
	}
}

func TestManagerRunOnceDeduplicatesRepeatedProposalInSameRun(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 17, 10, 0, 0, 0, time.UTC)
	proposal := Proposal{DedupKey: "lint/deadcode", Title: "Remove dead code", Body: "The unused path is reproducible."}
	issues := &fakeIssueStore{}
	manager := newTestManager(t, fakeRunner{proposals: []Proposal{proposal, proposal}}, issues, &fakeStore{}, now)
	result, err := manager.RunOnce(context.Background(), "maintenance")
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(result.Filed) != 1 || result.Deduplicated != 1 || len(issues.drafts) != 1 {
		t.Fatalf("result = %#v, drafts = %d", result, len(issues.drafts))
	}
}

func TestManagerRunOnceEnforcesFindingLimits(t *testing.T) {
	t.Parallel()
	proposals := []Proposal{
		{DedupKey: "finding/one", Title: "Finding one", Body: "Evidence one."},
		{DedupKey: "finding/two", Title: "Finding two", Body: "Evidence two."},
		{DedupKey: "finding/three", Title: "Finding three", Body: "Evidence three."},
	}
	tests := []struct {
		name         string
		maxPerRun    int
		maxOpen      int
		proposals    int
		existingOpen int
		wantFiled    int
		wantLimited  int
	}{
		{name: "under per-run cap", maxPerRun: 3, maxOpen: 10, proposals: 2, wantFiled: 2},
		{name: "exactly at per-run cap", maxPerRun: 2, maxOpen: 10, proposals: 2, wantFiled: 2},
		{name: "over per-run cap", maxPerRun: 2, maxOpen: 10, proposals: 3, wantFiled: 2, wantLimited: 1},
		{name: "over open-finding cap with existing issues", maxPerRun: 3, maxOpen: 2, proposals: 1, existingOpen: 2, wantLimited: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issues := &fakeIssueStore{}
			for index := range tt.existingOpen {
				issues.issues = append(issues.issues, connector.Issue{
					ID: fmt.Sprintf("existing-%d", index),
				})
			}
			store := &fakeStore{}
			if tt.existingOpen > 0 {
				record := RunRecord{ProjectID: "detent", RoutineName: "maintenance"}
				for index := range tt.existingOpen {
					record.Issues = append(record.Issues, IssueRecord{ID: fmt.Sprintf("existing-%d", index)})
				}
				store.records = append(store.records, record)
			}
			manager, err := New(Settings{
				ProjectID: "detent",
				Definitions: []config.Routine{{
					Name: "maintenance", Schedule: "0 * * * *", Prompt: "Inspect.",
					MaxFindingsPerRun: tt.maxPerRun, MaxOpenFindings: tt.maxOpen,
				}},
				SearchStates: []string{"Backlog", "Todo", "Done"},
				Runner:       fakeRunner{proposals: proposals[:tt.proposals]},
				Issues:       issues,
			}, store, nil, func() time.Time { return time.Date(2026, time.July, 17, 10, 0, 0, 0, time.UTC) })
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			result, err := manager.RunOnce(context.Background(), "maintenance")
			if err != nil {
				t.Fatalf("RunOnce() error = %v", err)
			}
			if result.Proposed != tt.proposals || len(result.Filed) != tt.wantFiled || result.Limited != tt.wantLimited {
				t.Fatalf("Result = %#v, want proposed=%d filed=%d limited=%d", result, tt.proposals, tt.wantFiled, tt.wantLimited)
			}
			gotRecord := store.records[len(store.records)-1]
			if gotRecord.Proposed != tt.proposals || gotRecord.Filed != tt.wantFiled || gotRecord.Limited != tt.wantLimited {
				t.Fatalf("Run records = %#v", store.records)
			}
		})
	}
}

func TestManagerRunOnceCountsPartialCreationsAgainstLimits(t *testing.T) {
	t.Parallel()
	proposals := []Proposal{
		{DedupKey: "finding/one", Title: "Finding one", Body: "Evidence one."},
		{DedupKey: "finding/two", Title: "Finding two", Body: "Evidence two."},
		{DedupKey: "finding/three", Title: "Finding three", Body: "Evidence three."},
	}
	issues := &fakeIssueStore{createErr: errors.New("project add failed"), partialCreate: true, hideFromBoard: true}
	store := &fakeStore{}
	manager, err := New(Settings{
		ProjectID: "detent",
		Definitions: []config.Routine{{
			Name: "maintenance", Schedule: "0 * * * *", Prompt: "Inspect.", MaxFindingsPerRun: 2, MaxOpenFindings: 2,
		}},
		SearchStates: []string{"Todo"},
		Runner:       fakeRunner{proposals: proposals},
		Issues:       issues,
	}, store, nil, func() time.Time { return time.Date(2026, time.July, 17, 10, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	result, err := manager.RunOnce(context.Background(), "maintenance")
	if err == nil || !strings.Contains(err.Error(), "project add failed") {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if issues.createCalls != 2 || len(result.Filed) != 2 || result.Limited != 1 || len(store.trackedIssues) != 2 {
		t.Fatalf("first result = %#v, create calls = %d, tracked = %#v", result, issues.createCalls, store.trackedIssues)
	}

	result, err = manager.RunOnce(context.Background(), "maintenance")
	if err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if issues.createCalls != 2 || len(result.Filed) != 0 || result.Limited != 3 {
		t.Fatalf("second result = %#v, create calls = %d", result, issues.createCalls)
	}
}

func TestManagerRunOnceRecordsFailures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 17, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		runner  fakeRunner
		issues  *fakeIssueStore
		wantErr string
	}{
		{name: "agent failure", runner: fakeRunner{err: errors.New("agent unavailable")}, issues: &fakeIssueStore{}, wantErr: "agent unavailable"},
		{name: "malformed output", runner: fakeRunner{output: "not json"}, issues: &fakeIssueStore{}, wantErr: "output is invalid"},
		{name: "dedup lookup failure", runner: fakeRunner{proposals: []Proposal{{DedupKey: "deps/a", Title: "Update A", Body: "A is stale."}}}, issues: &fakeIssueStore{fetchErr: errors.New("lookup failed")}, wantErr: "lookup failed"},
		{name: "issue creation failure", runner: fakeRunner{proposals: []Proposal{{DedupKey: "deps/a", Title: "Update A", Body: "A is stale."}}}, issues: &fakeIssueStore{createErr: errors.New("create failed")}, wantErr: "create failed"},
		{name: "state transition failure", runner: fakeRunner{proposals: []Proposal{{DedupKey: "deps/a", Title: "Update A", Body: "A is stale."}}}, issues: &fakeIssueStore{stateErr: errors.New("state failed")}, wantErr: "state failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{}
			manager := newTestManager(t, tt.runner, tt.issues, store, now)
			_, err := manager.RunOnce(context.Background(), "maintenance")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("RunOnce() error = %v, want %q", err, tt.wantErr)
			}
			if len(store.records) != 1 || !strings.Contains(store.records[0].Error, tt.wantErr) {
				t.Fatalf("records = %#v, want persisted error %q", store.records, tt.wantErr)
			}
		})
	}
}

func TestManagerRunOnceReturnsRunRecordFailure(t *testing.T) {
	t.Parallel()
	store := &fakeStore{recordErr: errors.New("database unavailable")}
	manager := newTestManager(t, fakeRunner{}, &fakeIssueStore{}, store, time.Date(2026, time.July, 17, 10, 0, 0, 0, time.UTC))
	_, err := manager.RunOnce(context.Background(), "maintenance")
	if err == nil || !strings.Contains(err.Error(), "record routine run: database unavailable") {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(store.records) != 1 {
		t.Fatalf("record attempts = %d, want 1", len(store.records))
	}
}

func newTestManager(t *testing.T, backend runner.Backend, issues IssueStore, store Store, now time.Time) *Manager {
	t.Helper()
	manager, err := New(Settings{
		ProjectID:    "detent",
		Definitions:  []config.Routine{{Name: "maintenance", Schedule: "0 * * * *", Prompt: "Inspect configured criteria."}},
		SearchStates: []string{"Backlog", "Todo", "Done"},
		Runner:       backend,
		Issues:       issues,
	}, store, nil, func() time.Time { return now })
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return manager
}

type fakeRunner struct {
	proposals []Proposal
	output    string
	err       error
	onRun     func(runner.RunRequest)
}

func (f fakeRunner) Run(ctx context.Context, request runner.RunRequest) (runner.RunResult, error) {
	if f.onRun != nil {
		f.onRun(request)
	}
	if f.err != nil {
		return runner.RunResult{}, f.err
	}
	for _, proposal := range f.proposals {
		arguments, err := json.Marshal(proposal)
		if err != nil {
			return runner.RunResult{}, err
		}
		if _, err := request.AgentToolHandler(ctx, runner.AgentToolCall{Name: ProposalToolName, Arguments: arguments}); err != nil {
			return runner.RunResult{}, err
		}
	}
	return runner.RunResult{FinalState: runner.FinalStateCompleted, Output: f.output}, nil
}

type fakeStore struct {
	records       []RunRecord
	trackedIssues []IssueRecord
	closedIssues  map[string]bool
	recordErr     error
}

type routineScheduleRecorder struct {
	runs []schedulehealth.Run
}

func (r *routineScheduleRecorder) RecordScheduledRun(_ context.Context, run schedulehealth.Run) error {
	r.runs = append(r.runs, run)
	return nil
}

func (s *fakeStore) LatestRoutineRun(_ context.Context, projectID string, routineName string) (RunRecord, bool, error) {
	for index := len(s.records) - 1; index >= 0; index-- {
		if s.records[index].ProjectID == projectID && s.records[index].RoutineName == routineName {
			return s.records[index], true, nil
		}
	}
	return RunRecord{}, false, nil
}

func (s *fakeStore) RecordRoutineRun(_ context.Context, record RunRecord) error {
	s.records = append(s.records, record)
	return s.recordErr
}

func (s *fakeStore) OpenRoutineIssueIDs(_ context.Context, projectID string, routineName string) ([]string, error) {
	seen := map[string]struct{}{}
	var issueIDs []string
	for _, record := range s.records {
		if record.ProjectID != projectID || record.RoutineName != routineName {
			continue
		}
		for _, issue := range record.Issues {
			if !s.closedIssues[issue.ID] {
				seen[issue.ID] = struct{}{}
			}
		}
	}
	for _, issue := range s.trackedIssues {
		if !s.closedIssues[issue.ID] {
			seen[issue.ID] = struct{}{}
		}
	}
	for issueID := range seen {
		issueIDs = append(issueIDs, issueID)
	}
	return issueIDs, nil
}

func (s *fakeStore) RecordRoutineIssue(_ context.Context, _ string, _ string, issue IssueRecord) error {
	s.trackedIssues = append(s.trackedIssues, issue)
	return nil
}

func (s *fakeStore) CloseRoutineIssues(_ context.Context, _ string, _ string, issueIDs []string) error {
	if s.closedIssues == nil {
		s.closedIssues = map[string]bool{}
	}
	for _, issueID := range issueIDs {
		s.closedIssues[issueID] = true
	}
	return nil
}

type fakeIssueStore struct {
	issues        []connector.Issue
	drafts        []intake.IssueDraft
	states        []string
	fetchErr      error
	createErr     error
	stateErr      error
	findErr       error
	partialCreate bool
	hideFromBoard bool
	createCalls   int
}

type routineMetricsRecorder struct {
	events []workflowmetrics.PhaseEvent
}

func (r *routineMetricsRecorder) RecordWorkflowPhaseEvent(_ context.Context, event workflowmetrics.PhaseEvent) (int64, error) {
	r.events = append(r.events, event)
	return int64(len(r.events)), nil
}

func (s *fakeIssueStore) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	if s.hideFromBoard {
		return nil, s.fetchErr
	}
	return append([]connector.Issue(nil), s.issues...), s.fetchErr
}

func (s *fakeIssueStore) FetchIssueStatesByIDs(_ context.Context, issueIDs []string) ([]connector.Issue, error) {
	wanted := map[string]struct{}{}
	for _, issueID := range issueIDs {
		wanted[issueID] = struct{}{}
	}
	var issues []connector.Issue
	for _, issue := range s.issues {
		if _, ok := wanted[issue.ID]; ok {
			issues = append(issues, issue)
		}
	}
	return issues, s.fetchErr
}

func (s *fakeIssueStore) FindIntakeIssue(_ context.Context, marker string) (intake.Issue, bool, error) {
	if s.findErr != nil {
		return intake.Issue{}, false, s.findErr
	}
	for _, issue := range s.issues {
		if strings.Contains(issue.Description, marker) {
			return intake.Issue{ID: issue.ID, Identifier: issue.Identifier, URL: issue.URL, Body: issue.Description, Closed: issue.Closed}, true, nil
		}
	}
	return intake.Issue{}, false, nil
}

func (s *fakeIssueStore) CreateIntakeIssue(_ context.Context, draft intake.IssueDraft) (intake.Issue, error) {
	s.createCalls++
	if s.createErr != nil && !s.partialCreate {
		return intake.Issue{}, s.createErr
	}
	s.drafts = append(s.drafts, draft)
	id := fmt.Sprintf("issue-%d", s.createCalls)
	s.issues = append(s.issues, connector.Issue{ID: id, Identifier: "DET-1", Description: draft.Body, Labels: append([]string(nil), draft.Labels...)})
	issue := intake.Issue{ID: id, Identifier: "DET-1", URL: "https://example.test/issues/1", Body: draft.Body}
	return issue, s.createErr
}

func (s *fakeIssueStore) SetIntakeIssueState(_ context.Context, _ string, state string) error {
	s.states = append(s.states, state)
	return s.stateErr
}
