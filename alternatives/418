package admission

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/connector/local"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestManagerCandidateCoverageAcrossRunsAndRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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

func TestManagerAdmissionUsesAcceptedDependencyFrontier(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, tt := range []struct {
		name, source, prerequisite, snapshotState                                       string
		enabled, human, unknown, bodyOnly, shared, localAlias, extraMissing, extraError bool
		wantReady                                                                       bool
		wantFirst                                                                       string
	}{
		{name: "active frontier", source: "Rework", enabled: true, wantFirst: "frontier"},
		{name: "blocked frontier", source: "Blocked", enabled: true, wantFirst: "frontier"},
		{name: "disabled ranking", source: "Rework", wantFirst: "older"},
		{name: "unaccepted backlog dependent", source: "Backlog", enabled: true, wantFirst: "older"},
		{name: "human dependent", source: "Blocked", enabled: true, human: true, wantFirst: "older"},
		{name: "unavailable owner", source: "Rework", enabled: true, unknown: true, wantFirst: "older"},
		{name: "fresh closed prerequisite", source: "Todo", enabled: true, prerequisite: "closed", wantReady: true, wantFirst: "frontier"},
		{name: "fresh merged prerequisite", source: "Todo", enabled: true, prerequisite: "merged", wantReady: true, wantFirst: "frontier"},
		{name: "fresh open prerequisite", source: "Todo", enabled: true, prerequisite: "open", snapshotState: "Done", wantFirst: "older"},
		{name: "missing prerequisite", source: "Todo", enabled: true, prerequisite: "missing", snapshotState: "Done", wantFirst: "older"},
		{name: "failed prerequisite read", source: "Todo", enabled: true, prerequisite: "error", snapshotState: "Done", wantFirst: "older"},
		{name: "closed unverified human prerequisite", source: "Todo", enabled: true, prerequisite: "human", snapshotState: "Done", wantFirst: "older"},
		{name: "body-only closed prerequisite", source: "Todo", enabled: true, prerequisite: "closed", bodyOnly: true, wantReady: true, wantFirst: "frontier"},
		{name: "body-only open prerequisite", source: "Todo", enabled: true, prerequisite: "open", bodyOnly: true, wantFirst: "older"},
		{name: "shared closed prerequisite", source: "Todo", enabled: true, prerequisite: "closed", shared: true, wantReady: true, wantFirst: "frontier"},
		{name: "shared merged prerequisite", source: "Todo", enabled: true, prerequisite: "merged", shared: true, bodyOnly: true, wantReady: true, wantFirst: "frontier"},
		{name: "shared open prerequisite", source: "Todo", enabled: true, prerequisite: "open", shared: true, snapshotState: "Done", wantFirst: "older"},
		{name: "shared missing prerequisite", source: "Todo", enabled: true, prerequisite: "missing", shared: true, wantFirst: "older"},
		{name: "shared failed prerequisite read", source: "Todo", enabled: true, prerequisite: "error", shared: true, wantFirst: "older"},
		{name: "shared unverified human prerequisite", source: "Todo", enabled: true, prerequisite: "human", shared: true, wantFirst: "older"},
		{name: "shared prerequisite with missing peer prerequisite", source: "Todo", enabled: true, prerequisite: "closed", shared: true, extraMissing: true, wantReady: true, wantFirst: "frontier"},
		{name: "shared prerequisite with inaccessible peer prerequisite", source: "Todo", enabled: true, prerequisite: "closed", shared: true, extraError: true, wantReady: true, wantFirst: "frontier"},
		{name: "shared local alias", source: "Todo", enabled: true, prerequisite: "closed", shared: true, bodyOnly: true, localAlias: true, wantReady: true, wantFirst: "frontier"},
		{name: "ambiguous shared local alias", source: "Todo", enabled: true, prerequisite: "ambiguous", shared: true, bodyOnly: true, localAlias: true, wantFirst: "older"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 30, 22, 30, 0, 0, time.UTC)
			older := admissionIssueFixture("older", "owner/repo#1", 0, now.Add(-time.Hour))
			frontier := admissionIssueFixture("frontier", "owner/repo#3242", 0, now)
			prerequisite := admissionIssueFixture("prerequisite", "owner/repo#2", 0, now)
			prerequisite.State = "Todo"
			prerequisite.Number = 2
			if tt.localAlias {
				frontier.Identifier = "local-frontier"
			}
			switch tt.prerequisite {
			case "closed", "human":
				prerequisite.Closed = true
				prerequisite.State = "Done"
			case "merged":
				prerequisite.PullRequest = &connector.PullRequest{State: "merged"}
			}
			if tt.prerequisite == "human" {
				prerequisite.Labels = []string{"human-owned"}
			}
			if tt.prerequisite != "" {
				if tt.bodyOnly {
					frontier.Description += "\nDepends on: #2"
				} else {
					state := tt.snapshotState
					if state == "" {
						state = "Backlog"
					}
					frontier.BlockedBy = []connector.BlockedRef{{ID: prerequisite.ID, Identifier: prerequisite.Identifier, State: state, Source: connector.BlockedRefSourceNative}}
				}
			}
			dependent := admissionIssueFixture("dependent", "owner/repo#3187", 0, now)
			dependent.State = tt.source
			dependent.BlockedBy = []connector.BlockedRef{{ID: frontier.ID, Identifier: frontier.Identifier, State: "Backlog"}}
			if tt.human {
				dependent.Labels = []string{"human-owned"}
			}
			cohort := []connector.Issue{dependent, dependent}
			tracker := memory.New(memory.Config{Issues: []connector.Issue{older, frontier, prerequisite}, Stateful: true})
			settings := admissionTestSettings(tracker, &scriptedAdmissionRunner{})
			resolver := &dependencyAdmissionTracker{IssueStore: tracker, issues: []connector.Issue{prerequisite}}
			if tt.prerequisite == "ambiguous" {
				other := prerequisite
				other.Identifier = "elsewhere/repo#2"
				resolver.issues = append(resolver.issues, other)
			}
			if tt.prerequisite == "missing" {
				resolver.issues = nil
			}
			if tt.prerequisite == "error" {
				resolver.err = errors.New("inaccessible prerequisite")
			}
			if tt.extraError {
				resolver.resolve = func(refs []string) ([]connector.Issue, error) {
					if reflect.DeepEqual(refs, []string{"owner/repo#3"}) {
						return []connector.Issue{prerequisite}, errors.New("inaccessible prerequisite")
					}
					return resolver.issues, nil
				}
			}
			settings.Issues = resolver
			settings.dependencies = make(map[string]*runner.AdmissionDependencies)
			settings.DispatchStates = []string{"Merging", "Rework", "In Progress", "Todo"}
			settings.TerminalStates = []string{"Done"}
			settings.PrioritizeBlockers = tt.enabled
			calls := 0
			settings.DependencyIssues = func(context.Context) []connector.Issue {
				calls++
				if tt.unknown {
					return nil
				}
				return cohort
			}
			backend := openManagerTestStore(t)
			manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })
			candidates := []connector.Issue{older, frontier}
			peer := frontier
			peer.ID = "peer"
			peer.Identifier = "owner/repo#3243"
			if tt.extraMissing || tt.extraError {
				peer.Description += "\nDepends on: owner/repo#3"
			}
			if tt.localAlias {
				peer.Identifier = "local-peer"
			}
			if tt.shared {
				candidates = append(candidates, peer)
			}
			got, err := manager.orderCandidateWindow(t.Context(), settings, candidates, map[string]int{}, now)
			if err != nil || len(got) != len(candidates) || got[0].ID != tt.wantFirst {
				t.Fatalf("order=%+v err=%v", got, err)
			}
			wantCalls := 0
			if tt.enabled {
				wantCalls = 1
			}
			if calls != wantCalls || cohort[0].ID != dependent.ID || cohort[1].ID != dependent.ID {
				t.Fatalf("callback=%d cohort=%+v", calls, cohort)
			}
			if got[0].ID == "frontier" && got[0].UnblockerCount != 1 {
				t.Fatalf("duplicate-dependent count=%d", got[0].UnblockerCount)
			}
			wantReads := 0
			if tt.prerequisite != "" {
				wantReads = 1
				if evidence := settings.dependencies[frontier.ID]; evidence == nil || evidence.Ready != tt.wantReady {
					t.Fatalf("fresh dependency evidence=%+v", evidence)
				}
			}
			if tt.shared && (tt.prerequisite == "missing" || tt.prerequisite == "error" || tt.localAlias || tt.extraMissing || tt.extraError) {
				wantReads = 2
			}
			if resolver.calls != wantReads {
				t.Fatalf("dependency reads=%d want=%d", resolver.calls, wantReads)
			}
			if wantReads > 0 {
				ref := "owner/repo#2"
				if tt.localAlias {
					ref = "#2"
				}
				for i, request := range resolver.requests {
					if i == 1 && (tt.extraMissing || tt.extraError) {
						ref = "owner/repo#3"
					}
					if !reflect.DeepEqual(request, []string{ref}) {
						t.Fatalf("dependency read=%+v", request)
					}
				}
			}
			if tt.shared {
				frontierEvidence := settings.dependencies[frontier.ID]
				peerEvidence := settings.dependencies[peer.ID]
				baseline := frontierEvidence
				if tt.extraMissing || tt.extraError {
					expected := *frontierEvidence
					expected.Ready = false
					entry := runner.AdmissionDependency{Identifier: "owner/repo#3", Error: "dependency was not returned by tracker"}
					if tt.extraError {
						entry.Error = "dependency resolution failed: inaccessible prerequisite"
					}
					expected.References = append(append([]runner.AdmissionDependency(nil), frontierEvidence.References...), entry)
					baseline = &expected
				}
				if !reflect.DeepEqual(baseline, peerEvidence) || !peerEvidence.ObservedAt.Equal(now) || dependencyFingerprint("candidate", baseline) != dependencyFingerprint("candidate", peerEvidence) {
					t.Fatalf("shared evidence changed: frontier=%+v peer=%+v", frontierEvidence, peerEvidence)
				}
				if !tt.extraError {
					baseline = resolveAdmissionDependencies(t.Context(), settings, peer, now)
					if !reflect.DeepEqual(peerEvidence, baseline) || issueFingerprint(peer, peerEvidence) != issueFingerprint(peer, baseline) {
						t.Fatalf("shared resolution changed candidate fingerprint: %+v", peerEvidence)
					}
				}
			}
			for _, candidate := range got {
				if candidate.ID == frontier.ID {
					expected := frontier
					expected.UnblockerCount = candidate.UnblockerCount
					if !reflect.DeepEqual(candidate, expected) || issueFingerprint(candidate, settings.dependencies[frontier.ID]) != issueFingerprint(frontier, settings.dependencies[frontier.ID]) {
						t.Fatalf("ranking changed candidate authority or fingerprint: %+v", candidate)
					}
				}
			}
		})
	}
}
