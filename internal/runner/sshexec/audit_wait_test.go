package sshexec_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/digitaldrywood/detent/internal/runner/phaserecovery"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runner/internal/gitfixture"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

const implementationComplete = "Verified the implementation.\n\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestLocalWorkerWireCompletionCarriesAuditWait(t *testing.T) {
	t.Parallel()
	ctx := testenv.WaitContext(t, t.Context())
	const question = "Does the upload handler enforce the body limit before parsing?"
	dir := gitfixture.SourceRepo(t, runGit)
	head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	ref := tracker.NativeChangeReference{ChangeID: "change_example", VersionID: "version_example", HeadSHA: head}
	owner := &auditWaitOwner{ref: ref, question: question}
	r, err := runner.NewRunner(runner.Dependencies{
		ProcessSource: func(context.Context, string) ([]int, error) { return nil, nil },
		Workflow:      config.Workflow{Config: config.Config{Gate: gate.Config{Kind: gate.KindHumanReview, SecurityAudit: gate.SecurityAuditConfig{Enabled: true}}}},
		Workspace:     auditWaitWorkspace{dir: dir, head: head},
		AgentBackend:  completedImplementation{},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := runner.RunRequest{
		Execution: owner, Issue: connector.Issue{ID: "native", Identifier: "native#1"},
		Mode: runner.RunModeImplement, DeferExecutionFinish: true,
	}
	left, right := net.Pipe()
	t.Cleanup(func() { left.Close(); right.Close() })
	central := runner.NewSSHPeer(ctx, left, left, runner.NewSSHCallbackHandler(request, nil, nil, nil))
	t.Cleanup(central.Close)
	var worker *runner.SSHPeer
	worker = runner.NewSSHPeer(ctx, right, right, func(ctx context.Context, method string, args []json.RawMessage) (any, error) {
		if method != "run" || len(args) != 1 {
			return nil, errors.New("unexpected worker callback")
		}
		var wire runner.SSHRunRequest
		if err := json.Unmarshal(args[0], &wire); err != nil {
			return nil, err
		}
		result, err := r.Run(ctx, wire.Bind(ctx, worker))
		return runner.NewSSHRunResponse(result, err), nil
	})
	t.Cleanup(worker.Close)

	var response runner.SSHRunResponse
	if err := central.Call(ctx, "run", &response, runner.NewSSHRunRequest(request)); err != nil {
		t.Fatal(err)
	}
	if err := response.Err(); err != nil || response.Result.FinalState != runner.FinalStateCompleted {
		t.Fatalf("wire execution did not complete: %s, %v", response.Result.FinalState, err)
	}
	change := response.Result.NativeChange
	if change == nil || change.VersionID != ref.VersionID || change.HeadSHA != ref.HeadSHA || change.SecurityAudit == nil || change.SecurityAudit.Verdict != "wait" {
		t.Fatalf("wire execution lost the current audit: %+v", change)
	}
	signal, reported := phaserecovery.CompletionSignalFromMessage(response.Result.FinalMessage)
	if !reported || signal == nil || signal.Invalid != nil || signal.Status != "blocked" || !strings.Contains(signal.HumanAction, question) || len(signal.Blockers) != 0 {
		t.Fatalf("wire completion did not route the audit question to a human: %s", response.Result.FinalMessage)
	}
}

type auditWaitOwner struct {
	ref       tracker.NativeChangeReference
	question  string
	recorded  *tracker.SecurityAuditReview
	published bool
}

func (*auditWaitOwner) Guard(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, nil
}

func (*auditWaitOwner) Finish(context.Context, string) error {
	return errors.New("worker tried to finish the host-owned execution")
}

func (*auditWaitOwner) Recovery() tracker.NativeRecovery                             { return tracker.NativeRecovery{} }
func (*auditWaitOwner) Start(context.Context, tracker.NativeExecutionIdentity) error { return nil }
func (*auditWaitOwner) Validate(context.Context) error                               { return nil }
func (*auditWaitOwner) Checkpoint(context.Context, tracker.NativeCheckpoint) error   { return nil }
func (*auditWaitOwner) ObserveRuntime(context.Context, tracker.NativeRuntimeObservation) error {
	return nil
}
func (*auditWaitOwner) SetRepository(string) {}
func (*auditWaitOwner) SetArtifactSource(string, func(context.Context, string, string) (artifact.GitCapture, error)) {
}
func (*auditWaitOwner) PrepareArtifacts(context.Context, string) error  { return nil }
func (*auditWaitOwner) ArtifactLog(context.Context, string) error       { return nil }
func (*auditWaitOwner) FinalizeArtifacts(context.Context, string) error { return nil }
func (*auditWaitOwner) PublishValidationEvidence(context.Context, []runner.ValidationEvidence) error {
	return nil
}

func (e *auditWaitOwner) PrepareFinish(_ context.Context, outcome, message string, failure *tracker.NativeTerminalFailure) error {
	disposition := runner.NativeDispositionFromMessage(message)
	if outcome != "succeeded" || failure != nil || disposition == nil || disposition.Status != "complete" {
		return errors.New("expected a completed implementation for publication")
	}
	e.published = true
	return nil
}

func (e *auditWaitOwner) SecurityAuditVersion(context.Context) (runner.NativeValidation, error) {
	if !e.published {
		return runner.NativeValidation{}, errors.New("audit requested before publication")
	}
	source := tracker.ChangeVersionInput{Repository: "example-org/example-project", BaseSHA: "base", HeadSHA: e.ref.HeadSHA}
	return runner.NativeValidation{
		Version: &tracker.ChangeVersion{ID: e.ref.VersionID, ChangeID: e.ref.ChangeID, ChangeVersionInput: source},
		Diff:    &connector.ValidationDiff{Repository: source.Repository, BaseSHA: source.BaseSHA, HeadSHA: source.HeadSHA, Digest: "digest"},
	}, nil
}

func (*auditWaitOwner) RecordStep(context.Context, tracker.NativeStepReceipt) error { return nil }

func (e *auditWaitOwner) Step(_ context.Context, binding tracker.NativeStepReceipt) (tracker.NativeStepReceipt, error) {
	if binding.Step != "security_audit" {
		return tracker.NativeStepReceipt{}, nil
	}
	if binding.Change != e.ref {
		return tracker.NativeStepReceipt{}, errors.New("audit requested for a different source")
	}
	binding.SecurityAudit = &tracker.SecurityAuditReview{VersionID: e.ref.VersionID, Verdict: "wait", Run: securityaudit.Run{
		HeadSHA:  e.ref.HeadSHA,
		Findings: []securityaudit.Finding{{ID: "upload", Certainty: securityaudit.CertaintyUndetermined, UnresolvedFact: e.question}},
	}}
	return binding, nil
}

func (e *auditWaitOwner) RecordSecurityAudit(_ context.Context, audit tracker.SecurityAuditReview) error {
	e.recorded = &audit
	return nil
}

func (e *auditWaitOwner) SourceReview(context.Context) (runner.NativeChange, error) {
	return runner.NativeChange{Changed: true, ChangeID: e.ref.ChangeID, VersionID: e.ref.VersionID, HeadSHA: e.ref.HeadSHA, SecurityAudit: e.recorded}, nil
}

func (*auditWaitOwner) PrepareSourceIteration(context.Context, tracker.NativeChangeReference) error {
	return errors.New("source iteration requested during audit wait")
}

func (*auditWaitOwner) PrepareReview(context.Context, tracker.NativeChangeReference) error {
	return errors.New("review continuation requested during audit wait")
}

type auditWaitWorkspace struct {
	dir  string
	head string
}

func (w auditWaitWorkspace) Create(context.Context, workspace.Issue) (workspace.Info, error) {
	return workspace.Info{Path: w.dir, Key: "native", Branch: "main"}, nil
}
func (w auditWaitWorkspace) RecoveryState(context.Context, workspace.Info, workspace.Issue) (workspace.RecoveryState, error) {
	return workspace.RecoveryState{HeadSHA: w.head, WorkspaceFingerprint: "tree"}, nil
}
func (auditWaitWorkspace) Cleanup(context.Context, string) error { return nil }
func (auditWaitWorkspace) BeforeRun(context.Context, workspace.Info, workspace.Issue) error {
	return nil
}
func (auditWaitWorkspace) AfterRun(context.Context, workspace.Info, workspace.Issue) {}
func (auditWaitWorkspace) DiffStat(context.Context, workspace.Info, workspace.Issue) (workspace.DiffStat, error) {
	return workspace.DiffStat{}, nil
}

type completedImplementation struct{}

func (completedImplementation) RunTurn(_ context.Context, _ runner.AgentTurnRequest, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	if err := update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: implementationComplete}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateTurnCompleted, Status: "completed"})
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx := testenv.WaitContext(t, t.Context())
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	procgroup.Configure(ctx, cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}
