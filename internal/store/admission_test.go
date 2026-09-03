package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	"github.com/digitaldrywood/detent/internal/provenance"
)

func TestAdmissionProposalLifecycleAndIdempotency(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openAdmissionTestStore(t, ctx)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	first := admissionTestProposal("proposal-1", "fingerprint-1", now)
	first.RecommendedEffort = "high"
	first.EffortRationale = "The change crosses admission and dispatch."
	created, err := backend.CreateAdmissionProposal(ctx, first)
	if err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v, want created", created, err)
	}
	created, err = backend.CreateAdmissionProposal(ctx, admissionTestProposal("proposal-duplicate", "fingerprint-1", now.Add(time.Minute)))
	if err != nil || created {
		t.Fatalf("duplicate CreateAdmissionProposal() = %t, %v, want idempotent", created, err)
	}
	if err := backend.MarkAdmissionProposalCommented(ctx, first.ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("MarkAdmissionProposalCommented() error = %v", err)
	}
	second := admissionTestProposal("proposal-2", "fingerprint-2", now.Add(2*time.Minute))
	created, err = backend.CreateAdmissionProposal(ctx, second)
	if err != nil || !created {
		t.Fatalf("superseding CreateAdmissionProposal() = %t, %v, want created", created, err)
	}
	open, err := backend.OpenAdmissionProposals(ctx, "detent", 0)
	if err != nil {
		t.Fatalf("OpenAdmissionProposals() error = %v", err)
	}
	if len(open) != 1 || open[0].ID != second.ID || !open[0].CommentedAt.IsZero() {
		t.Fatalf("open proposals = %#v, want uncommented proposal-2", open)
	}
	if err := backend.TransitionAdmissionProposal(
		ctx,
		second.ID,
		admissionmodel.ProposalOpen,
		admissionmodel.ProposalAccepted,
		now.Add(3*time.Minute),
	); err != nil {
		t.Fatalf("TransitionAdmissionProposal() error = %v", err)
	}
	if err := backend.TransitionAdmissionProposal(
		ctx,
		second.ID,
		admissionmodel.ProposalOpen,
		admissionmodel.ProposalRejected,
		now.Add(4*time.Minute),
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale TransitionAdmissionProposal() error = %v, want ErrNotFound", err)
	}
	history, err := backend.AdmissionProposalHistory(ctx, "detent", "issue-1")
	if err != nil {
		t.Fatalf("AdmissionProposalHistory() error = %v", err)
	}
	if len(history) != 2 || history[0].Status != admissionmodel.ProposalAccepted ||
		history[1].Status != admissionmodel.ProposalSuperseded ||
		history[1].RecommendedEffort != "high" ||
		history[1].EffortRationale != "The change crosses admission and dispatch." {
		t.Fatalf("history = %#v", history)
	}
	count, err := backend.CountOpenAdmissionProposals(ctx, "detent")
	if err != nil || count != 0 {
		t.Fatalf("CountOpenAdmissionProposals() = %d, %v, want 0", count, err)
	}
}

func TestAdmissionDeclineLifecycleAndProposalSupersession(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	backend := openAdmissionTestStore(t, ctx)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	proposal := admissionTestProposal("proposal-1", "old-fingerprint", now.Add(-time.Minute))
	if created, err := backend.CreateAdmissionProposal(ctx, proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	decline := admissionmodel.Decline{
		ID:              "decline-1",
		ProjectID:       proposal.ProjectID,
		IssueID:         proposal.IssueID,
		IssueIdentifier: proposal.IssueIdentifier,
		IssueURL:        proposal.IssueURL,
		Fingerprint:     "declined-fingerprint",
		Reason:          "tracker",
		Detail:          "the issue is a tracker without a completion contract",
		CreatedAt:       now,
	}
	created, err := backend.CreateAdmissionDecline(ctx, decline)
	if err != nil || !created {
		t.Fatalf("CreateAdmissionDecline() = %t, %v", created, err)
	}
	created, err = backend.CreateAdmissionDecline(ctx, decline)
	if err != nil || created {
		t.Fatalf("duplicate CreateAdmissionDecline() = %t, %v", created, err)
	}
	got, found, err := backend.AdmissionDecline(ctx, decline.ProjectID, decline.IssueID, decline.Fingerprint)
	if err != nil || !found || got.ID != decline.ID || got.Reason != decline.Reason || !got.CommentedAt.IsZero() {
		t.Fatalf("AdmissionDecline() = %#v, %t, %v", got, found, err)
	}
	if err := backend.MarkAdmissionDeclineCommented(ctx, decline.ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("MarkAdmissionDeclineCommented() error = %v", err)
	}
	got, found, err = backend.AdmissionDecline(ctx, decline.ProjectID, decline.IssueID, decline.Fingerprint)
	if err != nil || !found || !got.CommentedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("commented AdmissionDecline() = %#v, %t, %v", got, found, err)
	}
	history, err := backend.AdmissionProposalHistory(ctx, proposal.ProjectID, proposal.IssueID)
	if err != nil || len(history) != 1 || history[0].Status != admissionmodel.ProposalSuperseded || history[0].ResolutionReason != admissionResolutionNonDeliverable {
		t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
	}
}

func TestAdmissionCriteriaDeclineSupersedesProposalWithCriteriaReason(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	backend := openAdmissionTestStore(t, ctx)
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	proposal := admissionTestProposal("proposal-criteria", "old-fingerprint", now.Add(-time.Minute))
	if created, err := backend.CreateAdmissionProposal(ctx, proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	confidence := 0.24
	decline := admissionmodel.Decline{
		ID:              "decline-criteria",
		ProjectID:       proposal.ProjectID,
		IssueID:         proposal.IssueID,
		IssueIdentifier: proposal.IssueIdentifier,
		IssueURL:        proposal.IssueURL,
		Fingerprint:     "criteria-fingerprint",
		Reason:          admissionDeclineCriteriaNotMet,
		Detail:          "the issue does not satisfy alignment",
		Confidence:      &confidence,
		FailedDimension: "Alignment",
		FailedCriterion: "serves a stated current priority",
		CreatedAt:       now,
	}
	if created, err := backend.CreateAdmissionDecline(ctx, decline); err != nil || !created {
		t.Fatalf("CreateAdmissionDecline() = %t, %v", created, err)
	}
	history, err := backend.AdmissionProposalHistory(ctx, proposal.ProjectID, proposal.IssueID)
	if err != nil || len(history) != 1 || history[0].Status != admissionmodel.ProposalSuperseded ||
		history[0].ResolutionReason != admissionResolutionCriteriaNotMet {
		t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
	}
}

func TestAdmissionProposalExpiryAndRunLedger(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openAdmissionTestStore(t, ctx)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	proposal := admissionTestProposal("proposal-expiring", "fingerprint", now)
	if created, err := backend.CreateAdmissionProposal(ctx, proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	expired, err := backend.ExpireAdmissionProposals(ctx, "detent", proposal.ExpiresAt.Add(time.Second))
	if err != nil || expired != 1 {
		t.Fatalf("ExpireAdmissionProposals() = %d, %v, want 1", expired, err)
	}
	history, err := backend.AdmissionProposalHistory(ctx, "detent", proposal.IssueID)
	if err != nil {
		t.Fatalf("AdmissionProposalHistory() error = %v", err)
	}
	if len(history) != 1 || history[0].Status != admissionmodel.ProposalExpired || history[0].ResolvedAt.IsZero() {
		t.Fatalf("history = %#v, want expired", history)
	}

	record := admissionmodel.RunRecord{
		ProjectID:       "detent",
		ScheduledFor:    now,
		StartedAt:       now.Add(time.Minute),
		CompletedAt:     now.Add(2 * time.Minute),
		Outcome:         "completed",
		ProposalReason:  "criteria_not_met",
		CandidatesFound: 4,
		Candidates:      2,
		Proposed:        1,
		Skipped:         map[string]int{"author": 1},
		Truncated:       map[string]int{"candidates": 1},
		Issues: []admissionmodel.IssueRecord{{
			ID:         "issue-1",
			Identifier: "DD-1",
			URL:        "https://example.test/issues/1",
			ProposalID: "proposal-expiring",
		}},
	}
	if err := backend.RecordAdmissionRun(ctx, record); err != nil {
		t.Fatalf("RecordAdmissionRun() error = %v", err)
	}
	got, ok, err := backend.LatestAdmissionRun(ctx, "detent")
	if err != nil || !ok {
		t.Fatalf("LatestAdmissionRun() = %#v, %t, %v", got, ok, err)
	}
	if got.CandidatesFound != 4 || got.Candidates != 2 || got.Proposed != 1 ||
		got.ProposalReason != "criteria_not_met" ||
		got.Skipped["author"] != 1 || got.Truncated["candidates"] != 1 ||
		len(got.Issues) != 1 || got.Issues[0].ProposalID != "proposal-expiring" {
		t.Fatalf("LatestAdmissionRun() = %#v", got)
	}
	runs, err := backend.RecentAdmissionRuns(ctx, "detent", 3)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentAdmissionRuns() = %#v, %v", runs, err)
	}
}

func TestAdmissionProposalsAwaitingDecision(t *testing.T) {
	ctx := context.Background()
	backend := openAdmissionTestStore(t, ctx)
	reader := backend.(AdmissionProposalDecisionReader)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	proposals := []admissionmodel.Proposal{
		admissionTestProposal("detent-open", "detent-open", now.Add(-2*time.Hour)),
		admissionTestProposal("detent-expiring", "detent-expiring", now.Add(-time.Hour)),
		admissionTestProposal("docs-open", "docs-open", now.Add(-30*time.Minute)),
		admissionTestProposal("detent-resolved", "detent-resolved", now.Add(-3*time.Hour)),
	}
	proposals[0].IssueID, proposals[0].IssueIdentifier = "issue-open", "DETENT-1"
	proposals[1].IssueID, proposals[1].IssueIdentifier = "issue-expiring", "DETENT-2"
	proposals[1].ExpiresAt = now
	proposals[2].ProjectID, proposals[2].IssueID, proposals[2].IssueIdentifier = "docs", "issue-docs", "DOCS-1"
	proposals[3].IssueID, proposals[3].IssueIdentifier = "issue-resolved", "DETENT-3"
	for _, proposal := range proposals {
		if created, err := backend.CreateAdmissionProposal(ctx, proposal); err != nil || !created {
			t.Fatalf("CreateAdmissionProposal(%q) = %t, %v", proposal.ID, created, err)
		}
	}
	if err := backend.TransitionAdmissionProposal(
		ctx,
		"detent-resolved",
		admissionmodel.ProposalOpen,
		admissionmodel.ProposalAccepted,
		now.Add(-time.Minute),
	); err != nil {
		t.Fatalf("TransitionAdmissionProposal() error = %v", err)
	}

	tests := []struct {
		name      string
		projectID string
		at        time.Time
		wantIDs   []string
	}{
		{name: "fleet excludes resolved and expired", at: now, wantIDs: []string{"detent-open", "docs-open"}},
		{name: "project scope", projectID: "detent", at: now, wantIDs: []string{"detent-open"}},
		{name: "before exact expiry", projectID: "detent", at: now.Add(-time.Second), wantIDs: []string{"detent-open", "detent-expiring"}},
		{name: "unknown project", projectID: "missing", at: now, wantIDs: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := reader.AdmissionProposalsAwaitingDecision(ctx, tt.projectID, tt.at)
			if err != nil {
				t.Fatalf("AdmissionProposalsAwaitingDecision() error = %v", err)
			}
			gotIDs := make([]string, 0, len(got))
			for _, proposal := range got {
				gotIDs = append(gotIDs, proposal.ID)
			}
			if !reflect.DeepEqual(gotIDs, tt.wantIDs) {
				t.Fatalf("AdmissionProposalsAwaitingDecision() IDs = %#v, want %#v", gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestAdmissionAcceptanceAttributionAndDownstreamOutcomes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openAdmissionTestStore(t, ctx)
	createdAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	decisionAt := createdAt.Add(time.Minute)
	transitionAt := createdAt.Add(2 * time.Minute)
	recordedTransitionAt := transitionAt.Add(-30 * time.Second)
	proposal := admissionTestProposal("proposal-accepted", "fingerprint-accepted", createdAt)
	if created, err := backend.CreateAdmissionProposal(ctx, proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	transitionEventID, err := backend.RecordWorkflowPhaseEvent(ctx, WorkflowPhaseEvent{
		ProjectID:    "detent",
		IssueID:      proposal.IssueID,
		Identifier:   proposal.IssueIdentifier,
		IssueURL:     proposal.IssueURL,
		PhaseType:    WorkflowPhaseTypeLane,
		PhaseName:    proposal.TargetState,
		Reason:       "tracker_state_observed",
		Status:       "entered",
		StartedAt:    recordedTransitionAt,
		MetadataJSON: provenance.Apply("{}", provenance.Attribution{Origin: provenance.OriginHuman}, &provenance.Admission{Attributed: false}),
	})
	if err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent(transition) error = %v", err)
	}
	if err := backend.ResolveAdmissionProposal(ctx, admissionmodel.Decision{
		ProposalID:        proposal.ID,
		Outcome:           admissionmodel.ProposalAccepted,
		DecidedAt:         decisionAt,
		CommentID:         "decision-1",
		ActorLogin:        "ada",
		ActorKind:         "User",
		TransitionAt:      transitionAt,
		TransitionEventID: transitionEventID,
	}); err != nil {
		t.Fatalf("ResolveAdmissionProposal() error = %v", err)
	}
	history, err := backend.AdmissionProposalHistory(ctx, proposal.ProjectID, proposal.IssueID)
	if err != nil || len(history) != 1 {
		t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
	}
	if got := history[0]; got.Status != admissionmodel.ProposalAccepted ||
		got.DecisionSeconds != 60 ||
		got.DecisionCommentID != "decision-1" ||
		got.DecisionActorLogin != "ada" ||
		!got.TransitionAt.Equal(transitionAt) {
		t.Fatalf("accepted proposal = %#v", got)
	}
	timeline, err := backend.IssueWorkflowTimeline(ctx, IssueIdentity{ProjectID: proposal.ProjectID, IssueID: proposal.IssueID})
	if err != nil {
		t.Fatalf("IssueWorkflowTimeline() error = %v", err)
	}
	if len(timeline.Events) != 1 || !timeline.Events[0].StartedAt.Equal(recordedTransitionAt) {
		t.Fatalf("workflow events = %#v, want one recorded transition", timeline.Events)
	}
	metadata, ok := provenance.Parse(timeline.Events[0].MetadataJSON)
	if !ok || timeline.Events[0].Reason != "admission_proposal_accepted" ||
		metadata.Provenance.Origin != provenance.OriginAdmission ||
		metadata.Admission == nil ||
		!metadata.Admission.Attributed ||
		metadata.Admission.ProposalID != proposal.ID {
		t.Fatalf("attributed workflow event = %#v, metadata = %#v", timeline.Events[0], metadata)
	}

	reworkAt := transitionAt.Add(time.Minute)
	reviewAt := reworkAt.Add(time.Minute)
	completedAt := reviewAt.Add(time.Minute)
	for _, event := range []WorkflowPhaseEvent{
		{
			ProjectID: "detent", IssueID: proposal.IssueID, PhaseType: WorkflowPhaseTypeLane,
			PhaseName: "Rework", Reason: "review_feedback", Status: "entered", StartedAt: reworkAt,
		},
		{
			ProjectID: "detent", IssueID: proposal.IssueID, PhaseType: WorkflowPhaseTypeReview,
			PhaseName: "automated_review", Reason: "p1_findings", Status: "completed", StartedAt: reviewAt, FinishedAt: reviewAt,
		},
		{
			ProjectID: "detent", IssueID: proposal.IssueID, PhaseType: WorkflowPhaseTypeLane,
			PhaseName: "Done", Reason: "merged", Status: "entered", StartedAt: completedAt,
		},
	} {
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, event); err != nil {
			t.Fatalf("RecordWorkflowPhaseEvent(%s) error = %v", event.PhaseName, err)
		}
	}
	if _, err := backend.RecordUsageEvent(ctx, UsageEvent{
		ProjectID:  "detent",
		IssueID:    proposal.IssueID,
		CostUSD:    1.25,
		StartedAt:  reworkAt,
		FinishedAt: reviewAt,
		Outcome:    "completed",
	}); err != nil {
		t.Fatalf("RecordUsageEvent() error = %v", err)
	}
	observedAt := completedAt.Add(time.Minute)
	if err := backend.RefreshAdmissionOutcomes(ctx, admissionmodel.OutcomeRefresh{
		ProjectID:      "detent",
		TerminalStates: []string{"Done"},
		ReworkState:    "Rework",
		ObservedAt:     observedAt,
	}); err != nil {
		t.Fatalf("RefreshAdmissionOutcomes() error = %v", err)
	}
	outcomes, err := backend.AdmissionDownstreamOutcomes(ctx, "detent")
	if err != nil || len(outcomes) != 1 {
		t.Fatalf("AdmissionDownstreamOutcomes() = %#v, %v", outcomes, err)
	}
	if got := outcomes[0]; !got.CompletedAt.Equal(completedAt) ||
		got.ReworkCount != 1 ||
		got.ReviewChurnCount != 1 ||
		got.SpendUSD != 1.25 ||
		!got.UpdatedAt.Equal(observedAt) {
		t.Fatalf("downstream outcome = %#v", got)
	}
}

func TestAdmissionRejectionIsDistinctFromAcceptance(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openAdmissionTestStore(t, ctx)
	createdAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	proposal := admissionTestProposal("proposal-rejected", "fingerprint-rejected", createdAt)
	if created, err := backend.CreateAdmissionProposal(ctx, proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	if err := backend.ResolveAdmissionProposal(ctx, admissionmodel.Decision{
		ProposalID: proposal.ID,
		Outcome:    admissionmodel.ProposalRejected,
		DecidedAt:  createdAt.Add(3 * time.Minute),
		CommentID:  "decision-rejected",
		ActorLogin: "grace",
		ActorKind:  "User",
	}); err != nil {
		t.Fatalf("ResolveAdmissionProposal() error = %v", err)
	}
	history, err := backend.AdmissionProposalHistory(ctx, proposal.ProjectID, proposal.IssueID)
	if err != nil || len(history) != 1 ||
		history[0].Status != admissionmodel.ProposalRejected ||
		history[0].DecisionSeconds != 180 ||
		!history[0].TransitionAt.IsZero() {
		t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
	}
	outcomes, err := backend.AdmissionDownstreamOutcomes(ctx, proposal.ProjectID)
	if err != nil || len(outcomes) != 0 {
		t.Fatalf("AdmissionDownstreamOutcomes() = %#v, %v, want none", outcomes, err)
	}
}

func openAdmissionTestStore(t *testing.T, ctx context.Context) Store {
	t.Helper()
	backend, err := Open(ctx, Config{
		Backend: BackendSQLite,
		Path:    filepath.Join(t.TempDir(), "runtime.db"),
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return backend
}

func admissionTestProposal(id string, fingerprint string, now time.Time) admissionmodel.Proposal {
	return admissionmodel.Proposal{
		ID:              id,
		ProjectID:       "detent",
		IssueID:         "issue-1",
		IssueIdentifier: "DD-1",
		IssueURL:        "https://example.test/issues/1",
		TargetState:     "Todo",
		Fingerprint:     fingerprint,
		CriteriaSection: "Admission criteria",
		CriteriaText:    "- **Alignment** — serves a priority.",
		Findings: []admissionmodel.Finding{{
			Dimension:      "Alignment",
			CriterionQuote: "serves a priority",
			Matched:        true,
			Rationale:      "Matches the release priority.",
		}},
		Confidence: 0.8,
		Status:     admissionmodel.ProposalOpen,
		CreatedAt:  now,
		ExpiresAt:  now.AddDate(0, 0, 7),
	}
}
