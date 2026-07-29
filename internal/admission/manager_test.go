package admission

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/local"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestManagerDisabledRegistersNoSchedule(t *testing.T) {
	t.Parallel()

	manager, err := New(Settings{Config: config.BacklogAdmission{Enabled: false}}, nil, nil, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if manager.Enabled() {
		t.Fatal("Enabled() = true")
	}
	next, scheduled, err := manager.nextScheduled(context.Background())
	if err != nil || scheduled || !next.IsZero() {
		t.Fatalf("nextScheduled() = %v, %t, %v", next, scheduled, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestManagerEnforcesOrderingAndAllCapsBeforeWritingComments(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	priorityOne, priorityTwo, priorityThree := 1, 2, 3
	issues := []connector.Issue{
		admissionIssueFixture("issue-3", "DD-3", priorityThree, now.Add(-3*time.Hour)),
		admissionIssueFixture("issue-1", "DD-1", priorityOne, now.Add(-time.Hour)),
		admissionIssueFixture("issue-2", "DD-2", priorityTwo, now.Add(-2*time.Hour)),
	}
	tracker := memory.New(memory.Config{Issues: issues, Stateful: true, Now: func() time.Time { return now }})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.MaxCandidatesPerRun = 2
	settings.Config.MaxProposalsPerRun = 1
	settings.Config.MaxOpenProposals = 1
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if got, want := agent.candidateIDs[0], []string{"issue-1", "issue-2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate order = %#v, want %#v", got, want)
	}
	if result.CandidatesFound != 3 || result.Candidates != 2 || len(result.Proposals) != 1 ||
		result.Truncated["candidates"] != 1 || result.Truncated["proposals"] != 1 {
		t.Fatalf("result = %#v", result)
	}
	open, err := backend.OpenAdmissionProposals(context.Background(), "detent", 0)
	if err != nil {
		t.Fatalf("OpenAdmissionProposals() error = %v", err)
	}
	if len(open) != 1 || open[0].IssueID != "issue-1" || open[0].CommentedAt.IsZero() {
		t.Fatalf("open proposals = %#v", open)
	}
	events := tracker.Events()
	commentCount := 0
	for _, event := range events {
		if event.Kind == memory.EventKindComment {
			commentCount++
		}
		if event.Kind == memory.EventKindStateUpdate {
			t.Fatalf("admission changed issue status: %#v", event)
		}
	}
	if commentCount != 1 {
		t.Fatalf("comment count = %d, want 1", commentCount)
	}

	second, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if agent.calls != 1 || second.Skipped["open_proposal_cap"] == 0 {
		t.Fatalf("second result = %#v, runner calls = %d, want pre-agent open cap", second, agent.calls)
	}
}

func TestManagerExpiresProposalAndReproposesUnchangedIssue(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	current := now
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{admissionIssueFixture("issue-1", "DD-1", 1, now)},
		Stateful: true,
		Now:      func() time.Time { return current },
	})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return current })

	first, err := manager.RunOnce(context.Background())
	if err != nil || len(first.Proposals) != 1 {
		t.Fatalf("first RunOnce() = %#v, %v", first, err)
	}
	current = now.AddDate(0, 0, settings.Config.ProposalExpiryDays+1)
	second, err := manager.RunOnce(context.Background())
	if err != nil || len(second.Proposals) != 1 {
		t.Fatalf("second RunOnce() = %#v, %v", second, err)
	}
	history, err := backend.AdmissionProposalHistory(context.Background(), "detent", "issue-1")
	if err != nil {
		t.Fatalf("AdmissionProposalHistory() error = %v", err)
	}
	if len(history) != 2 || history[0].Status != admissionmodel.ProposalOpen || history[1].Status != admissionmodel.ProposalExpired {
		t.Fatalf("history = %#v", history)
	}
}

func TestManagerAuditCommentDoesNotDuplicateProposal(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{admissionIssueFixture("issue-1", "DD-1", 1, now)},
		Stateful: true,
		Now:      func() time.Time { return now },
	})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	manager := newAdmissionTestManager(t, admissionTestSettings(tracker, agent), backend, func() time.Time { return now })
	if result, err := manager.RunOnce(context.Background()); err != nil || len(result.Proposals) != 1 {
		t.Fatalf("first RunOnce() = %#v, %v", result, err)
	}
	second, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if agent.calls != 1 || second.Skipped["unchanged_open_proposal"] != 1 {
		t.Fatalf("second result = %#v, runner calls = %d", second, agent.calls)
	}
	history, err := backend.AdmissionProposalHistory(context.Background(), "detent", "issue-1")
	if err != nil || len(history) != 1 {
		t.Fatalf("history = %#v, %v", history, err)
	}
}

func TestManagerAcceptedDemotionIsSticky(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{admissionIssueFixture("issue-1", "DD-1", 1, now)},
		Stateful: true,
	})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	manager := newAdmissionTestManager(t, admissionTestSettings(tracker, agent), backend, func() time.Time { return now })
	if result, err := manager.RunOnce(context.Background()); err != nil || len(result.Proposals) != 1 {
		t.Fatalf("initial RunOnce() = %#v, %v", result, err)
	}
	if err := tracker.UpdateIssueState(context.Background(), "issue-1", "Todo"); err != nil {
		t.Fatalf("UpdateIssueState(Todo) error = %v", err)
	}
	if _, err := manager.RunOnce(context.Background()); err != nil {
		t.Fatalf("acceptance reconciliation error = %v", err)
	}
	if err := tracker.UpdateIssueState(context.Background(), "issue-1", "Backlog"); err != nil {
		t.Fatalf("UpdateIssueState(Backlog) error = %v", err)
	}
	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("demotion RunOnce() error = %v", err)
	}
	if agent.calls != 1 || result.Skipped["accepted_demotion"] != 1 {
		t.Fatalf("demotion result = %#v, runner calls = %d", result, agent.calls)
	}
	history, err := backend.AdmissionProposalHistory(context.Background(), "detent", "issue-1")
	if err != nil || len(history) != 1 || history[0].Status != admissionmodel.ProposalAccepted {
		t.Fatalf("history = %#v, %v", history, err)
	}
}

func TestManagerDefersForCapacityAndBudget(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tracker := memory.New(memory.Config{Issues: []connector.Issue{admissionIssueFixture("issue-1", "DD-1", 1, now)}, Stateful: true})
	tests := []struct {
		name     string
		settings func(*scriptedAdmissionRunner) Settings
		want     string
	}{
		{
			name: "capacity",
			settings: func(agent *scriptedAdmissionRunner) Settings {
				settings := admissionTestSettings(tracker, agent)
				localScheduler := scheduler.NewCountingSemaphore(scheduler.Config{Capacity: 1})
				if _, err := localScheduler.RequestSlot(context.Background(), scheduler.SlotRequest{State: "Todo"}); err != nil {
					t.Fatalf("hold scheduler slot: %v", err)
				}
				settings.Scheduler = localScheduler
				return settings
			},
			want: "fleet_capacity",
		},
		{
			name: "budget",
			settings: func(agent *scriptedAdmissionRunner) Settings {
				settings := admissionTestSettings(tracker, &budgetAdmissionRunner{
					scriptedAdmissionRunner: agent,
					status: runner.DailyBudgetStatus{
						Active:          true,
						CurrentSpendUSD: 100,
						MaxUSD:          100,
					},
				})
				return settings
			},
			want: "budget",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := openManagerTestStore(t)
			agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
			manager := newAdmissionTestManager(t, tt.settings(agent), backend, func() time.Time { return now })
			result, err := manager.RunOnce(context.Background())
			if err != nil {
				t.Fatalf("RunOnce() error = %v", err)
			}
			if result.DeferredReason != tt.want || agent.calls != 0 {
				t.Fatalf("result = %#v, runner calls = %d", result, agent.calls)
			}
		})
	}
}

func TestManagerFiltersLocallyAndRejectsFabricatedCriteria(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	allowed := admissionIssueFixture("allowed", "DD-1", 1, now)
	allowed.AuthorID = "octocat"
	excluded := admissionIssueFixture("excluded", "DD-2", 2, now)
	excluded.AuthorID = "octocat"
	excluded.Labels = []string{"do-not-admit"}
	otherAuthor := admissionIssueFixture("other", "DD-3", 3, now)
	otherAuthor.AuthorID = "hubot"
	tracker := memory.New(memory.Config{Issues: []connector.Issue{allowed, excluded, otherAuthor}, Stateful: true})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: func(request runner.RunRequest) []AgentProposal {
		return []AgentProposal{{
			IssueID: request.Admission.Candidates[0].ID,
			Findings: []admissionmodel.Finding{{
				Dimension:      "Alignment",
				CriterionQuote: "fabricated criterion",
				Matched:        true,
				Rationale:      "Untrusted issue text claimed a match.",
			}},
			Confidence: float64Pointer(1),
		}}
	}}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.ExcludeLabels = []string{"do-not-admit"}
	settings.Config.Authors.Allow = []string{"octocat"}
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })
	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if got, want := agent.candidateIDs[0], []string{"allowed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate ids = %#v, want %#v", got, want)
	}
	if len(result.Proposals) != 0 || result.Skipped["excluded_label"] != 1 ||
		result.Skipped["author"] != 1 || result.Skipped["invalid_agent_proposal"] != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestManagerLocalSQLiteStatesOnlyEndToEnd(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tracker, err := local.New(local.Config{
		Path:           filepath.Join(t.TempDir(), "tracker.db"),
		ProjectID:      "detent",
		Issues:         []connector.Issue{admissionIssueFixture("local-1", "LOCAL-1", 1, now)},
		ObservedStates: []string{"Backlog"},
		ActiveStates:   []string{"Todo"},
		TerminalStates: []string{"Done"},
	})
	if err != nil {
		t.Fatalf("local.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := tracker.Close(); err != nil {
			t.Fatalf("tracker.Close() error = %v", err)
		}
	})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.ExcludeLabels = nil
	settings.Config.Authors.Allow = nil
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })
	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(result.Proposals) != 1 || result.Proposals[0].IssueID != "local-1" {
		t.Fatalf("result = %#v", result)
	}
	issues, err := tracker.FetchIssuesByStates(context.Background(), []string{"Backlog"})
	if err != nil || len(issues) != 1 || len(issues[0].Comments) != 1 {
		t.Fatalf("persisted tracker issue = %#v, %v", issues, err)
	}
}

func TestManagerReservesCommentCapacityBeforeCreatingProposals(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	uncommented := admissionIssueFixture("issue-0", "DD-0", 1, now.Add(-time.Hour))
	candidate := admissionIssueFixture("issue-1", "DD-1", 2, now)
	tracker := memory.New(memory.Config{Issues: []connector.Issue{uncommented, candidate}, Stateful: true})
	backend := openManagerTestStore(t)
	created, err := backend.CreateAdmissionProposal(context.Background(), admissionmodel.Proposal{
		ID:              "admission-uncommented",
		ProjectID:       "detent",
		IssueID:         uncommented.ID,
		IssueIdentifier: uncommented.Identifier,
		TargetState:     "Todo",
		Fingerprint:     issueFingerprint(uncommented),
		CriteriaSection: "Admission criteria",
		CriteriaText:    admissionTestCriteria().Text,
		Findings: []admissionmodel.Finding{{
			Dimension:      "Alignment",
			CriterionQuote: "serves a stated current priority",
			Matched:        true,
			Rationale:      "Supports the priority.",
		}},
		Confidence: 0.8,
		Status:     admissionmodel.ProposalOpen,
		CreatedAt:  now.Add(-time.Hour),
		ExpiresAt:  now.AddDate(0, 0, 7),
	})
	if err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %v, %v", created, err)
	}
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.MaxProposalsPerRun = 1
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if agent.calls != 0 || result.Skipped["comment_cap"] != 1 || len(result.Proposals) != 0 {
		t.Fatalf("result = %#v, runner calls = %d", result, agent.calls)
	}
	open, err := backend.OpenAdmissionProposals(context.Background(), "detent", 0)
	if err != nil || len(open) != 1 || open[0].CommentedAt.IsZero() {
		t.Fatalf("open proposals = %#v, %v", open, err)
	}
}

func TestManagerRejectsMissingConfidence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tracker := memory.New(memory.Config{Issues: []connector.Issue{admissionIssueFixture("issue-1", "DD-1", 1, now)}, Stateful: true})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: func(request runner.RunRequest) []AgentProposal {
		return []AgentProposal{{
			IssueID: request.Admission.Candidates[0].ID,
			Findings: []admissionmodel.Finding{{
				Dimension:      "Alignment",
				CriterionQuote: "serves a stated current priority",
				Matched:        true,
				Rationale:      "Supports the priority.",
			}},
		}}
	}}
	manager := newAdmissionTestManager(t, admissionTestSettings(tracker, agent), backend, func() time.Time { return now })
	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(result.Proposals) != 0 || result.Skipped["invalid_agent_proposal"] != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestParseProposalsRequiresTypedEnvelope(t *testing.T) {
	t.Parallel()

	if _, err := parseProposals(`{}`); err == nil {
		t.Fatal("parseProposals({}) error = nil")
	}
	proposals, err := parseProposals(`{"proposals":[]}`)
	if err != nil || len(proposals) != 0 {
		t.Fatalf("parseProposals(empty) = %#v, %v", proposals, err)
	}
}

type scriptedAdmissionRunner struct {
	propose      func(runner.RunRequest) []AgentProposal
	calls        int
	candidateIDs [][]string
}

func (r *scriptedAdmissionRunner) Run(ctx context.Context, request runner.RunRequest) (runner.RunResult, error) {
	r.calls++
	ids := make([]string, 0, len(request.Admission.Candidates))
	for _, candidate := range request.Admission.Candidates {
		ids = append(ids, candidate.ID)
	}
	r.candidateIDs = append(r.candidateIDs, ids)
	for _, proposal := range r.propose(request) {
		raw, err := json.Marshal(proposal)
		if err != nil {
			return runner.RunResult{}, err
		}
		result, err := request.AgentToolHandler(ctx, runner.AgentToolCall{Name: ProposalToolName, Arguments: raw})
		if err != nil {
			return runner.RunResult{}, err
		}
		if !result.Success {
			return runner.RunResult{}, ErrInvalidOutput
		}
	}
	return runner.RunResult{FinalState: runner.FinalStateCompleted}, nil
}

type budgetAdmissionRunner struct {
	*scriptedAdmissionRunner
	status runner.DailyBudgetStatus
}

func (r *budgetAdmissionRunner) DailyBudgetStatus(context.Context, time.Time) (runner.DailyBudgetStatus, bool, error) {
	return r.status, true, nil
}

func proposeEveryCandidate(request runner.RunRequest) []AgentProposal {
	proposals := make([]AgentProposal, 0, len(request.Admission.Candidates))
	for _, candidate := range request.Admission.Candidates {
		proposals = append(proposals, AgentProposal{
			IssueID: candidate.ID,
			Findings: []admissionmodel.Finding{{
				Dimension:      "Alignment",
				CriterionQuote: "serves a stated current priority",
				Matched:        true,
				Rationale:      "The issue directly supports the stated priority.",
			}},
			Confidence: float64Pointer(0.8),
		})
	}
	return proposals
}

func admissionTestSettings(tracker IssueStore, backend runner.Backend) Settings {
	cfg := config.BacklogAdmission{
		Enabled:             true,
		Schedule:            "0 6 * * 1-5",
		Sources:             config.BacklogAdmissionSources{States: []string{"Backlog"}},
		TargetState:         "Todo",
		CriteriaSection:     "Admission criteria",
		MaxCandidatesPerRun: 50,
		MaxProposalsPerRun:  3,
		MaxOpenProposals:    10,
		ProposalExpiryDays:  7,
	}
	return Settings{
		ProjectID:      "detent",
		Config:         cfg,
		Criteria:       admissionTestCriteria(),
		DispatchStates: []string{"Backlog"},
		Runner:         backend,
		Issues:         tracker,
	}
}

func float64Pointer(value float64) *float64 {
	return &value
}

func admissionTestCriteria() config.AdmissionCriteria {
	return config.AdmissionCriteria{
		Section: "Admission criteria",
		Text:    "- **Alignment** — serves a stated current priority.\n- **Readiness** — has an actionable problem statement.",
		Dimensions: []config.AdmissionDimension{
			{Name: "Alignment", Text: "- **Alignment** — serves a stated current priority."},
			{Name: "Readiness", Text: "- **Readiness** — has an actionable problem statement."},
		},
	}
}

func admissionIssueFixture(id string, identifier string, priority int, createdAt time.Time) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = identifier
	issue.Title = "Candidate " + identifier
	issue.Description = "Actionable problem statement for " + identifier
	issue.State = "Backlog"
	issue.Priority = &priority
	issue.CreatedAt = &createdAt
	return issue
}

func openManagerTestStore(t *testing.T) store.Store {
	t.Helper()
	backend, err := store.Open(context.Background(), store.Config{
		Backend: store.BackendSQLite,
		Path:    filepath.Join(t.TempDir(), "runtime.db"),
	})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("backend.Close() error = %v", err)
		}
	})
	return backend
}

func newAdmissionTestManager(t *testing.T, settings Settings, backend Store, now func() time.Time) *Manager {
	t.Helper()
	manager, err := New(settings, backend, nil, now)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return manager
}

func TestIssueFingerprintIgnoresAuditCommentMetadata(t *testing.T) {
	t.Parallel()

	issue := admissionIssueFixture("issue-1", "DD-1", 1, time.Now())
	before := issueFingerprint(issue)
	updated := time.Now().Add(time.Hour)
	issue.UpdatedAt = &updated
	issue.Comments = []connector.IssueComment{{Body: "audit comment"}}
	after := issueFingerprint(issue)
	if before != after {
		t.Fatalf("fingerprint changed from %q to %q after comment metadata", before, after)
	}
	issue.Description += " changed"
	if issueFingerprint(issue) == after {
		t.Fatal("fingerprint did not change after body edit")
	}
}

func TestProposalCommentQuotesCriteriaAndDoesNotUseStatusLabel(t *testing.T) {
	t.Parallel()

	proposal := admissionmodel.Proposal{
		ID:              "admission-1",
		TargetState:     "Todo",
		CriteriaSection: "Admission criteria",
		Findings: []admissionmodel.Finding{{
			Dimension:      "Alignment",
			CriterionQuote: "serves a stated current priority",
			Rationale:      "Supports the release.",
		}},
		Confidence: 0.8,
		ExpiresAt:  time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
	}
	comment := proposalComment(proposal)
	for _, want := range []string{"serves a stated current priority", "Detent has not changed the issue status", "admission-1"} {
		if !strings.Contains(comment, want) {
			t.Fatalf("proposalComment() missing %q: %s", want, comment)
		}
	}
	if strings.Contains(comment, "detent:admission") {
		t.Fatalf("proposalComment() introduced a status-prefixed label: %s", comment)
	}
}
