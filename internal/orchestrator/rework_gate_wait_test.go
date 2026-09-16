package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestReworkGateWaitRejectsActionableCompletion(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		status       string
		mergeability string
		humanAction  string
	}{
		{name: "recorded auth blocker and dirty PR", status: workpad.StatusBlocked, mergeability: "dirty", humanAction: "Restore worker-github-cli-auth under the configured credential policy."},
		{name: "dirty PR with completed Workpad", status: workpad.StatusComplete, mergeability: "dirty"},
		{name: "clean PR with incomplete Workpad", status: workpad.StatusInProgress, mergeability: "clean"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := implementProgressIssue("same-head")
			issue.State = "Rework"
			issue.PullRequest.MergeableState = tt.mergeability
			issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: tt.status, HumanAction: tt.humanAction}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Rework"}, AutoPromote: AutoPromoteConfig{Enabled: true, GateWaitState: autoPromoteGateWaitSource, Gate: gate.Config{Kind: gate.KindCommand}}})
			decision := implementCompletionProgressDecision{Issue: issue, Outcome: store.WorkAttemptTerminalNoProgress, WorkpadStatus: tt.status, Reason: "unchanged_signature_clean_diff", CurrentSignature: autoPromoteReworkSignature{PRNumber: 1070, HeadSHA: "same-head"}, WorkspaceDiffStats: DiffStats{Status: "clean"}}
			got, reason := completedReworkGateWaitProgress(Running{Issue: issue}, decision, cfg, FinalStateCompleted)
			if reason != "" || got.Outcome == store.WorkAttemptTerminalSuccess {
				t.Fatalf("actionable completion became successful wait: outcome=%s reason=%s", got.Outcome, reason)
			}
		})
	}
}

func TestReworkGateWaitHistoryCannotResurrectSupersededWait(t *testing.T) {
	t.Parallel()
	issue := reworkGateWaitTestIssue(workpad.StatusComplete)
	now := time.Date(2026, 9, 5, 19, 18, 8, 0, time.UTC)
	valid := successfulReworkGateWaitAttempt(now, issue, autoPromoteReworkSignature{PRNumber: int64(issue.PullRequest.Number), HeadSHA: issue.PullRequest.HeadSHA}, true)
	failed := valid
	failed.TerminalState = store.WorkAttemptTerminalNoProgress
	for _, tt := range []struct {
		name    string
		history []store.WorkAttempt
		err     error
		want    bool
	}{
		{name: "newer no progress supersedes wait", history: []store.WorkAttempt{failed, valid}},
		{name: "ignore unrelated failed plan", history: []store.WorkAttempt{{TerminalState: store.WorkAttemptTerminalFailure}, valid}, want: true},
		{name: "history unavailable", err: errors.New("history unavailable")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			orch := &Orchestrator{cfg: normalizeConfig(Config{ActiveStates: []string{"Rework"}, AutoPromote: AutoPromoteConfig{Enabled: true, GateWaitState: autoPromoteGateWaitSource, Gate: gate.Config{Kind: gate.KindCommand}}}), connector: &implementProgressConnector{refreshed: issue}, workAttempts: &recordingWorkAttemptStore{history: tt.history, historyErr: tt.err}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			_, ok, err := orch.latestSuccessfulGateWaitAttempt(t.Context(), issue)
			if ok != tt.want || !errors.Is(err, tt.err) {
				t.Fatalf("found=%t err=%v, want=%t/%v", ok, err, tt.want, tt.err)
			}
			state := State{}
			orch.restoreDurableGateWaitCompletions(t.Context(), &state, []connector.Issue{{}, issue})
			if _, restored := state.Completed[issue.ID]; restored != tt.want {
				t.Fatalf("restored=%t, want=%t", restored, tt.want)
			}
		})
	}
}

func TestReworkGateWaitRejectsMismatchedDurableEvidence(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		mutate func(*connector.Issue, *store.WorkAttempt)
	}{
		{name: "missing PR", mutate: func(i *connector.Issue, _ *store.WorkAttempt) { i.PullRequest = nil }},
		{name: "missing PR number", mutate: func(i *connector.Issue, _ *store.WorkAttempt) {
			i.PullRequest.Number = 0
			i.PRNumber = nil
			i.PullRequest.URL = ""
		}},
		{name: "different attempt PR", mutate: func(_ *connector.Issue, a *store.WorkAttempt) { a.PRNumber = new(int64(999)) }},
		{name: "different signature PR", mutate: func(i *connector.Issue, a *store.WorkAttempt) {
			a.PRNumber = nil
			i.PullRequest.Number = 999
			i.PRNumber = new(999)
		}},
		{name: "malformed wait metadata", mutate: func(_ *connector.Issue, a *store.WorkAttempt) { a.WorkerMetadataJSON = "{" }},
		{name: "unavailable PR hydration", mutate: func(i *connector.Issue, _ *store.WorkAttempt) {
			i.PullRequest.HydrationUnavailableReason = "unavailable"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := reworkGateWaitTestIssue(workpad.StatusComplete)
			attempt := successfulReworkGateWaitAttempt(time.Now(), issue, autoPromoteReworkSignature{PRNumber: int64(issue.PullRequest.Number), HeadSHA: issue.PullRequest.HeadSHA}, true)
			tt.mutate(&issue, &attempt)
			if gateWaitAttemptMatchesPullRequest(attempt, issue, "Rework") {
				t.Fatal("mismatched durable completion was restored")
			}
			if tt.name == "malformed wait metadata" {
				if _, ok := completionGateWaitRecordFromAttempt(attempt); ok {
					t.Fatal("malformed metadata accepted")
				}
			}
		})
	}
}

func TestGateWaitHelpersTolerateMissingState(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	var orch *Orchestrator
	orch.restoreDurableGateWaitCompletions(ctx, nil, nil)
	if attempts, err := orch.recentAgentTerminalAttempts(ctx, connector.Issue{}); err != nil || len(attempts) != 0 {
		t.Fatalf("attempts=%v err=%v", attempts, err)
	}
	if autoPromoteCompletedFinalState(nil, "issue") != "" || autoPromoteOperationalCompletionAccepted(nil, "issue") || autoPromoteReviewWaitExpired(nil, "issue", AutoPromoteConfig{}, time.Now()) || autoPromoteIssueCompleted(nil, "issue") {
		t.Fatal("absent state supplied completion evidence")
	}
	if autoPromoteActiveGatePendingIssue(connector.Issue{}, nil, Config{}, AutoPromoteConfig{}) {
		t.Fatal("absent state supplied gate wait")
	}
	recordAutoPromoteSnapshotDecision(nil, "issue", AutoPromoteDecision{})
	state := newState(normalizeConfig(Config{}))
	recordAutoPromoteSnapshotDecision(&state, "", AutoPromoteDecision{})
	state.AutoPromoteDecisions["issue"] = autoPromoteDecision(AutoPromoteActionSkip, AutoPromoteReasonCINotGreen)
	recordAutoPromoteSnapshotDecision(&state, "issue", autoPromoteDecision(AutoPromoteActionPromote, AutoPromoteReasonReady))
	if len(state.AutoPromoteDecisions) != 0 {
		t.Fatal("superseded wait remains visible")
	}
}

func reworkGateWaitTestIssue(status string) connector.Issue {
	issue := securityAuditTestIssue()
	issue.AssignedToWorker = true
	issue.State = "Rework"
	issue.PullRequest.State = "OPEN"
	issue.PullRequest.MergeableState = "clean"
	issue.PullRequest.CIStatus = "success"
	issue.Comments = []connector.IssueComment{{Body: fmt.Sprintf("## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: %s\nblockers: []\nhuman_action: null\n```", status)}}
	return issue
}

func assertReworkGateWaitSafety(t *testing.T, stateIndex uint8, mergeIndex uint8, severityIndex uint8, metadata bool) {
	t.Helper()
	statuses := []string{workpad.StatusComplete, workpad.StatusBlocked, workpad.StatusInProgress, ""}
	mergeabilities := []string{"clean", "dirty", "conflicting"}
	severities := []string{"p1", "p2", "p3", ""}
	status := statuses[int(stateIndex)%len(statuses)]
	mergeability := mergeabilities[int(mergeIndex)%len(mergeabilities)]
	severity := severities[int(severityIndex)%len(severities)]
	issue := reworkGateWaitTestIssue(status)
	issue.PullRequest.MergeableState = mergeability
	issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: status}
	run := securityAuditPassingRun(issue)
	if severity != "" {
		run.Verdict = securityaudit.VerdictFail
		run.Findings = []securityaudit.Finding{{ID: "repair", Severity: severity}}
	}
	key := securityaudit.Key{ProjectID: run.ProjectID, Repository: run.Repository, PRNumber: run.PRNumber, BaseSHA: run.BaseSHA, HeadSHA: run.HeadSHA}
	audit := securityaudit.Evaluate(run, nil, key, run.ServiceIdentity, []string{"p1", "p2"})
	cfg := normalizeConfig(Config{ActiveStates: []string{"Rework"}, AutoPromote: AutoPromoteConfig{Enabled: true, GateWaitState: autoPromoteGateWaitSource, Gate: gate.Config{Kind: gate.KindCommand, SecurityAudit: gate.SecurityAuditConfig{Enabled: true, BlockOn: []string{"p1", "p2"}}}}})
	decision := implementCompletionProgressDecision{Issue: issue, Outcome: store.WorkAttemptTerminalNoProgress, WorkpadStatus: status, SecurityAudit: audit, CurrentSignature: autoPromoteReworkSignature{PRNumber: int64(issue.PullRequest.Number), HeadSHA: issue.PullRequest.HeadSHA}, WorkspaceDiffStats: DiffStats{Status: "clean"}}
	if !metadata {
		decision.WorkpadStatus = ""
	}
	got, reason := completedReworkGateWaitProgress(Running{Issue: issue}, decision, cfg, FinalStateCompleted)
	wantWait := metadata && status == workpad.StatusComplete && mergeability == "clean" && (severity == "" || severity == "p3")
	if (reason != "") != wantWait || (got.Outcome == store.WorkAttemptTerminalSuccess) != wantWait {
		t.Fatalf("status=%s mergeability=%s severity=%s metadata=%t: outcome=%s reason=%s wantWait=%t", status, mergeability, severity, metadata, got.Outcome, reason, wantWait)
	}
}

func TestReworkGateWaitSafetyMatrix(t *testing.T) {
	t.Parallel()
	for state := range uint8(4) {
		for merge := range uint8(3) {
			for severity := range uint8(4) {
				for _, metadata := range []bool{false, true} {
					t.Run(fmt.Sprintf("%d_%d_%d_%t", state, merge, severity, metadata), func(t *testing.T) {
						t.Parallel()
						assertReworkGateWaitSafety(t, state, merge, severity, metadata)
					})
				}
			}
		}
	}
}

func TestReworkGateWaitReloadReconcilesRepairs(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		mergeability string
		severity     string
		status       string
		wantWait     bool
	}{
		{name: "valid completed wait", mergeability: "clean", status: workpad.StatusComplete, wantWait: true},
		{name: "dirty to dirty", mergeability: "dirty", status: workpad.StatusComplete},
		{name: "configured P2 remains unresolved", mergeability: "clean", severity: "p2", status: workpad.StatusComplete},
		{name: "nonblocking P3", mergeability: "clean", severity: "p3", status: workpad.StatusComplete, wantWait: true},
		{name: "blocked Workpad", mergeability: "clean", status: workpad.StatusBlocked},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			issue := reworkGateWaitTestIssue(tt.status)
			issue.PullRequest.MergeableState = tt.mergeability
			now := time.Date(2026, 9, 5, 19, 18, 8, 0, time.UTC)
			attempt := successfulReworkGateWaitAttempt(now, issue, autoPromoteReworkSignature{PRNumber: int64(issue.PullRequest.Number), HeadSHA: issue.PullRequest.HeadSHA}, true)
			path := filepath.Join(t.TempDir(), "restart.db")
			db, err := store.Open(ctx, store.Config{Backend: store.BackendSQLite, Path: path})
			if err != nil {
				t.Fatal(err)
			}
			id, err := db.StartWorkAttempt(ctx, store.WorkAttemptStart{ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier, IssueURL: issue.URL, WorkerType: "agent", Lane: "Rework", StartedAt: attempt.StartedAt, PRNumber: attempt.PRNumber})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.CompleteWorkAttempt(ctx, store.WorkAttemptCompletion{AttemptID: id, CompletedAt: now, TerminalState: store.WorkAttemptTerminalSuccess, WorkerMetadataJSON: attempt.WorkerMetadataJSON}); err != nil {
				t.Fatal(err)
			}
			run := securityAuditPassingRun(issue)
			run.RecordedAt = now
			if tt.severity != "" {
				run.Verdict = securityaudit.VerdictFail
				run.Findings = []securityaudit.Finding{{ID: "repair", Severity: tt.severity, Body: "Repair the configured blocking finding."}}
			}
			if _, err := db.RecordSecurityAuditRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = store.Open(ctx, store.Config{Backend: store.BackendSQLite, Path: path})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			})
			orch := securityAuditTestOrchestrator(db)
			orch.cfg.ActiveStates = []string{"Todo", "In Progress", "Rework"}
			orch.cfg.AutoPromote.Enabled = true
			orch.cfg.AutoPromote.GateWaitState = autoPromoteGateWaitSource
			orch.cfg.AutoPromote.Gate.SecurityAudit.BlockOn = []string{"p1", "p2"}
			orch.cfg = normalizeConfig(orch.cfg)
			orch.connector = &implementProgressConnector{hydrated: issue, refreshed: issue}
			orch.workAttempts = db
			for _, cached := range []bool{false, true} {
				state := newState(orch.cfg)
				if cached {
					state.Completed[issue.ID] = completedFromGateWaitAttempt(issue, attempt)
				}
				orch.restoreDurableGateWaitCompletions(ctx, &state, []connector.Issue{issue})
				decision := orch.dispatchPlanner().dispatchableIssueDecision(issue, &state, false, now.Add(time.Hour), "")
				if wait := decision.reason == dispatchSkipAwaitingGate; wait != tt.wantWait || decision.dispatchable == tt.wantWait {
					t.Fatalf("cached=%t dispatch=%#v, want wait=%t", cached, decision, tt.wantWait)
				}
				if tt.severity == "p2" && state.RequiredGates[issue.ID].State != "failed" {
					t.Fatalf("audit gate = %#v", state.RequiredGates[issue.ID])
				}
			}
		})
	}
}

func TestReworkGateWaitRestoreDispatchesCurrentHeadValidatorRework(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name         string
		reworkState  string
		snapshotHead string
	}{
		{name: "stale default rework snapshot", reworkState: "Rework", snapshotHead: "stale-head"},
		{name: "empty configured rework snapshot", reworkState: "Repair"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			now := time.Date(2026, 9, 13, 15, 30, 0, 0, time.UTC)
			issue := reworkGateWaitTestIssue(workpad.StatusComplete)
			issue.State = tt.reworkState
			issue.PullRequest.HeadSHA = "current-head"
			snapshot := cloneIssue(issue)
			snapshot.PullRequest.HeadSHA = tt.snapshotHead

			memo := openValidatorMemoStore(t)
			attempt := successfulReworkGateWaitAttempt(now.Add(-time.Minute), issue, autoPromoteReworkSignature{
				PRNumber: int64(issue.PullRequest.Number),
				HeadSHA:  issue.PullRequest.HeadSHA,
			}, true)
			if !gateWaitAttemptMatchesPullRequest(attempt, issue, tt.reworkState) {
				t.Fatal("gate-wait attempt fixture does not match the current pull request")
			}
			wantValidator := gate.ValidatorResult{
				Submitted: true,
				Verdict:   gate.ValidatorVerdictRework,
				Score:     0.42,
				Summary:   "Repair the restored finding.",
				Findings: []gate.Finding{{
					Severity: "p1",
					Body:     "Use the current PR head.",
					Path:     "internal/orchestrator/autopromote_tick.go",
					Line:     1,
				}},
			}
			if err := memo.RecordValidatorVerdict(ctx, store.ValidatorVerdict{
				ProjectID:  "detent",
				IssueID:    issue.ID,
				HeadSHA:    issue.PullRequest.HeadSHA,
				Identifier: issue.Identifier,
				PRNumber:   attempt.PRNumber,
				Submitted:  wantValidator.Submitted,
				Verdict:    wantValidator.Verdict,
				Score:      wantValidator.Score,
				Summary:    wantValidator.Summary,
				Findings: []store.ValidatorFinding{{
					Severity: wantValidator.Findings[0].Severity,
					Body:     wantValidator.Findings[0].Body,
					Path:     wantValidator.Findings[0].Path,
					Line:     wantValidator.Findings[0].Line,
				}},
				Commented:  true,
				RecordedAt: now.Add(-30 * time.Second),
				UpdatedAt:  now.Add(-30 * time.Second),
			}); err != nil {
				t.Fatal(err)
			}

			cfg := normalizeConfig(Config{
				MaxConcurrentAgents: 1,
				ActiveStates:        []string{tt.reworkState},
				TerminalStates:      []string{"Done"},
				AutoPromote: AutoPromoteConfig{
					Enabled:       true,
					ReworkState:   tt.reworkState,
					GateWaitState: autoPromoteGateWaitSource,
					Gate: gate.Config{Kind: gate.KindCommand, Validator: gate.ValidatorConfig{
						Enabled: true,
					}},
				},
			})
			cfg.Project.ID = "detent"
			worker := newWorkerHostRunner()
			var logs strings.Builder
			tracker := &implementProgressConnector{hydrated: issue, refreshed: issue}
			orch := &Orchestrator{
				cfg:           cfg,
				connector:     tracker,
				workAttempts:  &recordingWorkAttemptStore{history: []store.WorkAttempt{attempt}},
				validatorMemo: memo,
				supervisor:    newTestSupervisor(t, worker, cfg),
				runResults:    make(chan runpkg.Completion),
				logger:        slog.New(slog.NewTextHandler(&logs, nil)),
			}
			state := newState(cfg)

			restored := orch.restoreDurableGateWaitCompletionState(ctx, &state, []connector.Issue{snapshot})
			transition := orch.transitionCompletedActiveIssuesToReviewWithHydratedValidatorHeads(
				ctx,
				&state,
				restored.issues,
				now,
				restored.validatorHeadHydration,
			)

			if _, waiting := state.Completed[issue.ID]; waiting {
				t.Fatalf("Completed[%q] retained after current-head rework verdict", issue.ID)
			}
			handoff, ok := state.PriorAttempts[issue.ID]
			if !ok || handoff.Source != "auto_promote" || handoff.Validator.Verdict != gate.ValidatorVerdictRework {
				t.Fatalf("PriorAttempts[%q] = %#v, want validator rework handoff", issue.ID, handoff)
			}
			if len(transition.dispatchCandidates) != 1 ||
				transition.dispatchCandidates[0].PullRequest == nil ||
				transition.dispatchCandidates[0].PullRequest.HeadSHA != issue.PullRequest.HeadSHA {
				t.Fatalf("dispatch candidates = %#v, want current head %q", transition.dispatchCandidates, issue.PullRequest.HeadSHA)
			}
			if strings.Contains(logs.String(), "reason=validator_missing") {
				t.Fatalf("stored current-head verdict logged validator_missing:\n%s", logs.String())
			}
			if tracker.hydrations != 1 {
				t.Fatalf("pull request hydrations = %d, want one current-head refresh", tracker.hydrations)
			}

			orch.dispatchReadyIssues(ctx, &state, transition.dispatchCandidates, now)
			request := receiveWorkerHostRunRequest(t, worker.started)
			if request.PriorAttempt.Validator.Verdict != wantValidator.Verdict ||
				request.PriorAttempt.Validator.Summary != wantValidator.Summary ||
				len(request.PriorAttempt.Validator.Findings) != 1 ||
				request.PriorAttempt.Validator.Findings[0].Body != wantValidator.Findings[0].Body {
				t.Fatalf("dispatched validator handoff = %#v, want %#v", request.PriorAttempt.Validator, wantValidator)
			}
		})
	}
}

func TestReworkGateWaitRestoreRequiresCurrentHeadHydration(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name       string
		hydrated   connector.Issue
		hydrateErr error
	}{
		{name: "hydration error", hydrateErr: errors.New("pull request unavailable")},
		{name: "mismatched issue", hydrated: connector.Issue{ID: "different-issue"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			now := time.Date(2026, 9, 13, 15, 45, 0, 0, time.UTC)
			issue := reworkGateWaitTestIssue(workpad.StatusComplete)
			issue.PullRequest.HeadSHA = "stale-head"
			attempt := successfulReworkGateWaitAttempt(now.Add(-time.Minute), issue, autoPromoteReworkSignature{
				PRNumber: int64(issue.PullRequest.Number),
				HeadSHA:  issue.PullRequest.HeadSHA,
			}, true)
			memo := openValidatorMemoStore(t)
			if err := memo.RecordValidatorVerdict(ctx, store.ValidatorVerdict{
				ProjectID:  "detent",
				IssueID:    issue.ID,
				HeadSHA:    issue.PullRequest.HeadSHA,
				Submitted:  true,
				Verdict:    gate.ValidatorVerdictRework,
				Summary:    "Stale findings must not dispatch.",
				Commented:  true,
				RecordedAt: now.Add(-30 * time.Second),
				UpdatedAt:  now.Add(-30 * time.Second),
			}); err != nil {
				t.Fatal(err)
			}
			cfg := normalizeConfig(Config{
				ActiveStates: []string{"Rework"},
				AutoPromote: AutoPromoteConfig{
					Enabled:       true,
					GateWaitState: autoPromoteGateWaitSource,
					Gate: gate.Config{Kind: gate.KindCommand, Validator: gate.ValidatorConfig{
						Enabled: true,
					}},
				},
			})
			cfg.Project.ID = "detent"
			tracker := &implementProgressConnector{
				hydrated:   tt.hydrated,
				refreshed:  issue,
				hydrateErr: tt.hydrateErr,
			}
			orch := &Orchestrator{
				cfg:           cfg,
				connector:     tracker,
				workAttempts:  memo,
				validatorMemo: memo,
				logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			state := newState(cfg)
			state.Completed[issue.ID] = completedFromGateWaitAttempt(issue, attempt)

			restored := orch.restoreDurableGateWaitCompletionState(ctx, &state, []connector.Issue{issue})
			transition := orch.transitionCompletedActiveIssuesToReviewWithHydratedValidatorHeads(
				ctx,
				&state,
				restored.issues,
				now,
				restored.validatorHeadHydration,
			)

			if handoff, ok := state.PriorAttempts[issue.ID]; ok {
				t.Fatalf("stale validator handoff restored after failed hydration: %#v", handoff)
			}
			if _, waiting := state.Completed[issue.ID]; !waiting {
				t.Fatalf("Completed[%q] removed after failed hydration", issue.ID)
			}
			if len(transition.dispatchCandidates) != 0 {
				t.Fatalf("dispatch candidates = %#v, want none after failed hydration", transition.dispatchCandidates)
			}
			if tracker.hydrations != 1 {
				t.Fatalf("pull request hydrations = %d, want one failed current-head refresh", tracker.hydrations)
			}
		})
	}
}

func TestReworkGateWaitRestoreSkipsHydrationWithoutDurableWait(t *testing.T) {
	t.Parallel()

	issue := reworkGateWaitTestIssue(workpad.StatusComplete)
	valid := successfulReworkGateWaitAttempt(time.Now(), issue, autoPromoteReworkSignature{
		PRNumber: int64(issue.PullRequest.Number),
		HeadSHA:  issue.PullRequest.HeadSHA,
	}, true)
	failed := valid
	failed.TerminalState = store.WorkAttemptTerminalNoProgress
	cfg := normalizeConfig(Config{
		ActiveStates: []string{"Rework"},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			GateWaitState: autoPromoteGateWaitSource,
			Gate: gate.Config{Kind: gate.KindCommand, Validator: gate.ValidatorConfig{
				Enabled: true,
			}},
		},
	})
	for _, tt := range []struct {
		name    string
		history []store.WorkAttempt
	}{
		{name: "no durable wait"},
		{name: "newer failure supersedes durable wait", history: []store.WorkAttempt{failed, valid}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tracker := &implementProgressConnector{hydrated: issue, refreshed: issue}
			orch := &Orchestrator{
				cfg:          cfg,
				connector:    tracker,
				workAttempts: &recordingWorkAttemptStore{history: tt.history},
			}
			state := newState(cfg)

			orch.restoreDurableGateWaitCompletionState(t.Context(), &state, []connector.Issue{issue})

			if tracker.hydrations != 0 {
				t.Fatalf("pull request hydrations = %d, want none without a current durable Rework wait", tracker.hydrations)
			}
			if _, restored := state.Completed[issue.ID]; restored {
				t.Fatal("superseded or absent durable Rework wait was restored")
			}
		})
	}
}

func TestAutoPromoteValidatorEnabledAllowsOperationalCompletion(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 13, 16, 0, 0, 0, time.UTC)
	issue := autoPromoteTickIssue("issue-operational-validator", nil, nil)
	issue.Description = operationalCompletionAuthorizationBody()
	issue.Comments = []connector.IssueComment{{
		Body: operationalCompletionWorkpadBody("The authorized maintenance completed successfully."),
	}}
	cfg := normalizeConfig(Config{
		TerminalStates: []string{"Done"},
		AutoPromote: AutoPromoteConfig{
			Enabled:     true,
			SourceState: issue.State,
			Gate: gate.Config{Kind: gate.KindCommand, Validator: gate.ValidatorConfig{
				Enabled: true,
			}},
		},
	})
	tracker := &autoPromoteTickConnector{}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{CompletionKind: workpad.CompletionOperational}

	result := orch.autoPromoteHumanReviewIssues(t.Context(), &state, []connector.Issue{issue}, now)

	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Done"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned = %#v, want %q", result.transitioned, issue.ID)
	}
}

func TestReworkGateWaitRefreshesWorkpadBeforeInvalidatingCompletion(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	now := time.Date(2026, 9, 13, 16, 0, 0, 0, time.UTC)
	current := reworkGateWaitTestIssue(workpad.StatusComplete)
	snapshot := cloneIssue(current)
	snapshot.Comments = reworkGateWaitTestIssue(workpad.StatusBlocked).Comments
	attempt := successfulReworkGateWaitAttempt(now.Add(-time.Minute), current, autoPromoteReworkSignature{
		PRNumber: int64(current.PullRequest.Number),
		HeadSHA:  current.PullRequest.HeadSHA,
	}, true)
	cfg := normalizeConfig(Config{
		ActiveStates: []string{"Rework"},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			GateWaitState: autoPromoteGateWaitSource,
			Gate: gate.Config{Kind: gate.KindCommand, Validator: gate.ValidatorConfig{
				Enabled: true,
			}},
		},
	})
	tracker := &implementProgressConnector{hydrated: current, refreshed: current}
	orch := &Orchestrator{
		cfg:          cfg,
		connector:    tracker,
		workAttempts: &recordingWorkAttemptStore{},
	}
	state := newState(cfg)
	state.Completed[current.ID] = completedFromGateWaitAttempt(current, attempt)

	orch.restoreDurableGateWaitCompletions(ctx, &state, []connector.Issue{snapshot})

	if _, waiting := state.Completed[current.ID]; !waiting {
		t.Fatal("current completed Workpad evidence was invalidated from a stale snapshot")
	}
}

func TestValidatorStageIdentityRequiresHeadSHA(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name        string
		pullRequest *connector.PullRequest
		branchName  string
		wantHead    string
	}{
		{name: "pull request head", pullRequest: &connector.PullRequest{HeadSHA: "head-sha", BranchName: "pr-branch"}, branchName: "issue-branch", wantHead: "head-sha"},
		{name: "pull request branch is not a head", pullRequest: &connector.PullRequest{BranchName: "pr-branch"}, branchName: "issue-branch"},
		{name: "issue branch is not a head", branchName: "issue-branch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			identity := validatorStageIdentityForIssue(connector.Issue{
				ID:          "issue-2554",
				PullRequest: tt.pullRequest,
				BranchName:  tt.branchName,
			})
			if identity.HeadSHA != tt.wantHead {
				t.Fatalf("HeadSHA = %q, want %q", identity.HeadSHA, tt.wantHead)
			}
			if (identity.Key != "") != (tt.wantHead != "") {
				t.Fatalf("Key = %q for head %q", identity.Key, tt.wantHead)
			}
		})
	}
}

func TestReworkCapabilityBlockerIsBoundedWithDependentTodo(t *testing.T) {
	t.Parallel()
	for _, humanAction := range []bool{true, false} {
		t.Run(fmt.Sprintf("explicit_human_action_%t", humanAction), func(t *testing.T) {
			t.Parallel()
			issue := reworkGateWaitTestIssue(workpad.StatusBlocked)
			action := "Restore worker-github-cli-auth under the configured credential policy."
			declaration := "human_action: " + action
			if !humanAction {
				declaration = "human_action: null\nblockers:\n  - reason: " + action + "\n    owner: human\n    recheck_interval: tick"
			}
			issue.Comments[0].Body = "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\n" + declaration + "\n```"
			issue.PullRequest.MergeableState = "dirty"
			tracker := &implementProgressConnector{hydrated: issue, refreshed: issue}
			attempts := &implementProgressAttemptStore{}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework"}, TerminalStates: []string{"Done"}, AutoPromote: AutoPromoteConfig{Enabled: true, GateWaitState: autoPromoteGateWaitSource, NoProgressLimit: 3, Gate: gate.Config{Kind: gate.KindCommand}}})
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts}
			state := newState(cfg)
			now := time.Date(2026, 9, 5, 19, 1, 0, 0, time.UTC)
			state.Running[issue.ID] = Running{Issue: issue, WorkAttemptID: 1, Mode: runpkg.RunModeImplement, StartedAt: now.Add(-time.Minute), DiffStats: DiffStats{Status: "clean"}}
			orch.handleRunResult(context.Background(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement}, Result: runpkg.RunResult{FinalState: FinalStateCompleted, DiffStats: DiffStats{Status: "clean"}}})
			completion := attempts.completions[len(attempts.completions)-1]
			if completion.TerminalState != store.WorkAttemptTerminalNoProgress {
				t.Fatalf("terminal state = %s", completion.TerminalState)
			}
			attempt := store.WorkAttempt{ID: 1, TerminalState: completion.TerminalState, WorkerMetadataJSON: completion.WorkerMetadataJSON}
			if completionGateWaitReasonFromAttempt(attempt) != "" {
				t.Fatal("capability blocker became a wait")
			}
			record, ok := implementProgressRecordFromAttempt(attempt)
			if !ok || record.HumanAction != action {
				t.Fatalf("lost human action: %#v", record)
			}
			attempts.history = append([]store.WorkAttempt{attempt}, attempts.history...)

			if len(tracker.updates) == 0 || tracker.updates[len(tracker.updates)-1].state != blockedStatusState {
				t.Fatalf("updates = %#v", tracker.updates)
			}
			if len(tracker.comments) == 0 || !strings.Contains(tracker.comments[len(tracker.comments)-1].body, action) {
				t.Fatal("park lost recovery requirement")
			}
			child := dispatchTestIssue("child", "Todo")
			child.BlockedBy = []connector.BlockedRef{{Identifier: issue.Identifier, State: "Blocked"}}
			decision := orch.dispatchPlanner().dispatchableIssueDecision(child, &state, false, time.Now(), "")
			if decision.dispatchable || decision.reason != dispatchSkipBlockedByDependency {
				t.Fatalf("dependent Todo dispatch=%#v", decision)
			}
		})
	}
}

func TestReworkGateWaitRejectsDirtyToDirtyEvidence(t *testing.T) {
	t.Parallel()
	issue := implementProgressIssue("same-head")
	issue.State = "Rework"
	issue.PullRequest.MergeableState = "dirty"
	issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, HumanAction: "Restore worker-github-cli-auth."}
	attempt := successfulReworkGateWaitAttempt(time.Now(), issue, autoPromoteReworkSignature{PRNumber: 1070, HeadSHA: "same-head"}, true)
	if gateWaitAttemptMatchesPullRequest(attempt, issue, "Rework") {
		t.Fatal("dirty-to-dirty durable completion remains eligible after restart")
	}
	if completedReworkGateWaitEvidenceCurrent(completedFromGateWaitAttempt(issue, attempt), issue) {
		t.Fatal("dirty-to-dirty in-memory completion remains eligible")
	}
}

func TestInvalidReworkWaitPreservesIndependentParks(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{spendProgressReason, dispatchLoopDetectedReason} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			issue := reworkGateWaitTestIssue(workpad.StatusComplete)
			issue.PullRequest.MergeableState = "dirty"
			now := time.Date(2026, 9, 5, 19, 18, 8, 0, time.UTC)
			attempt := successfulReworkGateWaitAttempt(now, issue, autoPromoteReworkSignature{PRNumber: int64(issue.PullRequest.Number), HeadSHA: issue.PullRequest.HeadSHA}, true)
			cfg := normalizeConfig(Config{ActiveStates: []string{"Rework"}, AutoPromote: AutoPromoteConfig{Enabled: true, GateWaitState: autoPromoteGateWaitSource, Gate: gate.Config{Kind: gate.KindCommand}}})
			orch := &Orchestrator{cfg: cfg, connector: &implementProgressConnector{refreshed: issue}, workAttempts: &recordingWorkAttemptStore{history: []store.WorkAttempt{attempt}}}
			state := newState(cfg)
			state.Completed[issue.ID] = completedFromGateWaitAttempt(issue, attempt)
			state.Blocked[issue.ID] = Blocked{Issue: issue, Reason: reason}
			orch.restoreDurableGateWaitCompletions(t.Context(), &state, []connector.Issue{issue})
			if _, exists := state.Completed[issue.ID]; exists {
				t.Fatal("invalid completion survived")
			}
			if state.Blocked[issue.ID].Reason != reason {
				t.Fatal("independent park was cleared")
			}
			if decision := orch.dispatchPlanner().dispatchableIssueDecision(issue, &state, false, now, ""); decision.dispatchable || decision.reason == dispatchSkipAwaitingGate {
				t.Fatalf("dispatch=%#v", decision)
			}
		})
	}
}
