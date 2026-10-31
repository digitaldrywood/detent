package hubclient

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNoChangeEvidenceUsesAttemptReceipts(t *testing.T) {
	h := newNativeChangeHub(t, true)
	issue := h.createInProgress(t, "Verify existing behavior")
	h.claim(t, issue.ID)
	execution := h.scheduler.RunExecution(issue.ID)
	ctx, stop, err := execution.Guard(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	head := strings.Repeat("c", 40)
	execution.(runner.DiffExecution).SetDiffSource(func(context.Context) (tracker.AttemptDiffRequest, bool) {
		return tracker.AttemptDiffRequest{BaseSHA: head, HeadSHA: head, Files: []tracker.AttemptDiffFile{}}, true
	})
	execution.(runner.RepositoryExecution).SetRepository(nativeChangeRepository)
	if err := execution.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := execution.Checkpoint(ctx, tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: "clean", HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil {
		t.Fatal(err)
	}
	message := "```detent-status\nschema: 1\nstatus: no_change_needed\nno_change:\n  verified_sha: " + head + "\n  evidence: focused regression passed\nblockers: []\nhuman_action: null\n```"
	if err := execution.(runner.CompletionExecution).PrepareFinish(ctx, "succeeded", message, nil); err != nil {
		t.Fatal(err)
	}
	if change := execution.(runner.ChangeExecution).NativeChange(); change != nil {
		t.Fatalf("no-change report created Change: %+v", change)
	}
	for _, stage := range []string{"review", "validator"} {
		input, err := execution.(runner.NativeReviewExecution).ReviewVersion(ctx)
		if err != nil || input.Version == nil || input.Version.BaseSHA != head || input.Version.HeadSHA != head || input.Version.ChangeID != "" || len(input.Discussion.Issue) == 0 {
			t.Fatalf("missing evidence input: %+v %v", input, err)
		}
		result := gate.ValidatorResult{Submitted: true, Verdict: "pass", HeadSHA: head, BaseSHA: head, Repository: nativeChangeRepository, Summary: "criteria verified", Score: 1}
		recovery := execution.Recovery()
		binding := tracker.NativeStepReceipt{Step: stage, PolicyID: recovery.Lease.PolicyID, ReviewState: tracker.NativeStepReviewState(recovery), Source: tracker.NativeCheckpoint{HeadSHA: head, WorkspaceDigest: input.Diff.Digest}, CompletedAt: time.Now(), Validator: &result}
		if err := execution.(runner.StepExecution).RecordStep(ctx, binding); err != nil {
			t.Fatal(err)
		}
		if stage == "review" {
			err = execution.(runner.NativeReviewExecution).RecordReview(ctx, result)
		} else {
			err = execution.(runner.NativeValidatorExecution).RecordValidator(ctx, result)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := execution.(interface {
		NoChangeResult(context.Context) (*runner.NativeNoChange, error)
	}).NoChangeResult(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Review == nil || result.Validator == nil {
		t.Fatalf("gate receipts lost: %+v", result)
	}
	left, right := net.Pipe()
	central := runner.NewSSHPeer(ctx, left, left, runner.NewSSHCallbackHandler(runner.RunRequest{Execution: execution}, nil, nil, nil))
	remote := runner.NewSSHPeer(ctx, right, right, nil)
	t.Cleanup(func() { central.Close(); remote.Close(); left.Close(); right.Close() })
	var forwarded *runner.NativeNoChange
	if err := remote.Call(ctx, "execution.NoChangeResult", &forwarded); err != nil || forwarded == nil || forwarded.Review == nil || forwarded.Validator == nil || forwarded.VerifiedSHA != head {
		t.Fatalf("SSH lost evidence receipts: %+v %v", forwarded, err)
	}
	if err := execution.Finish(ctx, "succeeded"); err != nil {
		t.Fatal(err)
	}
	recovery, err := h.admin.Recovery(ctx, tracker.NativeWorkItemID(issue.ID))
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Change != nil {
		t.Fatalf("published unwanted Change: %+v", recovery.Change)
	}
	if len(recovery.Attempts) == 0 || len(recovery.Attempts[len(recovery.Attempts)-1].Steps) != 2 {
		t.Fatalf("durable gate receipts lost: %+v", recovery.Attempts)
	}
}
