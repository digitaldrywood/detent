package admission

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/runner"
)

func TestAdmissionClosedDependencyEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("dependent", "owner/repo#20", 1, now)
	issue.Description = "Fix the reported crash.\nBlocked by: #10"
	blocker := admissionIssueFixture("blocker", "owner/repo#10", 1, now)
	blocker.Closed = true
	blocker.State = "Done"
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue, blocker}, Stateful: true})
	agent := &scriptedAdmissionRunner{propose: func(request runner.RunRequest) []AgentProposal {
		raw, err := json.Marshal(request.Admission.Candidates[0])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"ready":true`) || !strings.Contains(string(raw), `"observed_at":"2026-09-04T12:00:00Z"`) {
			t.Errorf("closed dependency missing current readiness evidence: %s", raw)
		}
		return proposeEveryCandidate(request)
	}}
	manager := newAdmissionTestManager(t, admissionTestSettings(tracker, agent), openManagerTestStore(t), func() time.Time { return now })
	result, err := manager.RunOnce(t.Context())
	if err != nil || len(result.Proposals) != 1 {
		t.Fatalf("RunOnce = %+v, %v", result, err)
	}
}

func TestResolveAdmissionDependencies(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		body        string
		state       string
		closed      bool
		pr          string
		rule        string
		missing     bool
		fail        bool
		unsupported bool
		ready       bool
		wantError   bool
	}{
		{name: "closed", body: "Blocked by: #10", closed: true, ready: true},
		{name: "terminal", body: "Depends on: #10", state: "Done", ready: true},
		{name: "open", body: "Blocked by: #10", state: "Todo"},
		{name: "merged", body: "- **Depends-on:** owner/repo#10", pr: "MERGED", ready: true},
		{name: "terminal rule excludes merged", body: "Depends on: #10", pr: "merged", rule: "terminal"},
		{name: "closed unmerged PR", body: "Depends on: #10", pr: "closed"},
		{name: "URL", body: "Blocked by: https://github.com/owner/repo/issues/10", closed: true, ready: true},
		{name: "missing cross repository", body: "Blocked by: elsewhere/private#10", missing: true, wantError: true},
		{name: "resolution error", body: "Blocked by: #10", fail: true, wantError: true},
		{name: "unsupported resolver", body: "Blocked by: #10", unsupported: true, wantError: true},
		{name: "unknown state", body: "Blocked by: #10"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
			issue := connector.Issue{Identifier: "owner/repo#20", Description: tt.body}
			dependency := connector.Issue{Identifier: "owner/repo#10", State: tt.state, Closed: tt.closed}
			if tt.pr != "" {
				dependency.PullRequest = &connector.PullRequest{State: tt.pr}
			}
			tracker := &dependencyAdmissionTracker{IssueStore: memory.New(memory.Config{}), issues: []connector.Issue{dependency}}
			if tt.missing {
				tracker.issues = nil
			}
			if tt.fail {
				tracker.err = errors.New("inaccessible reference")
			}
			settings := Settings{Issues: tracker, TerminalStates: []string{"Done"}, DependencyReadiness: tt.rule}
			if tt.unsupported {
				settings.Issues = admissionIssueStoreWithoutCommentReader{IssueStore: tracker.IssueStore}
			}
			got := resolveAdmissionDependencies(t.Context(), settings, issue, now)
			if got == nil || got.Ready != tt.ready || !got.ObservedAt.Equal(now) || len(got.References) != 1 || (got.References[0].Error != "") != tt.wantError {
				t.Fatalf("evidence = %+v", got)
			}
			if got.References[0].Ready != tt.ready {
				t.Fatalf("reference = %+v", got.References[0])
			}
		})
	}
}

type dependencyAdmissionTracker struct {
	IssueStore
	issues []connector.Issue
	err    error
	calls  int
}

func (s *dependencyAdmissionTracker) FetchIssueStatesByIdentifiers(context.Context, []string) ([]connector.Issue, error) {
	s.calls++
	return s.issues, s.err
}

func (s *dependencyAdmissionTracker) FetchIssueComments(ctx context.Context, issue connector.Issue) ([]connector.IssueComment, error) {
	return s.IssueStore.(connector.IssueCommentReader).FetchIssueComments(ctx, issue)
}

func TestAdmissionDependencyChangesInvalidateResults(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"pending", "declined", "legacy pending", "legacy decline", "human prerequisite"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
			issue := admissionIssueFixture("dependent", "owner/repo#20", 1, now)
			issue.Description = "Fix the reported crash.\nBlocked by: #10\nHuman must supply deployment credentials."
			tracker := &dependencyAdmissionTracker{IssueStore: memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true}), issues: []connector.Issue{{Identifier: "owner/repo#10", State: "Todo"}}}
			backend := openManagerTestStore(t)
			agent := &scriptedAdmissionRunner{propose: func(request runner.RunRequest) []AgentProposal {
				candidate := request.Admission.Candidates[0]
				if candidate.Description != issue.Description {
					t.Fatal("human prerequisite or issue text lost")
				}
				evaluation := admissionProposalEvaluation(candidate.ID)
				if !candidate.Dependencies.Ready || mode == "human prerequisite" {
					evaluation.Findings[1].Matched = false
					evaluation.Findings[1].Rationale = "A prerequisite remains."
					if mode == "declined" {
						evaluation.Disposition = admissionDispositionDeclined
						evaluation.Findings[0].Matched = false
					}
				}
				return []AgentProposal{evaluation}
			}}
			settings := admissionTestSettings(tracker, agent)
			manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })
			if mode == "legacy pending" {
				_, err := backend.CreateAdmissionProposal(t.Context(), admissionTestProposalForIssue("legacy", issue, now))
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "legacy decline" {
				_, _, err := manager.createAdmissionDecline(t.Context(), settings, issue, admissionDeclineClassification{reason: admissionDeclineCriteriaNotMet, detail: "Blocked by #10", confidence: float64Pointer(0.2), failedDimension: "Readiness", failedCriterion: "has an actionable problem statement"}, now)
				if err != nil {
					t.Fatal(err)
				}
			}
			first, err := manager.RunOnce(t.Context())
			if err != nil || agent.calls != 1 {
				t.Fatalf("first = %+v, %v; calls=%d", first, err, agent.calls)
			}
			now = now.Add(time.Hour)
			_, err = manager.RunOnce(t.Context())
			if err != nil || agent.calls != 1 {
				t.Fatalf("unchanged = %v; calls=%d", err, agent.calls)
			}
			tracker.issues[0].Closed = true
			now = now.Add(time.Hour)
			result, err := manager.RunOnce(t.Context())
			if err != nil || agent.calls != 2 || len(result.Proposals) != 1 {
				t.Fatalf("closed = %+v, %v; calls=%d", result, err, agent.calls)
			}
			proposal := result.Proposals[0]
			if proposal.Findings[1].Matched != (mode != "human prerequisite") {
				t.Fatalf("readiness = %+v", proposal.Findings[1])
			}
			if !strings.Contains(proposal.Findings[1].Rationale, "observed at 2026-09-04T14:00:00Z") {
				t.Fatal("missing persisted timestamp")
			}
			history, err := backend.AdmissionProposalHistory(t.Context(), settings.ProjectID, issue.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, old := range history {
				if old.ID != proposal.ID && old.Status != admissionmodel.ProposalSuperseded {
					t.Fatalf("stale proposal = %+v", old)
				}
			}
			comments, err := tracker.FetchIssueComments(t.Context(), issue)
			if err != nil || !strings.Contains(comments[len(comments)-1].Body, "historical evaluation evidence") {
				t.Fatalf("comments=%+v, %v", comments, err)
			}
		})
	}
}

func TestAdmissionRevalidatesDependenciesDuringEvaluation(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	issue := admissionIssueFixture("dependent", "owner/repo#20", 1, now)
	issue.Description += "\nDepends on: #10"
	tracker := &dependencyAdmissionTracker{IssueStore: memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true}), issues: []connector.Issue{{Identifier: "owner/repo#10", Closed: true}}}
	agent := &scriptedAdmissionRunner{propose: func(request runner.RunRequest) []AgentProposal {
		tracker.issues[0].Closed = false
		return proposeEveryCandidate(request)
	}}
	manager := newAdmissionTestManager(t, admissionTestSettings(tracker, agent), openManagerTestStore(t), func() time.Time { return now })
	result, err := manager.RunOnce(t.Context())
	if err != nil || len(result.Proposals) != 0 || result.Skipped["stale_or_ineligible"] != 1 {
		t.Fatalf("result=%+v, %v", result, err)
	}
}

func FuzzAdmissionDependencyReadiness(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4})
	f.Add([]byte{1, 1})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, states []byte) {
		if len(states) > admissionDependencyLimit {
			states = states[:admissionDependencyLimit]
		}
		issue := connector.Issue{Identifier: "owner/repo#1000"}
		tracker := &dependencyAdmissionTracker{}
		wantReady := true
		for index, state := range states {
			ref := "owner/repo#" + strconv.Itoa(index+1)
			issue.Description += "\nBlocked by: " + ref
			dependency := connector.Issue{Identifier: ref}
			switch state % 5 {
			case 0:
				dependency.State = "Todo"
				wantReady = false
			case 1:
				dependency.Closed = true
			case 2:
				wantReady = false
				continue
			case 3:
				dependency.PullRequest = &connector.PullRequest{State: "merged"}
			case 4:
				dependency.State = "Done"
			}
			tracker.issues = append(tracker.issues, dependency)
		}
		settings := Settings{Issues: tracker, TerminalStates: []string{"Done"}}
		now := time.Unix(100, 0).UTC()
		evidence := resolveAdmissionDependencies(t.Context(), settings, issue, now)
		if len(states) == 0 {
			if evidence != nil {
				t.Fatal("unexpected evidence")
			}
			return
		}
		if evidence.Ready != wantReady {
			t.Fatalf("ready=%t want=%t states=%v", evidence.Ready, wantReady, states)
		}
		fingerprint := issueFingerprint(issue, evidence)
		evidence.ObservedAt = now.Add(time.Hour)
		if issueFingerprint(issue, evidence) != fingerprint {
			t.Fatal("freshness alone invalidated fingerprint")
		}
		evidence.References[0].Ready = !evidence.References[0].Ready
		if issueFingerprint(issue, evidence) == fingerprint {
			t.Fatal("dependency change did not invalidate fingerprint")
		}
	})
}

func TestAdmissionDependencyAcceptanceFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		closed      bool
		unavailable bool
		change      bool
		automatic   bool
		wantState   string
	}{
		{name: "explicit open", wantState: "Backlog"},
		{name: "explicit unavailable", unavailable: true, wantState: "Backlog"},
		{name: "explicit closed", closed: true, wantState: "Todo"},
		{name: "changed after proposal", closed: true, change: true, wantState: "Backlog"},
		{name: "automatic open", automatic: true, wantState: "Backlog"},
		{name: "automatic closed", closed: true, automatic: true, wantState: "Todo"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
			issue := admissionIssueFixture("dependent", "owner/repo#20", 1, now)
			issue.Description += "\nBlocked by: #10"
			tracker := &dependencyAdmissionTracker{IssueStore: memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true}), issues: []connector.Issue{{Identifier: "owner/repo#10", Closed: tt.closed}}}
			if tt.unavailable {
				tracker.issues = nil
			}
			backend := openManagerTestStore(t)
			settings := admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate})
			settings.Config.AutoAdmit = tt.automatic
			settings.Config.AutoAdmitMinConfidence = 0.5
			manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })
			proposal := admissionTestProposalForIssue("proposal", issue, now)
			proposal.Fingerprint = issueFingerprint(issue, resolveAdmissionDependencies(t.Context(), settings, issue, now))
			if _, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil {
				t.Fatal(err)
			}
			if tt.change {
				tracker.issues[0].Closed = false
			}
			decision := admissionmodel.Decision{ProposalID: proposal.ID, Outcome: admissionmodel.ProposalAccepted, DecidedAt: now, CommentID: "decision", ActorLogin: "operator", Automatic: tt.automatic}
			if err := manager.admitProposal(t.Context(), settings, issue, proposal, decision); err != nil {
				t.Fatal(err)
			}
			fresh, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
			if err != nil || len(fresh) != 1 || fresh[0].State != tt.wantState {
				t.Fatalf("issues=%+v, %v", fresh, err)
			}
		})
	}
}

func TestAdmissionDependencyResolutionBounds(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	issue := connector.Issue{ID: "dependent", Identifier: "owner/repo#1000"}
	tracker := &dependencyAdmissionTracker{}
	for index := range admissionDependencyLimit + 1 {
		ref := "owner/repo#" + strconv.Itoa(index+1)
		issue.BlockedBy = append(issue.BlockedBy, connector.BlockedRef{Identifier: ref})
		tracker.issues = append(tracker.issues, connector.Issue{Identifier: ref, Closed: true})
	}
	settings := Settings{Issues: tracker}
	evidence := resolveAdmissionDependencies(t.Context(), settings, issue, now)
	if evidence == nil {
		t.Fatal("missing bounded dependency evidence")
	}
	if evidence.Ready || len(evidence.References) != admissionDependencyLimit+1 || !strings.Contains(evidence.References[0].Error, "limit exceeded") || tracker.calls != 1 {
		t.Fatalf("evidence=%+v; calls=%d", evidence, tracker.calls)
	}
	if !strings.Contains(admissionDependencySummary(evidence), "limit exceeded") {
		t.Fatal("resolution error absent from explanation")
	}
	tracker.IssueStore = memory.New(memory.Config{})
	settings = admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate})
	manager := newAdmissionTestManager(t, settings, openManagerTestStore(t), func() time.Time { return now })
	candidates, _, truncated, err := manager.unproposedCandidates(t.Context(), settings, []connector.Issue{issue}, map[string]int{}, 1, now, 0, nil)
	if err != nil || len(candidates) != 0 || truncated != 1 || tracker.calls != 2 {
		t.Fatalf("candidates=%+v, truncated=%d, err=%v, calls=%d", candidates, truncated, err, tracker.calls)
	}
}

func TestAdmissionDependencyParserPreservesNativeAndProse(t *testing.T) {
	t.Parallel()
	issue := connector.Issue{Identifier: "owner/repo#20", BlockedBy: []connector.BlockedRef{{Identifier: "OWNER/REPO#10"}, {Identifier: "owner/repo#20"}}, Description: "Blocked by: #10, #11\nDepends-on: external/repo#12\nHistorical mention of #13"}
	want := []string{"external/repo#12", "owner/repo#10", "owner/repo#11"}
	if got := admissionDependencyReferences(issue); !reflect.DeepEqual(got, want) {
		t.Fatalf("refs=%v want=%v", got, want)
	}
}

func TestAdmissionUnchangedDependencyDeclineDoesNotStarveNextCandidate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	first := admissionIssueFixture("first", "owner/repo#20", 1, now)
	second := admissionIssueFixture("second", "owner/repo#21", 1, now.Add(time.Minute))
	first.Description += "\nBlocked by: #10"
	second.Description += "\nBlocked by: #10"
	tracker := &dependencyAdmissionTracker{IssueStore: memory.New(memory.Config{Issues: []connector.Issue{first, second}, Stateful: true}), issues: []connector.Issue{{Identifier: "owner/repo#10", Closed: true}}}
	agent := &scriptedAdmissionRunner{propose: func(request runner.RunRequest) []AgentProposal {
		evaluation := admissionProposalEvaluation(request.Admission.Candidates[0].ID)
		if evaluation.IssueID == first.ID {
			evaluation.Disposition = admissionDispositionDeclined
			for index := range evaluation.Findings {
				evaluation.Findings[index].Matched = false
			}
		}
		return []AgentProposal{evaluation}
	}}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.MaxCandidatesPerRun = 1
	manager := newAdmissionTestManager(t, settings, openManagerTestStore(t), func() time.Time { return now })
	if _, err := manager.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	result, err := manager.RunOnce(t.Context())
	if err != nil || len(result.Proposals) != 1 || result.Proposals[0].IssueID != second.ID {
		t.Fatalf("result=%+v, %v", result, err)
	}
}
