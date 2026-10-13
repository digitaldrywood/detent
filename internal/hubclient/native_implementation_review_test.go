package hubclient

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestNativeImplementationReviewIsRecordedSeparately(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		validator bool
		verdict   string
		decision  string
	}{
		{name: "source pass leaves separate acceptance required", validator: true, verdict: "pass", decision: "approved"},
		{name: "source pass without acceptance validator", verdict: "pass", decision: "approved"},
		{name: "source wait preserves typed pending evidence", validator: true, verdict: "wait", decision: "commented"},
		{name: "source rework preserves typed negative evidence", validator: true, verdict: "rework", decision: "changes_requested"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			next := h.descriptor
			next.Gates.AutomatedReview = gate.AutomatedReviewRequired
			next.Gates.Validator = test.validator
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
			receipt := gate.CommandResult{ReceiptID: "receipt-original", Command: "make check", HeadSHA: head, TreeSHA: strings.Repeat("a", 40), StartedAt: time.Now().UTC().Add(time.Hour), DurationNS: int64(time.Second), Evidence: &gate.CommandEvidence{Environment: gate.CheckEnvironment{OS: "linux", Architecture: "amd64", GoVersion: "go1.26.9"}}}
			if err := execution.(runner.SourceValidationExecution).RecordSourceValidation(ctx, receipt); err != nil {
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
			result := gate.ValidatorResult{VersionID: input.Version.ID, HeadSHA: input.Version.HeadSHA, BaseSHA: input.Version.BaseSHA, Repository: input.Version.Repository, DiffDigest: input.Diff.Digest, Verdict: test.verdict, Summary: "Independent source result", SessionID: 123, Submitted: true, Score: 1, Commands: []gate.CommandResult{receipt}}
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
			if len(detail.Reviews) != 1 || detail.Reviews[0].ImplementationReview == nil || detail.Reviews[0].Validator != nil || detail.Reviews[0].Decision != test.decision {
				t.Fatalf("implementation review confused with acceptance: %+v", detail.Reviews)
			}
			if (test.validator || test.verdict != "pass") && detail.Summary.Status == "reviewed" {
				t.Fatal("implementation review bypassed required validator acceptance")
			}
			if change.ImplementationReview == nil || change.ImplementationReview.Verdict != test.verdict || change.Reviewed != (detail.Summary.Status == "reviewed") {
				t.Fatalf("source verdict or current review status omitted from completion: %+v", change)
			}
			if repeated, err := reviewer.ReviewVersion(ctx); err != nil || (repeated.Version == nil) != (test.verdict == "pass") {
				t.Fatalf("current source approval reuse differs from latest verdict: %+v, %v", repeated, err)
			} else if repeated.Version != nil && !reflect.DeepEqual(repeated.Commands, []gate.CommandResult{receipt}) {
				t.Fatalf("rejected review lost genuine command evidence: %+v", repeated.Commands)
			}
			if test.verdict == "pass" {
				acceptance, err := execution.(runner.NativeValidatorExecution).ValidatorVersion(ctx)
				if err != nil || (acceptance.Version != nil) != test.validator {
					t.Fatalf("separate acceptance validator requirement changed: %+v, %v", acceptance, err)
				}
				if test.validator {
					if !reflect.DeepEqual(acceptance.Commands, []gate.CommandResult{receipt}) {
						t.Fatalf("fresh acceptance did not receive source-review command evidence: %+v", acceptance.Commands)
					}
					validator := execution.(runner.NativeValidatorExecution)
					if err := validator.RecordValidator(ctx, result); err == nil || !strings.Contains(err.Error(), "separate sessions") {
						t.Fatalf("source-review session could also authorize acceptance: %v", err)
					}
					result.SessionID++
					result.Verdict = gate.ValidatorVerdictRework
					failed := receipt
					failed.ReceiptID, failed.ExitCode, failed.StartedAt = "receipt-clock-behind-failure", 7, receipt.StartedAt.Add(-2*time.Hour)
					result.Commands = []gate.CommandResult{failed}
					if err := validator.RecordValidator(ctx, result); err != nil {
						t.Fatalf("fresh separate acceptance session was refused: %v", err)
					}
					retry, err := validator.ValidatorVersion(ctx)
					if err != nil || retry.Version == nil || !reflect.DeepEqual(retry.Commands, []gate.CommandResult{receipt, failed}) {
						t.Fatalf("acceptance retry lost trusted review order under runner clock skew: %+v, %v", retry, err)
					}
					result.SessionID++
					result.Verdict = gate.ValidatorVerdictPass
					if err := validator.RecordValidator(ctx, result); err != nil {
						t.Fatalf("independent acceptance retry was refused: %v", err)
					}
				}
			}
		})
	}
}
