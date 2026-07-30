package admission

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/local"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/provenance"
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

func TestManagerRecordsCandidateReaderTruncation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	issues := make([]connector.Issue, connector.DefaultCandidatePageSize+2)
	for index := range issues {
		issues[index] = admissionIssueFixture(
			"issue-"+strconv.Itoa(index),
			"DD-"+strconv.Itoa(index),
			1,
			now.Add(time.Duration(index)*time.Minute),
		)
		issues[index].Labels = []string{"skip"}
	}
	tracker := memory.New(memory.Config{Issues: issues, Stateful: true, Now: func() time.Time { return now }})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.ExcludeLabels = []string{"skip"}
	settings.Config.MaxCandidatesPerRun = 2
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Truncated["candidate_reader"] != 1 || result.CandidatesFound != connector.DefaultCandidatePageSize {
		t.Fatalf("result = %#v, want reader truncation at %d candidates", result, connector.DefaultCandidatePageSize)
	}
	if agent.calls != 0 {
		t.Fatalf("runner calls = %d, want no eligible candidates", agent.calls)
	}
	record, ok, err := backend.LatestAdmissionRun(context.Background(), "detent")
	if err != nil || !ok {
		t.Fatalf("LatestAdmissionRun() = %#v, %t, %v", record, ok, err)
	}
	if record.Truncated["candidate_reader"] != 1 {
		t.Fatalf("run ledger truncation = %#v, want candidate_reader=1", record.Truncated)
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
	outcomes, err := backend.AdmissionDownstreamOutcomes(context.Background(), "detent")
	if err != nil || len(outcomes) != 0 {
		t.Fatalf("AdmissionDownstreamOutcomes() = %#v, %v, want expiry not counted as acceptance", outcomes, err)
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

func TestManagerReconcilesAgainstStoredProposalTarget(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	issue.State = "Todo"
	transitionAt := now.Add(2 * time.Minute)
	decisionAt := now.Add(time.Minute)
	issue.StageUpdatedAt = &transitionAt
	issue.Comments = []connector.IssueComment{{
		ID:               "decision-1",
		Body:             admissionAcceptCommand("proposal-1"),
		AuthorLogin:      "ada",
		AuthorKind:       "User",
		AuthorAuthorized: true,
		CreatedAt:        &decisionAt,
	}}
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-1", issue, now)
	if created, err := backend.CreateAdmissionProposal(context.Background(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.TargetState = "Ready"
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return transitionAt })

	if _, err := manager.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	history, err := backend.AdmissionProposalHistory(context.Background(), "detent", issue.ID)
	if err != nil || len(history) != 1 || history[0].Status != admissionmodel.ProposalAccepted {
		t.Fatalf("history = %#v, %v", history, err)
	}
	if history[0].DecisionSeconds != 60 || !history[0].TransitionAt.Equal(transitionAt) ||
		history[0].DecisionCommentID != "decision-1" {
		t.Fatalf("decision evidence = %#v", history[0])
	}
	if agent.calls != 0 {
		t.Fatalf("runner calls = %d, want 0", agent.calls)
	}
}

func TestManagerReconcilesAcceptanceAfterIssueLeavesTargetState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	current := now
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{issue},
		Stateful: true,
		Now:      func() time.Time { return current },
	})
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-1", issue, now)
	if created, err := backend.CreateAdmissionProposal(ctx, proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	decisionAt := now.Add(time.Minute)
	current = decisionAt
	if err := tracker.CreateComment(ctx, issue.ID, admissionAcceptCommand(proposal.ID)); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	targetAt := now.Add(2 * time.Minute)
	current = targetAt
	if err := tracker.UpdateIssueState(ctx, issue.ID, proposal.TargetState); err != nil {
		t.Fatalf("UpdateIssueState(%s) error = %v", proposal.TargetState, err)
	}
	if _, err := backend.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
		ProjectID:  proposal.ProjectID,
		IssueID:    proposal.IssueID,
		Identifier: proposal.IssueIdentifier,
		IssueURL:   proposal.IssueURL,
		PhaseType:  store.WorkflowPhaseTypeLane,
		PhaseName:  proposal.TargetState,
		Reason:     "tracker_state_observed",
		Status:     "entered",
		StartedAt:  targetAt,
		MetadataJSON: provenance.Apply(
			"{}",
			provenance.Attribution{Origin: provenance.OriginUnknown},
			&provenance.Admission{Attributed: false},
		),
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
	current = now.Add(3 * time.Minute)
	if err := tracker.UpdateIssueState(ctx, issue.ID, "In Progress"); err != nil {
		t.Fatalf("UpdateIssueState(In Progress) error = %v", err)
	}
	manager := newAdmissionTestManager(
		t,
		admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate}),
		backend,
		func() time.Time { return current },
	)

	if _, err := manager.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	history, err := backend.AdmissionProposalHistory(ctx, proposal.ProjectID, proposal.IssueID)
	if err != nil || len(history) != 1 {
		t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
	}
	if history[0].Status != admissionmodel.ProposalAccepted || !history[0].TransitionAt.Equal(targetAt) {
		t.Fatalf("accepted proposal = %#v", history[0])
	}
	timeline, err := backend.IssueWorkflowTimeline(ctx, store.IssueIdentity{IssueID: proposal.IssueID})
	if err != nil || len(timeline.Events) != 1 ||
		timeline.Events[0].Reason != "admission_proposal_accepted" {
		t.Fatalf("IssueWorkflowTimeline() = %#v, %v", timeline, err)
	}
}

func TestManagerTargetTransitionWithoutDecisionRemainsOpen(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	issue.State = "Todo"
	transitionAt := now.Add(time.Minute)
	issue.StageUpdatedAt = &transitionAt
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-1", issue, now)
	if created, err := backend.CreateAdmissionProposal(context.Background(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	manager := newAdmissionTestManager(
		t,
		admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate}),
		backend,
		func() time.Time { return transitionAt },
	)
	if _, err := manager.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	open, err := backend.OpenAdmissionProposals(context.Background(), "detent", 0)
	if err != nil || len(open) != 1 || open[0].ID != proposal.ID {
		t.Fatalf("OpenAdmissionProposals() = %#v, %v, want proposal open", open, err)
	}
	outcomes, err := backend.AdmissionDownstreamOutcomes(context.Background(), "detent")
	if err != nil || len(outcomes) != 0 {
		t.Fatalf("AdmissionDownstreamOutcomes() = %#v, %v, want none", outcomes, err)
	}
}

func TestManagerAcceptanceTransitionsOrSupersedes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		current    string
		wantState  string
		wantStatus admissionmodel.ProposalStatus
		wantReason string
	}{
		{
			name:       "source issue is admitted",
			current:    "Backlog",
			wantState:  "Todo",
			wantStatus: admissionmodel.ProposalAccepted,
			wantReason: admissionResolutionExplicitAccept,
		},
		{
			name:       "issue that left source is superseded",
			current:    "In Progress",
			wantState:  "In Progress",
			wantStatus: admissionmodel.ProposalSuperseded,
			wantReason: admissionResolutionSourceStateChanged,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			currentTime := now
			issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
			tracker := memory.New(memory.Config{
				Issues:   []connector.Issue{issue},
				Stateful: true,
				Now:      func() time.Time { return currentTime },
			})
			backend := openManagerTestStore(t)
			proposal := admissionTestProposalForIssue("proposal-1", issue, now)
			if created, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil || !created {
				t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
			}
			currentTime = now.Add(time.Minute)
			if err := tracker.CreateComment(t.Context(), issue.ID, admissionAcceptCommand(proposal.ID)); err != nil {
				t.Fatalf("CreateComment() error = %v", err)
			}
			if tt.current != issue.State {
				if err := tracker.UpdateIssueState(t.Context(), issue.ID, tt.current); err != nil {
					t.Fatalf("UpdateIssueState(%s) error = %v", tt.current, err)
				}
			}
			manager := newAdmissionTestManager(
				t,
				admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate}),
				backend,
				func() time.Time { return currentTime },
			)

			if _, err := manager.RunOnce(t.Context()); err != nil {
				t.Fatalf("RunOnce() error = %v", err)
			}
			issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
			if err != nil || len(issues) != 1 || issues[0].State != tt.wantState {
				t.Fatalf("issue state = %#v, %v, want %s", issues, err, tt.wantState)
			}
			history, err := backend.AdmissionProposalHistory(t.Context(), proposal.ProjectID, issue.ID)
			if err != nil || len(history) != 1 {
				t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
			}
			if history[0].Status != tt.wantStatus || history[0].ResolutionReason != tt.wantReason {
				t.Fatalf("resolved proposal = %#v", history[0])
			}
			if tt.wantStatus != admissionmodel.ProposalAccepted {
				return
			}
			timeline, err := backend.IssueWorkflowTimeline(t.Context(), store.IssueIdentity{IssueID: issue.ID})
			if err != nil || len(timeline.Events) != 1 {
				t.Fatalf("IssueWorkflowTimeline() = %#v, %v", timeline, err)
			}
			metadata, ok := provenance.Parse(timeline.Events[0].MetadataJSON)
			if !ok || metadata.Provenance.Actor == nil ||
				metadata.Provenance.Actor.Login != "memory" ||
				metadata.Admission == nil ||
				!metadata.Admission.Attributed ||
				metadata.Admission.ProposalID != proposal.ID {
				t.Fatalf("admission provenance = %#v", metadata)
			}
		})
	}
}

func TestManagerIgnoresUnauthorizedAdmissionDecision(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	decisionAt := now.Add(time.Minute)
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	issue.Comments = []connector.IssueComment{{
		ID:               "decision-1",
		Backend:          connector.BackendGitHub.String(),
		Body:             admissionAcceptCommand("proposal-1"),
		AuthorLogin:      "outsider",
		AuthorKind:       "User",
		AuthorAuthorized: true,
		CreatedAt:        &decisionAt,
	}}
	tracker := &authorizingAdmissionIssueStore{
		Connector:  memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true}),
		authorized: false,
	}
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-1", issue, now)
	proposal.CommentedAt = now
	if created, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	manager := newAdmissionTestManager(
		t,
		admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate}),
		backend,
		func() time.Time { return decisionAt },
	)

	if _, err := manager.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
	if err != nil || len(issues) != 1 || issues[0].State != "Backlog" {
		t.Fatalf("issue = %#v, %v", issues, err)
	}
	history, err := backend.AdmissionProposalHistory(t.Context(), proposal.ProjectID, proposal.IssueID)
	if err != nil || len(history) != 1 || history[0].Status != admissionmodel.ProposalOpen {
		t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
	}
}

func TestManagerAcceptanceRevalidatesImmediatelyBeforeMutation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	currentTime := now
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	tracker := &stateChangingAdmissionStore{
		Connector: memory.New(memory.Config{
			Issues:   []connector.Issue{issue},
			Stateful: true,
			Now:      func() time.Time { return currentTime },
		}),
		issueID: issue.ID,
		state:   "In Progress",
	}
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-1", issue, now)
	if created, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	currentTime = now.Add(time.Minute)
	if err := tracker.CreateComment(t.Context(), issue.ID, admissionAcceptCommand(proposal.ID)); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	manager := newAdmissionTestManager(
		t,
		admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate}),
		backend,
		func() time.Time { return currentTime },
	)

	if _, err := manager.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	issues, err := tracker.Connector.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
	if err != nil || len(issues) != 1 || issues[0].State != "In Progress" {
		t.Fatalf("issue = %#v, %v", issues, err)
	}
	history, err := backend.AdmissionProposalHistory(t.Context(), proposal.ProjectID, issue.ID)
	if err != nil || len(history) != 1 ||
		history[0].Status != admissionmodel.ProposalSuperseded ||
		history[0].ResolutionReason != admissionResolutionSourceStateChanged {
		t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
	}
}

func TestManagerAutoAdmissionModes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		enabled    bool
		confidence float64
		wantState  string
		wantStatus admissionmodel.ProposalStatus
	}{
		{
			name:       "disabled requires explicit acceptance",
			confidence: 1,
			wantState:  "Backlog",
			wantStatus: admissionmodel.ProposalOpen,
		},
		{
			name:       "below threshold remains proposed",
			enabled:    true,
			confidence: 0.89,
			wantState:  "Backlog",
			wantStatus: admissionmodel.ProposalOpen,
		},
		{
			name:       "threshold match is admitted",
			enabled:    true,
			confidence: 0.9,
			wantState:  "Todo",
			wantStatus: admissionmodel.ProposalAccepted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
			tracker := memory.New(memory.Config{
				Issues:   []connector.Issue{issue},
				Stateful: true,
				Now:      func() time.Time { return now },
			})
			backend := openManagerTestStore(t)
			agent := &scriptedAdmissionRunner{propose: proposeEveryCandidateAtConfidence(tt.confidence)}
			settings := admissionTestSettings(tracker, agent)
			settings.Config.AutoAdmit = tt.enabled
			settings.Config.AutoAdmitMinConfidence = 0.9
			manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

			result, err := manager.RunOnce(t.Context())
			if err != nil || len(result.Proposals) != 1 {
				t.Fatalf("RunOnce() = %#v, %v", result, err)
			}
			issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
			if err != nil || len(issues) != 1 || issues[0].State != tt.wantState {
				t.Fatalf("issue state = %#v, %v, want %s", issues, err, tt.wantState)
			}
			history, err := backend.AdmissionProposalHistory(t.Context(), "detent", issue.ID)
			if err != nil || len(history) != 1 || history[0].Status != tt.wantStatus {
				t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
			}
			if tt.wantStatus == admissionmodel.ProposalAccepted &&
				history[0].ResolutionReason != admissionResolutionAutoAdmit {
				t.Fatalf("auto-admitted proposal = %#v", history[0])
			}
		})
	}
}

func TestManagerRecoversAutoAdmissionAfterResolutionFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{issue},
		Stateful: true,
		Now:      func() time.Time { return now },
	})
	backend := openManagerTestStore(t)
	failingStore := &failingAdmissionResolutionStore{Store: backend, fail: true}
	settings := admissionTestSettings(
		tracker,
		&scriptedAdmissionRunner{propose: proposeEveryCandidateAtConfidence(1)},
	)
	settings.Config.AutoAdmit = true
	settings.Config.AutoAdmitMinConfidence = 0.9
	manager := newAdmissionTestManager(t, settings, failingStore, func() time.Time { return now })

	if _, err := manager.RunOnce(t.Context()); !errors.Is(err, errAdmissionResolutionFailure) {
		t.Fatalf("first RunOnce() error = %v, want %v", err, errAdmissionResolutionFailure)
	}
	issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
	if err != nil || len(issues) != 1 || issues[0].State != "Todo" {
		t.Fatalf("issue after failed resolution = %#v, %v", issues, err)
	}
	if _, err := manager.RunOnce(t.Context()); err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	history, err := backend.AdmissionProposalHistory(t.Context(), "detent", issue.ID)
	if err != nil || len(history) != 1 ||
		history[0].Status != admissionmodel.ProposalAccepted ||
		history[0].ResolutionReason != admissionResolutionAutoAdmit {
		t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
	}
	stateUpdates := 0
	for _, event := range tracker.Events() {
		if event.Kind == memory.EventKindStateUpdate {
			stateUpdates++
		}
	}
	if stateUpdates != 1 {
		t.Fatalf("state update events = %d, want 1", stateUpdates)
	}
}

func TestManagerAutoAdmissionRespectsEligibilityAndCaps(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	first := admissionIssueFixture("issue-1", "DD-1", 1, now)
	first.AuthorID = "octocat"
	second := admissionIssueFixture("issue-2", "DD-2", 2, now)
	second.AuthorID = "octocat"
	excluded := admissionIssueFixture("issue-3", "DD-3", 3, now)
	excluded.AuthorID = "octocat"
	excluded.Labels = []string{"do-not-admit"}
	otherAuthor := admissionIssueFixture("issue-4", "DD-4", 4, now)
	otherAuthor.AuthorID = "hubot"
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{first, second, excluded, otherAuthor},
		Stateful: true,
		Now:      func() time.Time { return now },
	})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.AutoAdmit = true
	settings.Config.AutoAdmitMinConfidence = 0.8
	settings.Config.MaxProposalsPerRun = 1
	settings.Config.MaxOpenProposals = 1
	settings.Config.ExcludeLabels = []string{"do-not-admit"}
	settings.Config.Authors.Allow = []string{"octocat"}
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Skipped["excluded_label"] != 1 || result.Skipped["author"] != 1 ||
		result.Truncated["proposals"] != 1 {
		t.Fatalf("result = %#v", result)
	}
	issues, err := tracker.FetchIssueStatesByIDs(
		t.Context(),
		[]string{first.ID, second.ID, excluded.ID, otherAuthor.ID},
	)
	if err != nil || len(issues) != 4 {
		t.Fatalf("FetchIssueStatesByIDs() = %#v, %v", issues, err)
	}
	states := map[string]string{}
	for _, issue := range issues {
		states[issue.ID] = issue.State
	}
	if states[first.ID] != "Todo" ||
		states[second.ID] != "Backlog" ||
		states[excluded.ID] != "Backlog" ||
		states[otherAuthor.ID] != "Backlog" {
		t.Fatalf("states = %#v", states)
	}
}

func TestManagerAutoAdmissionRespectsOpenProposalCap(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	first := admissionIssueFixture("issue-1", "DD-1", 1, now)
	second := admissionIssueFixture("issue-2", "DD-2", 2, now)
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{first, second},
		Stateful: true,
		Now:      func() time.Time { return now },
	})
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-open", first, now)
	proposal.Confidence = 0.5
	if created, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.AutoAdmit = true
	settings.Config.AutoAdmitMinConfidence = 0.9
	settings.Config.MaxOpenProposals = 1
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if agent.calls != 0 || result.Skipped["open_proposal_cap"] != 1 {
		t.Fatalf("result = %#v, runner calls = %d", result, agent.calls)
	}
	issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{first.ID, second.ID})
	if err != nil || len(issues) != 2 || issues[0].State != "Backlog" || issues[1].State != "Backlog" {
		t.Fatalf("issues = %#v, %v", issues, err)
	}
}

func TestManagerAutoAdmissionSupersedesIneligibleOpenProposal(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	original := admissionIssueFixture("issue-1", "DD-1", 1, now)
	current := original
	current.Labels = []string{"do-not-admit"}
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{current},
		Stateful: true,
		Now:      func() time.Time { return now },
	})
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-open", original, now)
	proposal.Confidence = 1
	if created, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	settings := admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate})
	settings.Config.AutoAdmit = true
	settings.Config.AutoAdmitMinConfidence = 0.9
	settings.Config.ExcludeLabels = []string{"do-not-admit"}
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	if _, err := manager.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	history, err := backend.AdmissionProposalHistory(t.Context(), proposal.ProjectID, proposal.IssueID)
	if err != nil || len(history) != 1 ||
		history[0].Status != admissionmodel.ProposalSuperseded ||
		history[0].ResolutionReason != admissionResolutionAutoAdmitIneligible {
		t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
	}
	issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{proposal.IssueID})
	if err != nil || len(issues) != 1 || issues[0].State != "Backlog" {
		t.Fatalf("issue = %#v, %v", issues, err)
	}
}

func TestManagerFiltersProposalHistoryBeforeCandidateCap(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	issues := []connector.Issue{
		admissionIssueFixture("issue-1", "DD-1", 1, now),
		admissionIssueFixture("issue-2", "DD-2", 2, now),
		admissionIssueFixture("issue-3", "DD-3", 3, now),
	}
	tracker := memory.New(memory.Config{Issues: issues, Stateful: true})
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-accepted", issues[0], now.Add(-time.Hour))
	if created, err := backend.CreateAdmissionProposal(context.Background(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	if err := backend.TransitionAdmissionProposal(
		context.Background(),
		proposal.ID,
		admissionmodel.ProposalOpen,
		admissionmodel.ProposalAccepted,
		now,
	); err != nil {
		t.Fatalf("TransitionAdmissionProposal() error = %v", err)
	}
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.MaxCandidatesPerRun = 1
	settings.Config.MaxProposalsPerRun = 1
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if got, want := agent.candidateIDs[0], []string{"issue-2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate ids = %#v, want %#v", got, want)
	}
	if result.Skipped["accepted_demotion"] != 1 || result.Truncated["candidates"] != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestManagerAcceptedDemotionIsSticky(t *testing.T) {
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
	manager := newAdmissionTestManager(t, admissionTestSettings(tracker, agent), backend, func() time.Time { return current })
	if result, err := manager.RunOnce(context.Background()); err != nil || len(result.Proposals) != 1 {
		t.Fatalf("initial RunOnce() = %#v, %v", result, err)
	}
	open, err := backend.OpenAdmissionProposals(context.Background(), "detent", 0)
	if err != nil || len(open) != 1 {
		t.Fatalf("OpenAdmissionProposals() = %#v, %v", open, err)
	}
	current = now.Add(time.Minute)
	if err := tracker.CreateComment(context.Background(), "issue-1", admissionAcceptCommand(open[0].ID)); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	if _, err := manager.RunOnce(context.Background()); err != nil {
		t.Fatalf("acceptance reconciliation error = %v", err)
	}
	admitted, err := tracker.FetchIssueStatesByIDs(context.Background(), []string{"issue-1"})
	if err != nil || len(admitted) != 1 || admitted[0].State != "Todo" {
		t.Fatalf("admitted issue = %#v, %v, want Todo", admitted, err)
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

func TestManagerUnionsLabelCandidatesAndSkipsIneligibleStates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	stateAndLabel := admissionIssueFixture("both", "DD-1", 1, now)
	stateAndLabel.Labels = []string{"sentry"}
	labelOnly := admissionIssueFixture("label", "DD-2", 2, now.Add(time.Minute))
	labelOnly.State = "Needs Triage"
	labelOnly.Labels = []string{"needs-decision"}
	target := admissionIssueFixture("target", "DD-6", 6, now.Add(5*time.Minute))
	target.State = "Todo"
	target.Labels = []string{"sentry"}
	blocked := admissionIssueFixture("blocked", "DD-3", 3, now.Add(2*time.Minute))
	blocked.State = "Blocked"
	blocked.Labels = []string{"sentry"}
	terminal := admissionIssueFixture("terminal", "DD-4", 4, now.Add(3*time.Minute))
	terminal.State = "Done"
	terminal.Labels = []string{"sentry"}
	excluded := admissionIssueFixture("excluded", "DD-5", 5, now.Add(4*time.Minute))
	excluded.Labels = []string{"sentry", "skip"}
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{stateAndLabel, labelOnly, target, blocked, terminal, excluded},
		Stateful: true,
	})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.Sources.Labels = []string{"sentry", "needs-decision"}
	settings.Config.ExcludeLabels = []string{"skip"}
	settings.Config.MaxProposalsPerRun = 10
	settings.TerminalStates = []string{"Done"}
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.CandidatesFound != 6 || result.Candidates != 2 || len(result.Proposals) != 2 {
		t.Fatalf("result = %#v, want deduplicated union with two eligible candidates", result)
	}
	if result.Skipped["label_target_state"] != 1 ||
		result.Skipped["label_blocked_state"] != 1 ||
		result.Skipped["label_terminal_state"] != 1 ||
		result.Skipped["excluded_label"] != 1 {
		t.Fatalf("skipped = %#v", result.Skipped)
	}
	if got, want := agent.candidateIDs[0], []string{"both", "label"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate ids = %#v, want %#v", got, want)
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

type stateChangingAdmissionStore struct {
	*memory.Connector
	fetches int
	issueID string
	state   string
}

type authorizingAdmissionIssueStore struct {
	*memory.Connector
	authorized bool
}

func (s *authorizingAdmissionIssueStore) IsIssueCommentAuthorAuthorized(
	context.Context,
	connector.Issue,
	connector.IssueComment,
) (bool, error) {
	return s.authorized, nil
}

var errAdmissionResolutionFailure = errors.New("admission resolution failed")

type failingAdmissionResolutionStore struct {
	Store
	fail bool
}

func (s *failingAdmissionResolutionStore) ResolveAdmissionProposal(
	ctx context.Context,
	decision admissionmodel.Decision,
) error {
	if s.fail && decision.Automatic && decision.Outcome == admissionmodel.ProposalAccepted {
		s.fail = false
		return errAdmissionResolutionFailure
	}
	return s.Store.ResolveAdmissionProposal(ctx, decision)
}

func (s *stateChangingAdmissionStore) FetchIssueStatesByIDs(
	ctx context.Context,
	issueIDs []string,
) ([]connector.Issue, error) {
	s.fetches++
	if s.fetches == 2 {
		if err := s.UpdateIssueState(ctx, s.issueID, s.state); err != nil {
			return nil, err
		}
	}
	return s.Connector.FetchIssueStatesByIDs(ctx, issueIDs)
}

func (r *budgetAdmissionRunner) DailyBudgetStatus(context.Context, time.Time) (runner.DailyBudgetStatus, bool, error) {
	return r.status, true, nil
}

func proposeEveryCandidate(request runner.RunRequest) []AgentProposal {
	return proposeEveryCandidateAtConfidence(0.8)(request)
}

func proposeEveryCandidateAtConfidence(confidence float64) func(runner.RunRequest) []AgentProposal {
	return func(request runner.RunRequest) []AgentProposal {
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
				Confidence: float64Pointer(confidence),
			})
		}
		return proposals
	}
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

func admissionTestProposalForIssue(id string, issue connector.Issue, now time.Time) admissionmodel.Proposal {
	return admissionmodel.Proposal{
		ID:              id,
		ProjectID:       "detent",
		IssueID:         issue.ID,
		IssueIdentifier: issue.Identifier,
		TargetState:     "Todo",
		Fingerprint:     issueFingerprint(issue),
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
		CreatedAt:  now,
		ExpiresAt:  now.AddDate(0, 0, 7),
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
	comment := proposalComment(proposal, false)
	for _, want := range []string{"serves a stated current priority", "have Detent move the issue", "admission-1"} {
		if !strings.Contains(comment, want) {
			t.Fatalf("proposalComment() missing %q: %s", want, comment)
		}
	}
	if strings.Contains(comment, "then move the issue") {
		t.Fatalf("proposalComment() still requires a manual move: %s", comment)
	}
	if strings.Contains(comment, "detent:admission") {
		t.Fatalf("proposalComment() introduced a status-prefixed label: %s", comment)
	}
}
