package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type independentReviewExecution struct {
	*reworkReviewExecution
	receipts retryStepExecution
}

func (e *independentReviewExecution) RecordStep(ctx context.Context, receipt tracker.NativeStepReceipt) error {
	return e.receipts.RecordStep(ctx, receipt)
}

func (e *independentReviewExecution) Step(ctx context.Context, binding tracker.NativeStepReceipt) (tracker.NativeStepReceipt, error) {
	return e.receipts.Step(ctx, binding)
}

func TestIndependentFailureReviewResume(t *testing.T) {
	for _, test := range []struct {
		name                                                                   string
		pending, unknown, refused, changedSource, changedPolicy, humanFeedback bool
		want                                                                   bool
	}{
		{name: "repair done reuses source review", want: true},
		{name: "dependency pending", pending: true},
		{name: "unknown requires implementation", unknown: true},
		{name: "refused repair requires implementation", refused: true},
		{name: "rebased source requires review", changedSource: true},
		{name: "changed policy invalidates review", changedPolicy: true},
		{name: "new human feedback requires implementation", humanFeedback: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := initRunnerSourceRepo(t)
			base := strings.TrimSpace(runRunnerGit(t, repo, "rev-parse", "HEAD"))
			attemptDiffFile(t, repo, "example.txt", "candidate\n")
			runRunnerGit(t, repo, "add", "example.txt")
			runRunnerGit(t, repo, "commit", "-m", "Candidate")
			head := strings.TrimSpace(runRunnerGit(t, repo, "rev-parse", "HEAD"))
			capture, err := workspace.CaptureChangeSource(t.Context(), repo, base, head)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 27, 7, 0, 0, 0, time.UTC)
			version := tracker.ChangeVersion{ID: "version_example", ChangeID: "change_example", ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: base, HeadSHA: head, RunID: "run_example", AttemptID: "attempt_example", PolicyID: "policy_example", Source: &capture.Source, Repository: "https://github.com/example-org/example-project"}}
			version.Policy.ID = version.PolicyID
			version.Policy.Gates.AutomatedReview, version.Policy.Gates.Validator = gate.AutomatedReviewRequired, true
			version.Validation = &gate.CommandResult{Command: "check-input", HeadSHA: head, TreeSHA: strings.Repeat("a", 40)}
			message := "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"
			source := tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{AttemptID: version.AttemptID, RunID: version.RunID, PolicyID: version.PolicyID, CompletionBody: message, Disposition: &tracker.NativeDisposition{Status: "complete"}}, Status: "succeeded"}
			ref := &tracker.NativeChangeReference{ChangeID: version.ChangeID, VersionID: version.ID, HeadSHA: head}
			recovery := tracker.NativeRecovery{Change: ref, Lease: tracker.NativeLease{PolicyID: version.PolicyID}, Attempts: []tracker.NativeAttempt{source}, Issue: tracker.NativeIssue{DependencyReady: !test.pending, Blockers: []tracker.NativeDependency{{ID: "wi_repair", Terminal: !test.pending, State: "Done"}}}, ChangeDetail: &tracker.ChangeDetail{Change: tracker.ChangeRequest{ID: version.ChangeID, CurrentVersion: version.ID}, Versions: []tracker.ChangeVersion{version}}}
			prior := tracker.ChangeReview{ID: "source-review", VersionID: version.ID, Decision: "approved", CreatedAt: now.Add(-time.Hour), ImplementationReview: &gate.ValidatorResult{Verdict: "pass", VersionID: version.ID, BaseSHA: base, HeadSHA: head, Summary: "Retained source review", SessionID: 17}}
			recovery.ChangeDetail.Reviews = []tracker.ChangeReview{prior}
			originalState := tracker.NativeStepReviewState(recovery)
			diagnostic := "source.go:4:2: independent input failure"
			command := gate.CommandResult{Command: "check-input", HeadSHA: head, ExitCode: 1, Output: diagnostic, FinishedAt: now, BaseReproduction: &failureattribution.Observation{Command: "check-input", HeadSHA: base, ExitCode: 1, Output: diagnostic, Complete: !test.unknown}}
			validation := &gate.ValidatorResult{Verdict: "rework", BaseSHA: base, HeadSHA: head, Findings: []gate.Finding{{Severity: "p1", Body: diagnostic, CheckCommand: command.Command, CheckDiagnostic: diagnostic}}, Commands: []gate.CommandResult{command}}
			gate.NormalizeAttribution(validation)
			review := tracker.ChangeReview{ID: "failed-validator", VersionID: version.ID, Decision: "changes_requested", Validator: validation, RepairDependencies: []tracker.NativeWorkItemID{"wi_repair"}}
			if test.refused {
				review.RepairDependencies = nil
				review.RepairRefusal = "Dependencies cannot form a cycle"
			}
			recovery.ChangeDetail.Reviews = append(recovery.ChangeDetail.Reviews, review)
			recovery.ChangeDetail.Summary.NativeReview = "changes_requested"
			if test.humanFeedback {
				recovery.Discussion = []tracker.NativeComment{{ID: "feedback", Actor: tracker.Actor{Kind: "human", PrincipalID: "human_example"}, Body: "Revise the implementation", CreatedAt: now, UpdatedAt: now}}
			}
			if test.changedPolicy {
				recovery.ChangeDetail.Summary.Status = "stale_policy"
			}
			attempt := nativeReviewResumeAttempt(recovery, "check-input")
			if (attempt != nil) != (test.want || test.changedSource) {
				t.Fatalf("resume attempt=%v want=%v", attempt, test.want)
			}
			if attempt == nil {
				return
			}
			local := &workspace.RecoveryState{HeadSHA: head}
			current := &workspace.ChangeSource{Version: version, Bundle: capture.Bundle}
			if test.changedSource {
				current.Version.HeadSHA = strings.Repeat("d", 40)
				local.HeadSHA = current.Version.HeadSHA
			}
			if nativeReviewResumeMatches(recovery, local, current) != test.want {
				t.Fatalf("source binding reused changed source=%v", test.changedSource)
			}
			if !test.changedSource && tracker.NativeStepReviewState(recovery) != originalState {
				t.Fatal("independent rejection invalidated still-valid source review")
			}
			if recovery.ChangeDetail.Reviews[0].ImplementationReview != prior.ImplementationReview || recovery.ChangeDetail.Reviews[1].Validator.Commands[0].ExitCode != 1 {
				t.Fatal("resume rewrote retained evidence")
			}
			if !test.want {
				return
			}
			owner := &independentReviewExecution{
				reworkReviewExecution: &reworkReviewExecution{
					reworkArtifactsExecution: reworkArtifactsExecution{base: base, testExecution: testExecution{recovery: recovery}},
					source:                   *current,
				},
				receipts: retryStepExecution{finalizingTestExecution: &finalizingTestExecution{}},
			}
			input := NativeValidation{Version: &version, Diff: &connector.ValidationDiff{Repository: version.Repository, BaseSHA: base, HeadSHA: head, Digest: "fixture-diff"}}
			receipt, err := validationStepBinding(owner, input, "review")
			if err != nil {
				t.Fatal(err)
			}
			receipt.Validator = prior.ImplementationReview
			if err := owner.RecordStep(t.Context(), receipt); err != nil {
				t.Fatal(err)
			}
			backend := &completedExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: repo, Branch: "native"}, recoveryStates: []workspace.RecoveryState{{HeadSHA: head, WorkspaceFingerprint: "source-digest"}}}}, finalizationBase: base}
			failedGate := stepBinding(owner, "validation", tracker.NativeCheckpoint{HeadSHA: head, WorkspaceDigest: "source-digest"}, tracker.NativeChangeReference{})
			failedGate.Validation = &command
			if err := owner.RecordStep(t.Context(), failedGate); err != nil {
				t.Fatal(err)
			}
			agent := &resolvingReworkAgent{t: t, reviewVerdict: "pass"}
			cfg := config.Config{Tracker: config.Tracker{Kind: config.TrackerHubNative}, Gate: gate.Config{Run: "check-input", AutomatedReview: gate.AutomatedReviewRequired}}
			r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: cfg, Prompt: "Complete the source"}, Workspace: backend, AgentBackend: agent})
			if err != nil {
				t.Fatal(err)
			}
			r.now = func() time.Time { return now }
			result, err := r.Run(t.Context(), RunRequest{Mode: RunModeImplement, Issue: connector.Issue{ID: "native", Identifier: "native#1", State: "Rework"}, Execution: owner})
			if err != nil {
				t.Fatal(err)
			}
			if result.TurnStarted || agent.calls != 0 || agent.reviewCalls != 0 || backend.validationCalls != 1 || backend.finalizationCalls != 1 {
				t.Fatalf("repair retry repeated valid work or reused a failed gate: turn=%v coding=%d reviews=%d gates=%d finalizations=%d", result.TurnStarted, agent.calls, agent.reviewCalls, backend.validationCalls, backend.finalizationCalls)
			}
			if owner.validation == nil || owner.validation.ExitCode != 0 || owner.validation.HeadSHA != head || len(owner.reviews) != 1 || owner.reviews[0].Summary != "Retained source review" || owner.reviews[0].SessionID != 17 || owner.reviews[0].HeadSHA != head {
				t.Fatalf("repair retry lost gate execution or retained review: validation=%+v reviews=%+v", owner.validation, owner.reviews)
			}
			if failedGate.Validation.ExitCode != 1 || recovery.ChangeDetail.Reviews[0].ImplementationReview != prior.ImplementationReview {
				t.Fatal("repair retry changed historical evidence")
			}
		})
	}
}
