package hubclient

import (
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestNativeImplementationReviewIsRecordedSeparately(t *testing.T) {
	t.Parallel()
	h := newNativeChangeHub(t, true)
	next := h.descriptor
	next.Gates.AutomatedReview = gate.AutomatedReviewRequired
	next.Gates.Validator = true
	next = next.WithID()
	if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: h.descriptor.ID, Policy: next}); err != nil {
		t.Fatal(err)
	}
	h.descriptor = next
	issue := h.createInProgress(t, "Review source before acceptance")
	h.claim(t, issue.ID)
	execution := h.scheduler.RunExecution(issue.ID)
	ctx, stop, err := execution.Guard(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	head := strings.Repeat("c", 40)
	execution.(runner.DiffExecution).SetDiffSource(nativeChangeDiff(head, "README.md"))
	execution.(runner.RepositoryExecution).SetRepository(nativeChangeRepository)
	if err := execution.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := execution.(runner.RuntimeExecution).ObserveRuntime(ctx, tracker.NativeRuntimeObservation{Phase: "implementation", Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: 41}}); err != nil {
		t.Fatal(err)
	}
	if err := execution.Checkpoint(ctx, tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: "unpushed", HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil {
		t.Fatal(err)
	}
	if err := execution.(runner.CompletionExecution).PrepareFinish(ctx, "succeeded", "", nil); err != nil {
		t.Fatal(err)
	}
	reviewer := execution.(runner.NativeReviewExecution)
	input, err := reviewer.ReviewVersion(ctx)
	if err != nil || input.Version == nil {
		t.Fatalf("required current-head review unavailable: %+v, %v", input, err)
	}
	result := gate.ValidatorResult{VersionID: input.Version.ID, HeadSHA: input.Version.HeadSHA, BaseSHA: input.Version.BaseSHA, Repository: input.Version.Repository, DiffDigest: input.Diff.Digest, Verdict: gate.ValidatorVerdictPass, Summary: "No source defects", SessionID: 123, Submitted: true, Score: 1}
	wrong := result
	wrong.HeadSHA = strings.Repeat("d", 40)
	if err := reviewer.RecordReview(ctx, wrong); err == nil {
		t.Fatal("moved review head was recorded")
	}
	if err := reviewer.RecordReview(ctx, result); err != nil {
		t.Fatal(err)
	}
	change := execution.(runner.ChangeExecution).NativeChange()
	detail, err := h.admin.runtimeChange(ctx, tracker.NativeWorkItemID(issue.ID), change.ChangeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Reviews) != 1 || detail.Reviews[0].ImplementationReview == nil || detail.Reviews[0].Validator != nil || detail.Reviews[0].Decision != "approved" {
		t.Fatalf("implementation review confused with acceptance: %+v", detail.Reviews)
	}
	if detail.Summary.Status == "reviewed" {
		t.Fatal("implementation review bypassed required validator acceptance")
	}
	if repeated, err := reviewer.ReviewVersion(ctx); err != nil || repeated.Version != nil {
		t.Fatalf("current source approval was not reused: %+v, %v", repeated, err)
	}
	if acceptance, err := execution.(runner.NativeValidatorExecution).ValidatorVersion(ctx); err != nil || acceptance.Version == nil {
		t.Fatalf("separate acceptance validator was suppressed: %+v, %v", acceptance, err)
	}
}
