package admission

import (
	"context"
	"fmt"
	"testing"
	"time"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/runner"
)

func TestManagerCandidateWindowOrdersByPersistedEvaluation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		changed bool
		want    []string
	}{
		{name: "unseen then oldest evaluation", want: []string{"unseen", "old", "recent"}},
		{name: "changed candidate becomes unseen", changed: true, want: []string{"recent", "unseen", "old"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
			issues := []connector.Issue{
				admissionIssueFixture("recent", "DD-1", 1, now),
				admissionIssueFixture("old", "DD-2", 1, now),
				admissionIssueFixture("unseen", "DD-3", 1, now),
			}
			tracker := memory.New(memory.Config{Issues: issues, Stateful: true})
			agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
			settings := admissionTestSettings(tracker, agent)
			backend := openManagerTestStore(t)
			for i, evaluatedAt := range []time.Time{now.Add(-time.Minute), now.Add(-time.Hour)} {
				record := admissionmodel.RunRecord{
					ProjectID: "detent", ScheduledFor: evaluatedAt, StartedAt: evaluatedAt, CompletedAt: evaluatedAt,
					Issues: []admissionmodel.IssueRecord{{ID: issues[i].ID, Fingerprint: admissionEvaluationFingerprints(settings, issues[i]).proposal, EvaluatedAt: evaluatedAt}},
				}
				if err := backend.RecordAdmissionRun(t.Context(), record); err != nil {
					t.Fatal(err)
				}
			}
			if test.changed {
				if err := tracker.UpdateIssueBody(t.Context(), "recent", "Revised requirements"); err != nil {
					t.Fatal(err)
				}
			}
			manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })
			result, err := manager.RunOnce(t.Context())
			if err != nil || len(result.Proposals) != 3 {
				t.Fatalf("RunOnce() = %#v, %v", result, err)
			}
			for i, want := range test.want {
				if agent.candidateIDs[i][0] != want {
					t.Fatalf("evaluation order = %v, want %v", agent.candidateIDs, test.want)
				}
			}
		})
	}
}

func TestManagerCandidateWindowCoverage(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		count   int
		blocked bool
	}{
		{name: "new malformed results", count: 21},
		{name: "standing malformed blocks", count: 21, blocked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
			issues := make([]connector.Issue, test.count)
			for i := range issues {
				issues[i] = admissionIssueFixture(fmt.Sprintf("issue-%02d", i), fmt.Sprintf("DD-%02d", i), 1, now.Add(time.Duration(i)*time.Minute))
			}
			issues[2].Title = "Epic: track the rollout"
			tracker := &staleWindowIssueStore{Connector: memory.New(memory.Config{Issues: issues, Stateful: true})}
			backend := openManagerTestStore(t)
			agent := &windowAdmissionRunner{seen: map[string]int{}}
			settings := admissionTestSettings(tracker, agent)
			settings.Config.MaxCandidatesPerRun = 3
			manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })
			if test.blocked {
				for _, issue := range issues[:2] {
					for range malformedAdmissionAttemptLimit {
						if _, err := manager.recordMalformedResult(t.Context(), settings, issue, malformedEvaluation{errorClass: "parse", errorCode: "invalid_json", output: []byte("{")}, now); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			for run := range (test.count + 2) / 3 {
				// Recreate the manager to verify coverage survives worker restart.
				manager = newAdmissionTestManager(t, settings, backend, func() time.Time { return now })
				result, err := manager.RunOnce(t.Context())
				if err != nil || result.Candidates > 3 {
					t.Fatalf("run %d = %#v, %v", run, result, err)
				}
				record, found, err := backend.LatestAdmissionRun(t.Context(), "detent")
				if err != nil || !found || len(record.Issues) != result.Candidates {
					t.Fatalf("run %d evidence = %#v, %t, %v", run, record, found, err)
				}
				for _, evaluated := range record.Issues {
					if evaluated.Fingerprint == "" || evaluated.Identifier == "" || !evaluated.EvaluatedAt.Equal(now) || agent.seen[evaluated.ID] == 0 {
						t.Fatalf("missing evaluation identity: %#v", evaluated)
					}
					if evaluated.ID != "issue-00" && evaluated.ID != "issue-01" && evaluated.SkipReason != "stale_or_ineligible" {
						t.Fatalf("missing stale verdict: %#v", evaluated)
					}
				}
				now = now.Add(15 * time.Minute)
			}
			for _, issue := range issues[3:] {
				if agent.seen[issue.ID] != 1 {
					t.Errorf("%s evaluated %d times, want once within the bounded runs", issue.ID, agent.seen[issue.ID])
				}
			}
			if agent.seen[issues[2].ID] != 0 {
				t.Fatal("tracking epic reached the agent")
			}
			if test.blocked && (agent.seen[issues[0].ID] != 0 || agent.seen[issues[1].ID] != 0) {
				t.Fatal("unchanged malformed block reached the agent")
			}
			// Changed snapshots re-enter the window for both malformed and stale results.
			for _, index := range []int{0, 3} {
				issue := issues[index]
				before := agent.seen[issue.ID]
				if err := tracker.UpdateIssueBody(t.Context(), issue.ID, issue.Description+" revised"); err != nil {
					t.Fatal(err)
				}
				if _, err := manager.RunOnce(t.Context()); err != nil {
					t.Fatal(err)
				}
				if agent.seen[issue.ID] != before+1 {
					t.Fatalf("changed %s was not reevaluated", issue.ID)
				}
				now = now.Add(15 * time.Minute)
			}
		})
	}
}

func TestManagerCandidateWindowReconsidersRestoredSnapshot(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func([]connector.Issue) []connector.Issue
	}{
		{name: "leaves source state", change: func(issues []connector.Issue) []connector.Issue {
			issues[0].State = "In Progress"
			return issues
		}},
		{name: "closed", change: func(issues []connector.Issue) []connector.Issue {
			issues[0].Closed = true
			return issues
		}},
		{name: "missing", change: func([]connector.Issue) []connector.Issue { return nil }},
		{name: "content reverted", change: func(issues []connector.Issue) []connector.Issue {
			issues[0].Title += " revised"
			return issues
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
			issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
			tracker := &restoredWindowIssueStore{
				Connector: memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true}),
				change:    test.change,
			}
			backend := openManagerTestStore(t)
			agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
			settings := admissionTestSettings(tracker, agent)
			for run := range 3 {
				if run == 2 {
					tracker.change = nil // Return to the exact original eligible snapshot.
				}
				manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })
				result, err := manager.RunOnce(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if run < 2 {
					if result.Skipped["stale_or_ineligible"] != 1 || len(agent.candidateIDs) != 1 {
						t.Fatalf("run %d: result = %#v, evaluations = %v", run, result, agent.candidateIDs)
					}
				} else if len(result.Proposals) != 1 || len(agent.candidateIDs) != 2 {
					t.Fatalf("restored candidate never reconsidered: result = %#v, evaluations = %v", result, agent.candidateIDs)
				}
				now = now.Add(15 * time.Minute)
			}
		})
	}
}

type restoredWindowIssueStore struct {
	*memory.Connector
	change func([]connector.Issue) []connector.Issue
}

func (s *restoredWindowIssueStore) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]connector.Issue, error) {
	issues, err := s.Connector.FetchIssueStatesByIDs(ctx, ids)
	if err == nil && len(issues) > 0 && s.change != nil {
		issues = s.change(issues)
	}
	return issues, err
}

// The candidate reader retains an old snapshot while the point lookup returns
// newer issue content, reproducing a repeated stale revalidation verdict.
type staleWindowIssueStore struct{ *memory.Connector }

func (s *staleWindowIssueStore) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]connector.Issue, error) {
	issues, err := s.Connector.FetchIssueStatesByIDs(ctx, ids)
	for i := range issues {
		issues[i].Title += " (updated)"
	}
	return issues, err
}

type windowAdmissionRunner struct{ seen map[string]int }

func (r *windowAdmissionRunner) Run(ctx context.Context, request runner.RunRequest) (runner.RunResult, error) {
	id := request.Admission.Candidates[0].ID
	r.seen[id]++
	if id == "issue-00" || id == "issue-01" {
		return runner.RunResult{FinalState: runner.FinalStateCompleted, Output: "{"}, nil
	}
	agent := scriptedAdmissionRunner{propose: proposeEveryCandidate}
	return agent.Run(ctx, request)
}
