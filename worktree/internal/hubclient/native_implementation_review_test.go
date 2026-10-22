package hubclient

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestNativeImplementationReviewIsRecordedSeparately(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name            string
		validator       bool
		verdict         string
		decision        string
		technicalResume bool
		unsatisfiable   bool
		summaryFailure  bool
		feedback        string
	}{
		{name: "source pass leaves separate acceptance required", validator: true, verdict: "pass", decision: "approved"},
		{name: "unsatisfiable acceptance remains commented for completion", validator: true, verdict: "pass", decision: "approved", unsatisfiable: true},
		{name: "source pass without acceptance validator", verdict: "pass", decision: "approved"},
		{name: "source wait preserves typed pending evidence", validator: true, verdict: "wait", decision: "commented"},
		{name: "operator evidence resume survives restart and uses new review sessions", validator: true, verdict: "wait", decision: "commented", technicalResume: true},
		{name: "source rework preserves typed negative evidence", validator: true, verdict: "rework", decision: "changes_requested"},
		{name: "recorded source rework survives summary read failure", validator: true, verdict: "rework", decision: "changes_requested", summaryFailure: true},
		{name: "recorded source pass with failed summary cannot accept", verdict: "pass", decision: "approved", summaryFailure: true},
		{name: "equal server timestamp human feedback refreshes source approval", verdict: "pass", decision: "approved", feedback: "equal"},
		{name: "historical version feedback retains current source approval", verdict: "pass", decision: "approved", feedback: "historical"},
		{name: "historical version feedback is excluded from fresh source wait", verdict: "wait", decision: "commented", feedback: "historical"},
		{name: "runner completion commentary retains source approval", verdict: "pass", decision: "approved", feedback: "bot"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var h *nativeChangeHub
			if test.technicalResume {
				h = newNativeChangeHubWithStates(t, "In Review", []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}}, {Name: "In Progress", Dispatchable: true, Transitions: []string{"Blocked", "In Review", "Done"}}, {Name: "Blocked", Transitions: []string{"In Progress"}}, {Name: "In Review", Transitions: []string{"In Progress", "Done"}}, {Name: "Done", Terminal: true}})
			} else {
				h = newNativeChangeHub(t, true)
			}
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
			receipt := gate.CommandResult{Command: "make check", HeadSHA: head, TreeSHA: strings.Repeat("a", 40), StartedAt: time.Now().UTC().Add(time.Hour), DurationNS: int64(time.Second), Evidence: &gate.CommandEvidence{Environment: gate.CheckEnvironment{OS: "linux", Architecture: "amd64", GoVersion: "go1.26.9"}}}
			receipt.FinishedAt = receipt.StartedAt.Add(time.Second)
			receipt.ReceiptID = receipt.PipelineTiming("finalization").ID()
			if err := execution.(runner.SourceValidationExecution).RecordSourceValidation(ctx, receipt); err != nil {
				t.Fatal(err)
			}
			if err := execution.(runner.CompletionExecution).PrepareFinish(ctx, "succeeded", "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", nil); err != nil {
				t.Fatal(err)
			}
			reviewer := execution.(runner.NativeReviewExecution)
			input, err := reviewer.ReviewVersion(ctx)
			if err != nil || input.Version == nil {
				t.Fatalf("required current-head review unavailable: %+v, %v", input, err)
			}
			result := gate.ValidatorResult{VersionID: input.Version.ID, HeadSHA: input.Version.HeadSHA, BaseSHA: input.Version.BaseSHA, Repository: input.Version.Repository, DiffDigest: input.Diff.Digest, Verdict: test.verdict, Summary: "Independent source result", SessionID: 123, Submitted: true, Score: 1, Commands: []gate.CommandResult{receipt}, CriteriaEvidence: []gate.CriterionEvidence{{Criterion: "Complete the requested work.", Kind: gate.EvidenceReceipt, Reference: "make check", Behavior: "The source command passes for the reviewed head and tree.", ReceiptIDs: []string{receipt.ReceiptID}}}}
			wrong := result
			wrong.HeadSHA = strings.Repeat("d", 40)
			if err := reviewer.RecordReview(ctx, wrong); err == nil {
				t.Fatal("moved review head was recorded")
			}
			if test.summaryFailure {
				next := h.native.client.httpClient.Transport
				posted := false
				h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if posted && request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/changes/") {
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("summary unavailable")), Request: request}, nil
					}
					response, err := next.RoundTrip(request)
					if err == nil && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/reviews") && response.StatusCode >= 200 && response.StatusCode < 300 {
						posted = true
					}
					return response, err
				})
				err = reviewer.RecordReview(ctx, result)
				h.native.client.httpClient.Transport = next
				if !posted || (err != nil) != (test.verdict == "pass") {
					t.Fatalf("acknowledged review summary failure mishandled: posted=%v error=%v", posted, err)
				}
			} else if err := reviewer.RecordReview(ctx, result); err != nil {
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
			if change.ImplementationReview == nil || change.ImplementationReview.Verdict != test.verdict || change.Reviewed != (!test.summaryFailure && detail.Summary.Status == "reviewed") {
				t.Fatalf("source verdict or current review status omitted from completion: %+v", change)
			}
			if repeated, err := reviewer.ReviewVersion(ctx); err != nil || (repeated.Version == nil) != (test.verdict == "pass") {
				t.Fatalf("current source approval reuse differs from latest verdict: %+v, %v", repeated, err)
			} else if repeated.Version != nil && !reflect.DeepEqual(repeated.Commands, []gate.CommandResult{receipt}) {
				t.Fatalf("rejected review lost genuine command evidence: %+v", repeated.Commands)
			}
			if test.feedback != "" {
				if test.feedback == "bot" {
					if _, err := h.native.CreateComment(ctx, tracker.NativeWorkItemID(issue.ID), tracker.CreateComment{Mutation: nativeMutationKey(), Body: "Latest bot completion: source complete."}); err != nil {
						t.Fatal(err)
					}
				} else {
					comment := tracker.ChangeDiscussion{ID: "feedback_current", VersionID: input.Version.ID, Body: "Current source feedback", Actor: tracker.Actor{Kind: "human", PrincipalID: "operator"}, CreatedAt: detail.Reviews[0].CreatedAt}
					if test.feedback == "historical" {
						comment.VersionID = "version_previous"
						comment.CreatedAt = comment.CreatedAt.Add(time.Hour)
					}
					next := h.native.client.httpClient.Transport
					h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						response, err := next.RoundTrip(request)
						if err == nil && request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/changes/") && response.StatusCode == http.StatusOK {
							var current operatortool.ChangeResult
							if err := json.NewDecoder(response.Body).Decode(&current); err != nil {
								return nil, err
							}
							response.Body.Close()
							current.Detail.Discussion = append(current.Detail.Discussion, comment)
							raw, err := json.Marshal(current)
							if err != nil {
								return nil, err
							}
							response.Body = io.NopCloser(strings.NewReader(string(raw)))
						}
						return response, err
					})
					t.Cleanup(func() { h.native.client.httpClient.Transport = next })
				}
				fresh, err := reviewer.ReviewVersion(ctx)
				wantFresh := test.feedback == "equal" || test.verdict == "wait"
				if err != nil || (fresh.Version != nil) != wantFresh {
					t.Fatalf("server chronology/version/actor freshness differs: input=%+v error=%v", fresh, err)
				}
				if wantFresh && test.feedback == "historical" && len(fresh.Discussion.Change) != 0 {
					t.Fatalf("historical Change feedback became current source request: %+v", fresh.Discussion)
				}
				if test.feedback == "equal" && (len(fresh.Discussion.Change) != 1 || fresh.Discussion.Change[0].ID != "feedback_current") {
					t.Fatalf("equal-time feedback omitted from fresh source input: %+v", fresh.Discussion)
				}
				return
			}
			if test.technicalResume {
				if err := h.connector.UpdateIssueState(ctx, issue.ID, "Blocked"); err != nil {
					t.Fatal(err)
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
					t.Fatal(err)
				}
				if got := h.candidates(t); len(got) != 0 {
					t.Fatalf("technical hold redispatched: %+v", got)
				}
				other := h.createInProgress(t, "Independent ready work")
				h.claim(t, other.ID)
				otherExecution := h.scheduler.RunExecution(other.ID)
				if err := otherExecution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
					t.Fatal(err)
				}
				if err := h.connector.UpdateIssueState(t.Context(), other.ID, "Done"); err != nil {
					t.Fatal(err)
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), other.ID, "completed"); err != nil {
					t.Fatal(err)
				}
				current, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.admin.CreateComment(t.Context(), current.WorkItemID, tracker.CreateComment{Mutation: nativeMutationKey(), Body: "Unrelated context must preserve the technical hold."}); err != nil {
					t.Fatal(err)
				}
				h.reopen(func(string) error { return nil })
				if got := h.candidates(t); len(got) != 0 {
					t.Fatalf("restart or comment redispatched held review: %+v", got)
				}
				wrong := tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision - 1, State: "In Progress", Reason: "user_requested"}
				if _, err := h.admin.Transition(t.Context(), current.WorkItemID, wrong); err == nil {
					t.Fatal("stale operator revision resumed review")
				}
				if _, err := h.admin.Transition(t.Context(), current.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "In Progress", Reason: "user_requested"}); err != nil {
					t.Fatal(err)
				}
				h.claim(t, issue.ID)
				execution = h.scheduler.RunExecution(issue.ID)
				ctx, stop, err = execution.Guard(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				execution.(runner.DiffExecution).SetDiffSource(nativeChangeDiff(head, "README.md"))
				execution.(runner.RepositoryExecution).SetRepository(nativeChangeRepository)
				if err := execution.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
					t.Fatal(err)
				}
				if err := execution.(runner.RuntimeExecution).ObserveRuntime(ctx, tracker.NativeRuntimeObservation{Phase: "implementation", Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: 42}}); err != nil {
					t.Fatal(err)
				}
				if err := execution.Checkpoint(ctx, tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: "unpushed", HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil {
					t.Fatal(err)
				}
				if err := execution.(runner.CompletionExecution).PrepareFinish(ctx, "succeeded", "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", nil); err != nil {
					t.Fatal(err)
				}
				reviewer = execution.(runner.NativeReviewExecution)
				resumed, err := reviewer.ReviewVersion(ctx)
				if err != nil || resumed.Version == nil || resumed.Version.ID != input.Version.ID || resumed.Version.HeadSHA != head {
					t.Fatalf("operator resume changed version: %+v, %v", resumed, err)
				}
				result.Verdict = gate.ValidatorVerdictPass
				result.SessionID = 125
				if err := reviewer.RecordReview(ctx, result); err != nil {
					t.Fatal(err)
				}
				test.verdict = "pass"
			}
			if test.verdict == "pass" {
				acceptance, err := execution.(runner.NativeValidatorExecution).ValidatorVersion(ctx)
				if err != nil || (acceptance.Version != nil) != test.validator {
					t.Fatalf("separate acceptance validator requirement changed: %+v, %v", acceptance, err)
				}
				if test.validator {
					expectedCommands := []gate.CommandResult{receipt}
					if test.technicalResume {
						expectedCommands = append(expectedCommands, receipt)
					}
					if !reflect.DeepEqual(acceptance.Commands, expectedCommands) {
						t.Fatalf("fresh acceptance did not receive source-review command evidence: %+v", acceptance.Commands)
					}
					validator := execution.(runner.NativeValidatorExecution)
					var rejection *APIError
					if err := validator.RecordValidator(ctx, result); !errors.As(err, &rejection) || rejection.Rejection == nil || rejection.Rejection.Category != "acceptance_validation" || rejection.Rejection.Field != "validator.session_id" {
						t.Fatalf("source-review session could also authorize acceptance: %v", err)
					}
					remote, _ := nativeSSHExecution(t, ctx, execution, t.TempDir())
					err = remote.(runner.NativeValidatorExecution).RecordValidator(ctx, result)
					var carrier interface {
						NativeTerminalFailure() tracker.NativeTerminalFailure
					}
					if !errors.As(err, &carrier) {
						t.Fatalf("worker transport lost acceptance rejection: %v", err)
					}
					failure := carrier.NativeTerminalFailure().Public()
					if failure.Rejection == nil || failure.Rejection.Category != "acceptance_validation" || failure.Rejection.Field != "validator.session_id" || failure.Stage != "acceptance_publication" || failure.Attribution != "instance" {
						t.Fatalf("worker acceptance failure = %+v", failure)
					}
					if test.unsatisfiable {
						result.SessionID++
						result.CriteriaEvidence = []gate.CriterionEvidence{{Criterion: "Complete the requested work.", Kind: gate.EvidenceNotVerified, UnverifiedClass: gate.CriterionUnsatisfiable, Reference: "Requires production"}}
						gate.NormalizeCriterionEvidence(&result, []string{"Complete the requested work."}, receipt.TreeSHA)
						cfg := gate.ValidatorConfig{Enabled: true}
						gate.ApplyCriterionPolicy(cfg, &result)
						if decision, stopped := gate.EvaluateValidator(cfg, result); stopped {
							result.Verdict = string(decision.Action)
						}
						if err := validator.RecordValidator(ctx, result); err != nil {
							t.Fatal(err)
						}
						detail, err := h.admin.runtimeChange(ctx, tracker.NativeWorkItemID(issue.ID), change.ChangeID)
						completion := execution.(runner.ChangeExecution).NativeChange()
						if err != nil || len(detail.Reviews) != 2 || detail.Reviews[1].Decision != "commented" || completion.Validator == nil || completion.Validator.Verdict != gate.ValidatorVerdictWait || !reflect.DeepEqual(completion.Validator.UnsatisfiableCriteria(), []string{"Complete the requested work."}) || completion.Reviewed {
							t.Fatalf("unsatisfiable publication lost wait or criterion class: reviews=%+v completion=%+v error=%v", detail.Reviews, completion, err)
						}
						return
					}
					result.SessionID++
					result.Verdict = gate.ValidatorVerdictRework
					failed := receipt
					failed.ExitCode, failed.StartedAt = 7, receipt.StartedAt.Add(-2*time.Hour)
					failed.FinishedAt = failed.StartedAt.Add(time.Second)
					failed.ReceiptID = failed.PipelineTiming("finalization").ID()
					result.Commands = []gate.CommandResult{failed}
					if err := validator.RecordValidator(ctx, result); err != nil {
						t.Fatalf("fresh separate acceptance session was refused: %v", err)
					}
					retry, err := validator.ValidatorVersion(ctx)
					if err != nil || retry.Version == nil || !reflect.DeepEqual(retry.Commands, append(expectedCommands, failed)) {
						t.Fatalf("acceptance retry lost trusted review order under runner clock skew: %+v, %v", retry, err)
					}
					result.SessionID++
					result.Verdict = gate.ValidatorVerdictPass
					result.Commands = []gate.CommandResult{receipt}
					if err := validator.RecordValidator(ctx, result); err != nil {
						t.Fatalf("independent acceptance retry was refused: %v", err)
					}
					if test.technicalResume {
						detail, err := h.admin.runtimeChange(ctx, tracker.NativeWorkItemID(issue.ID), change.ChangeID)
						if err != nil || detail.Change.CurrentVersion != input.Version.ID || detail.Summary.Status != "reviewed" || len(detail.Reviews) != 4 {
							t.Fatalf("resumed review did not retain ordered authority: %+v, %v", detail.Summary, err)
						}
					}
				}
			}
		})
	}
}
