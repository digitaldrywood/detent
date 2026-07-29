package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
)

func TestAdmissionProposalLifecycleAndIdempotency(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openAdmissionTestStore(t, ctx)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	first := admissionTestProposal("proposal-1", "fingerprint-1", now)
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
	if len(history) != 2 || history[0].Status != admissionmodel.ProposalAccepted || history[1].Status != admissionmodel.ProposalSuperseded {
		t.Fatalf("history = %#v", history)
	}
	count, err := backend.CountOpenAdmissionProposals(ctx, "detent")
	if err != nil || count != 0 {
		t.Fatalf("CountOpenAdmissionProposals() = %d, %v, want 0", count, err)
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
		got.Skipped["author"] != 1 || got.Truncated["candidates"] != 1 ||
		len(got.Issues) != 1 || got.Issues[0].ProposalID != "proposal-expiring" {
		t.Fatalf("LatestAdmissionRun() = %#v", got)
	}
	runs, err := backend.RecentAdmissionRuns(ctx, "detent", 3)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentAdmissionRuns() = %#v, %v", runs, err)
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
