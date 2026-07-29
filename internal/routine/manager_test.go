package routine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/runner"
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
	records   []RunRecord
	recordErr error
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

type fakeIssueStore struct {
	issues    []connector.Issue
	drafts    []intake.IssueDraft
	states    []string
	fetchErr  error
	createErr error
	stateErr  error
}

type routineMetricsRecorder struct {
	events []workflowmetrics.PhaseEvent
}

func (r *routineMetricsRecorder) RecordWorkflowPhaseEvent(_ context.Context, event workflowmetrics.PhaseEvent) (int64, error) {
	r.events = append(r.events, event)
	return int64(len(r.events)), nil
}

func (s *fakeIssueStore) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return append([]connector.Issue(nil), s.issues...), s.fetchErr
}

func (s *fakeIssueStore) CreateIntakeIssue(_ context.Context, draft intake.IssueDraft) (intake.Issue, error) {
	if s.createErr != nil {
		return intake.Issue{}, s.createErr
	}
	s.drafts = append(s.drafts, draft)
	id := "issue-" + time.Now().Format("150405.000000000")
	s.issues = append(s.issues, connector.Issue{ID: id, Identifier: "DET-1", Description: draft.Body, Labels: append([]string(nil), draft.Labels...)})
	return intake.Issue{ID: id, Identifier: "DET-1", URL: "https://example.test/issues/1", Body: draft.Body}, nil
}

func (s *fakeIssueStore) SetIntakeIssueState(_ context.Context, _ string, state string) error {
	s.states = append(s.states, state)
	return s.stateErr
}
