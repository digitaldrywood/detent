package admission

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/connector/local"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestManagerCandidateCoverageAcrossRunsAndRestart(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                     string
		local, restart, excluded bool
	}{
		{name: "memory excluded prefix", excluded: true},
		{name: "memory restart", restart: true, excluded: true},
		{name: "sqlite restart", local: true, restart: true, excluded: true},
		{name: "eligible pages", restart: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
			issues := make([]connector.Issue, 202)
			for i := range issues {
				issues[i] = admissionIssueFixture(fmt.Sprintf("issue-%03d", i), fmt.Sprintf("DD-%03d", i), 1, now.Add(time.Duration(i)*time.Minute))
				if test.excluded && i < 200 {
					issues[i].Labels = []string{"skip"}
				}
			}
			dir := t.TempDir()
			var tracker IssueStore
			if test.local {
				c, err := local.New(local.Config{Path: filepath.Join(dir, "issues.db"), ProjectID: "detent", Issues: issues})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := c.Close(); err != nil {
						t.Error(err)
					}
				})
				tracker = c
			} else {
				tracker = memory.New(memory.Config{Issues: issues, Stateful: true})
			}
			cfg := store.Config{Backend: store.BackendSQLite, Path: filepath.Join(dir, "runtime.db")}
			backend, err := store.Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := backend.Close(); err != nil {
					t.Error(err)
				}
			})
			agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
			settings := admissionTestSettings(tracker, agent)
			settings.Config.ExcludeLabels = []string{"skip"}
			settings.Config.MaxCandidatesPerRun = 100
			settings.Config.MaxProposalsPerRun = 250
			settings.Config.MaxOpenProposals = 250
			manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })
			seen := map[string]bool{}
			for run := range 3 {
				if run > 0 && test.restart {
					if err := backend.Close(); err != nil {
						t.Fatal(err)
					}
					backend, err = store.Open(t.Context(), cfg)
					if err != nil {
						t.Fatal(err)
					}
					manager = newAdmissionTestManager(t, settings, backend, func() time.Time { return now })
				}
				result, err := manager.RunOnce(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if result.CandidatesFound > 100 {
					t.Fatalf("unbounded read: %d", result.CandidatesFound)
				}
				for _, proposal := range result.Proposals {
					seen[proposal.IssueID] = true
				}
				now = now.Add(time.Hour)
			}
			want := 202
			if test.excluded {
				want = 2
			}
			if len(seen) != want || !seen["issue-201"] {
				t.Fatalf("evaluated %d issues, tail=%t; want %d including tail", len(seen), seen["issue-201"], want)
			}
			latest, found, err := backend.LatestAdmissionRun(t.Context(), "detent")
			if err != nil || !found {
				t.Fatalf("latest run: %t %v", found, err)
			}
			for _, cursor := range latest.CandidateProgress.Cursors {
				if cursor != "" {
					t.Fatalf("exhausted cursor = %q", cursor)
				}
			}
		})
	}
}

// budgetCandidateStore models a short state source consuming the entire remote
// read budget, plus a label-only eligible tail. Reopening both manager and store
// must advance selector traversal, and a partial read must still be evaluated.
type budgetCandidateStore struct {
	IssueStore
	remaining int
	partial   bool
}

func (s *budgetCandidateStore) ReadCandidates(ctx context.Context, request connector.CandidateRequest) (connector.CandidateResult, error) {
	if s.remaining == 0 {
		return connector.CandidateResult{}, &github.RESTFanoutDeferralError{BudgetScope: "candidates", FanoutCap: 1}
	}
	s.remaining--
	result, err := s.IssueStore.ReadCandidates(ctx, request)
	if err != nil {
		return result, err
	}
	if s.partial {
		result.NextCursor = "1"
		result.Truncated = true
		return result, &github.RESTFanoutDeferralError{BudgetScope: "candidates", FanoutCap: 1}
	}
	return result, nil
}

func TestManagerCandidateSelectorBudgetFairness(t *testing.T) {
	t.Parallel()
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
			prefix := admissionIssueFixture("prefix", "DD-1", 1, now)
			prefix.Labels = []string{"skip"}
			tail := admissionIssueFixture("tail", "DD-2", 1, now)
			tail.State = "Investigate"
			tail.Labels = []string{"intake"}
			tracker := &budgetCandidateStore{IssueStore: memory.New(memory.Config{Issues: []connector.Issue{prefix, tail}, Stateful: true}), partial: partial}
			backend := openManagerTestStore(t)
			agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
			settings := admissionTestSettings(tracker, agent)
			settings.Config.Sources.Labels = []string{"intake"}
			settings.Config.ExcludeLabels = []string{"skip"}
			for run := range 2 {
				tracker.remaining = 1
				manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })
				result, err := manager.RunOnce(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if run == 1 && (len(result.Proposals) != 1 || result.Proposals[0].IssueID != "tail") {
					t.Fatalf("tail not evaluated after restart: %#v", result)
				}
				now = now.Add(time.Hour)
			}
		})
	}
}

type staleHistoryBudgetStore struct {
	*budgetCandidateStore
	stale bool
}

func (s *staleHistoryBudgetStore) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]connector.Issue, error) {
	if budget, ok := connector.RESTFanoutBudgetFromContext(ctx); ok && budget.Scope() == admissionRESTFanoutScope+"_candidates" {
		return nil, &github.RESTFanoutDeferralError{BudgetScope: budget.Scope(), FanoutCap: 1}
	}
	issues, err := s.IssueStore.FetchIssueStatesByIDs(ctx, ids)
	if s.stale {
		for i := range issues {
			if issues[i].ID == "previous" {
				issues[i].Title = "Changed after candidate read"
			}
		}
	}
	return issues, err
}

func TestManagerPartialReadWithStaleHistory(t *testing.T) {
	t.Parallel()
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("still_stale=%t", stale), func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
			previous := admissionIssueFixture("previous", "DD-1", 1, now)
			fresh := admissionIssueFixture("fresh", "DD-2", 1, now)
			tracker := &staleHistoryBudgetStore{budgetCandidateStore: &budgetCandidateStore{IssueStore: memory.New(memory.Config{Issues: []connector.Issue{previous, fresh}, Stateful: true}), remaining: 1, partial: true}, stale: stale}
			backend := openManagerTestStore(t)
			agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
			settings := admissionTestSettings(tracker, agent)
			at := now.Add(-time.Hour)
			if err := backend.RecordAdmissionRun(t.Context(), admissionmodel.RunRecord{ProjectID: settings.ProjectID, ScheduledFor: at, StartedAt: at, CompletedAt: at, Issues: []admissionmodel.IssueRecord{{ID: previous.ID, Fingerprint: admissionEvaluationFingerprints(settings, previous).proposal, EvaluatedAt: at, SkipReason: "stale_or_ineligible"}}}); err != nil {
				t.Fatal(err)
			}
			manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })
			result, err := manager.RunOnce(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if stale {
				want = 1
			}
			if len(result.Proposals) != want || result.Proposals[0].IssueID != "fresh" {
				t.Fatalf("partial read discarded or stale candidate admitted: %#v", result)
			}
		})
	}
}
