package nativechange

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestLargeValidatorPublicationUsesLiveFence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale bool
	}{
		{name: "archive upload recovers from 503"},
		{name: "prior receipt survives review fence refusal", stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				descriptor := clientTestPolicy()
				lease := tracker.NativeLease{ID: "lease", WorkItemID: "wi_example", FencingToken: 1, PolicyID: descriptor.ID}
				const changeID, versionID = "change_example", "version_example"
				var guarded context.Context
				var archivePayload, reviewPayload []byte
				var archiveRef attachment.ArchiveReference
				var receipt string
				var diff tracker.AttemptDiffRequest
				var completion tracker.NativeRunEvent
				uploads, posts, reviews, reads, diffs, completions := 0, 0, 0, 0, 0, 0
				client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "fixture" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(req *http.Request) (*http.Response, error) {
					response := httptest.NewRecorder()
					switch {
					case strings.HasSuffix(req.URL.Path, "/validate"):
						io.WriteString(response, "{}")
					case strings.HasSuffix(req.URL.Path, "/capabilities"):
						json.NewEncoder(response).Encode(map[string]any{"features": []string{tracker.NativeValidatorReceiptCapability}})
					case req.URL.Query().Get("view") == "validator_receipt":
						reads++
						var cause *runner.CancellationCause
						if !errors.As(context.Cause(guarded), &cause) || cause.Reason != "execution_authority_unavailable" {
							t.Error("receipt read started before typed fence cancellation")
						}
						json.NewEncoder(response).Encode(map[string]string{"request_sha256": receipt})
					case req.URL.Query().Get("view") == "bounded":
						sections := map[string]operatortool.ChangeReadPage{}
						for _, name := range []string{"versions", "reviews", "checks", "discussion"} {
							sections[name] = operatortool.ChangeReadPage{Complete: true}
						}
						json.NewEncoder(response).Encode(operatortool.ChangeResult{
							OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: string(lease.WorkItemID),
							Detail: &tracker.ChangeDetail{
								Change:  tracker.ChangeRequest{ID: changeID, CurrentVersion: versionID, ProjectID: "prj_example", WorkItemID: lease.WorkItemID},
								Summary: tracker.ChangeSummary{Status: "reviewed"},
							},
							Read: &operatortool.ChangeRead{VersionID: versionID, Sections: sections},
						})
					case strings.HasSuffix(req.URL.Path, "/archives"):
						uploads++
						raw, err := io.ReadAll(req.Body)
						if err != nil {
							return nil, err
						}
						if req.Header.Get(attachment.ArchiveLeaseHeader) != string(lease.ID) || req.Header.Get(attachment.ArchiveFenceHeader) != "1" {
							t.Fatal("archive upload omitted the original fence")
						}
						if archivePayload != nil && !bytes.Equal(archivePayload, raw) {
							t.Fatal("archive upload replay changed the payload")
						}
						archivePayload = raw
						if !tc.stale && uploads == 1 {
							response.WriteHeader(http.StatusServiceUnavailable)
							io.WriteString(response, `{"code":"unavailable"}`)
							break
						}
						var archive changerequest.ValidatorArchive
						if err := json.Unmarshal(raw, &archive); err != nil {
							return nil, err
						}
						command, err := json.Marshal(archive.Review)
						if err != nil {
							return nil, err
						}
						digest := sha256.Sum256(command)
						receipt = hex.EncodeToString(digest[:])
						archiveRef, _, err = attachment.PrepareArchive("org_example", 1, bytes.NewReader(raw))
						if err != nil {
							return nil, err
						}
						json.NewEncoder(response).Encode(archiveRef)
					case strings.HasSuffix(req.URL.Path, "/reviews"):
						posts++
						raw, err := io.ReadAll(req.Body)
						if err != nil {
							return nil, err
						}
						var submission changerequest.ReviewSubmission
						if json.Unmarshal(raw, &submission) != nil || submission.LeaseID != lease.ID || submission.FencingToken != lease.FencingToken ||
							submission.ValidatorArchive == nil || submission.ValidatorArchive.SHA256 != archiveRef.SHA256 {
							t.Fatal("review did not carry the reserved archive and original fence")
						}
						if tc.stale && posts == 2 {
							if !bytes.Equal(raw, reviewPayload) {
								t.Fatal("review replay changed the original command")
							}
							response.WriteHeader(http.StatusConflict)
							io.WriteString(response, `{"code":"stale_fencing_token"}`)
							break
						}
						reviews++
						reviewPayload = raw
						if tc.stale {
							return nil, io.ErrUnexpectedEOF
						}
						io.WriteString(response, `{"review_id":"review_example","version_id":"version_example"}`)
					case strings.HasSuffix(req.URL.Path, "/diff"):
						diffs++
						if err := json.NewDecoder(req.Body).Decode(&diff); err != nil {
							return nil, err
						}
						io.WriteString(response, "{}")
					case strings.HasSuffix(req.URL.Path, "/events"):
						completions++
						if err := json.NewDecoder(req.Body).Decode(&completion); err != nil {
							return nil, err
						}
						response.WriteHeader(http.StatusNoContent)
					case strings.HasSuffix(req.URL.Path, "/release"):
						response.WriteHeader(http.StatusNoContent)
					default:
						t.Fatalf("unexpected request: %s %s", req.Method, req.URL)
					}
					return response.Result(), nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_example", "prj_example")
				if err != nil {
					t.Fatal(err)
				}
				scheduler, err := newTestScheduler(client, SchedulerConfig{Now: time.Now, LeaseTTL: time.Minute, HeartbeatInterval: time.Second, Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}})
				if err != nil {
					t.Fatal(err)
				}
				source := fixtureConnector(native)
				execution := NewExecution(scheduler, executioncontract.ExecutionClaim{Source: source, Lease: lease}).(*nativeExecution)
				fixtureInstallClaim(t, scheduler, nativeClaim{source: source, lease: lease, deadline: time.Now().Add(time.Minute), execution: execution}, descriptor, "project")
				execution.data.Identity = &tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "example"}
				head := nativeChangeFixtureHead("c")
				execution.SetDiffSource(nativeChangeDiff(head, "README.md"))
				execution.change = &runner.NativeChange{ChangeID: changeID, VersionID: versionID, Changed: true, HeadSHA: head, BaseSHA: nativeChangeFixtureHead("a")}
				execution.settled = true
				var stop func()
				guarded, stop, err = execution.Guard(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				result := gate.ValidatorResult{VersionID: versionID, SessionID: 41, Verdict: "pass", Commands: []gate.CommandResult{{Command: "make test", Output: strings.Repeat("x", 1300<<10)}}}
				if err := execution.RecordValidator(guarded, result); err != nil {
					t.Fatal(err)
				}
				if execution.change.Validator == nil || execution.change.Validator.SessionID != result.SessionID {
					t.Fatal("committed validator result was not recorded")
				}
				if tc.stale {
					for _, ctx := range []context.Context{context.WithoutCancel(guarded), t.Context()} {
						if err := execution.Finish(ctx, "succeeded"); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
							t.Errorf("finish after review fence refusal = %v", err)
						}
					}
					if uploads != 1 || posts != 2 || reviews != 1 || reads != 1 || diffs != 0 || completions != 0 {
						t.Fatalf("after refusal: uploads=%d posts=%d reviews=%d receipt reads=%d diffs=%d completions=%d", uploads, posts, reviews, reads, diffs, completions)
					}
					return
				}
				if err := execution.Finish(guarded, "succeeded"); err != nil {
					t.Fatal(err)
				}
				if uploads != 2 || posts != 1 || reviews != 1 || reads != 0 || diffs != 1 || completions != 1 ||
					completion.Data.Outcome != "succeeded" || diff.Generation.Seq != completion.Data.Sequence {
					t.Fatalf("publication did not recover: uploads=%d posts=%d reviews=%d receipt reads=%d diffs=%d completions=%d outcome=%q",
						uploads, posts, reviews, reads, diffs, completions, completion.Data.Outcome)
				}
			})
		})
	}
}

func TestNativePrepareReviewAcrossAttempts(t *testing.T) {
	for _, outcome := range []string{"audit publication failed", "succeeded"} {
		t.Run(outcome, func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			previousPolicy := h.descriptor.ID
			h.descriptor.Gates.AutomatedReview = gate.AutomatedReviewRequired
			h.descriptor.Gates.SecurityAudit = true
			h.descriptor = h.descriptor.WithID()
			if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: previousPolicy, Policy: h.descriptor}); err != nil {
				t.Fatal(err)
			}
			issue := h.createInProgress(t, "Review preserved source")
			item := tracker.NativeWorkItemID(issue.ID)
			head := nativeChangeFixtureHead("c")
			start := func() *nativeExecution {
				t.Helper()
				h.claim(t, issue.ID)
				e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
				e.SetRepository(nativeChangeRepository)
				e.SetChangeSource(func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
					return nativeChangeSourceFixture(ctx, t, base, head), nil
				})
				e.SetDiffSource(nativeChangeDiff(head, "README.md"))
				if err := e.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
					t.Fatal(err)
				}
				if err := e.Checkpoint(t.Context(), tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil {
					t.Fatal(err)
				}
				if err := e.PrepareFinish(t.Context(), "succeeded", "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", nil); err != nil {
					t.Fatal(err)
				}
				return e
			}
			first := start()
			change := first.NativeChange()
			original, err := h.admin.Change(t.Context(), item, change.ChangeID)
			if err != nil || len(original.Versions) != 1 {
				t.Fatalf("first publication: %+v, %v", original, err)
			}
			finish := "succeeded"
			if outcome == "audit publication failed" {
				audit := tracker.SecurityAuditReview{VersionID: change.VersionID, Verdict: "pass", Run: securityaudit.Run{BaseSHA: change.BaseSHA, HeadSHA: change.HeadSHA}}
				if err := first.RecordSecurityAudit(t.Context(), audit); err == nil {
					t.Fatal("audit without execution evidence was accepted")
				}
				finish = "failed"
			}
			if err := first.Finish(t.Context(), finish); err != nil {
				t.Fatal(err)
			}
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
				t.Fatal(err)
			}
			current, err := h.admin.Issue(t.Context(), item)
			if err != nil {
				t.Fatal(err)
			}
			if current.State != "In Progress" || finish == "succeeded" {
				target := "In Progress"
				if current.State == "Done" || current.State == "In Progress" {
					target = "Todo"
				}
				if _, err := h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: target, Reason: "user_requested"}); err != nil {
					t.Fatal(err)
				}
				confirmIssueContract(t, h.admin, issue.ID)
			}
			second := start()
			if second.data.AttemptID == first.data.AttemptID {
				t.Fatal("second execution reused the first attempt identity")
			}
			change = second.NativeChange()
			ref := tracker.NativeChangeReference{ChangeID: change.ChangeID, VersionID: change.VersionID, HeadSHA: change.HeadSHA}
			if err := second.PrepareReview(t.Context(), ref); err != nil {
				t.Fatalf("prepare second attempt review: %v", err)
			}
			review, err := second.ReviewVersion(t.Context())
			if err != nil || review.Version == nil || review.Version.ID != ref.VersionID || review.Version.BaseSHA != original.Versions[0].BaseSHA || review.Version.HeadSHA != head || review.Version.PolicyID != h.descriptor.ID || review.Version.Policy.ID != h.descriptor.ID || review.Version.AttemptID != first.data.AttemptID || review.Version.RunID != first.data.RunID {
				t.Fatalf("review lost source or recorded owner binding: %+v, %v", review.Version, err)
			}
			latest, err := h.admin.Change(t.Context(), item, ref.ChangeID)
			if err != nil || !reflect.DeepEqual(original.Versions, latest.Versions) {
				t.Fatalf("prior version provenance changed: %+v, %v", latest.Versions, err)
			}
			for _, test := range []struct {
				name            string
				code            string
				previousVersion string
				mutate          func(*tracker.ChangeDetail)
				prepare         func(*nativeExecution)
			}{
				{name: "missing change", code: "change_missing", prepare: func(e *nativeExecution) { e.change = nil }},
				{name: "landing", code: "change_landed", prepare: func(e *nativeExecution) { e.change.Landing = &runner.NativeLanding{} }},
				{name: "change id", code: "change_id_mismatch", prepare: func(e *nativeExecution) { e.change.ChangeID = "private id" }},
				{name: "version id", code: "version_id_mismatch", prepare: func(e *nativeExecution) { e.change.VersionID = "private version" }},
				{name: "published head", code: "head_mismatch", prepare: func(e *nativeExecution) { e.change.HeadSHA = "private head" }},
				{name: "missing disposition", code: "disposition_missing", prepare: func(e *nativeExecution) { e.preparedDisposition = nil }},
				{name: "incomplete disposition", code: "disposition_incomplete", prepare: func(e *nativeExecution) { e.preparedDisposition.Status = "private status" }},
				{name: "blockers", code: "disposition_blockers", prepare: func(e *nativeExecution) { e.preparedDisposition.Blockers = true }},
				{name: "human action", code: "disposition_human_action", prepare: func(e *nativeExecution) { e.preparedDisposition.HumanAction = true }},
				{name: "reason", code: "disposition_reason", prepare: func(e *nativeExecution) { e.preparedDisposition.ReasonCode = "private reason" }},
				{name: "no change", code: "no_change_source_mismatch", prepare: func(e *nativeExecution) { e.change.NoChange = &tracker.NativeNoChangeCompletion{} }},
				{name: "changed current version", code: "version_not_current", previousVersion: "version_previous"},
				{name: "landed current version", code: "version_not_current", mutate: func(d *tracker.ChangeDetail) { d.Change.Landed = &tracker.ChangeLanding{VersionID: ref.VersionID} }},
				{name: "unavailable version", code: "version_missing", mutate: func(d *tracker.ChangeDetail) { d.Versions = nil }},
				{name: "head", code: "source_owner_mismatch", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].HeadSHA = nativeChangeFixtureHead("d") }},
				{name: "base", code: "source_owner_mismatch", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].BaseSHA = nativeChangeFixtureHead("b") }},
				{name: "policy", code: "source_owner_mismatch", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].PolicyID = "policy_other" }},
				{name: "frozen policy", code: "source_owner_mismatch", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].Policy.ID = "policy_other" }},
				{name: "item", code: "", mutate: func(d *tracker.ChangeDetail) { d.Change.WorkItemID = "wi_other" }},
				{name: "project", code: "", mutate: func(d *tracker.ChangeDetail) { d.Change.ProjectID = "prj_other" }},
				{name: "attempt", code: "source_owner_mismatch", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].AttemptID = "attempt_other" }},
				{name: "run", code: "source_owner_mismatch", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].RunID = "run_other" }},
				{name: "unrecovered source", code: "source_owner_mismatch", prepare: func(e *nativeExecution) { e.recoveredSource = nil }},
				{name: "unrecorded source owner", code: "source_owner_mismatch", prepare: func(e *nativeExecution) { e.claim.recovery.Attempts = nil }},
			} {
				t.Run(test.name, func(t *testing.T) {
					savedChange, savedDisposition := second.change, second.preparedDisposition
					change, disposition := *savedChange, *savedDisposition
					second.change, second.preparedDisposition = &change, &disposition
					t.Cleanup(func() { second.change, second.preparedDisposition = savedChange, savedDisposition })
					source, recovery := second.recoveredSource, second.claim.recovery
					t.Cleanup(func() { second.recoveredSource, second.claim.recovery = source, recovery })
					if test.prepare != nil {
						test.prepare(second)
					}
					transport := fixtureClient(h.native.Transport()).requests.Load()
					fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						response, err := transport.RoundTrip(request)
						if err != nil || request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, "/"+ref.ChangeID) {
							return response, err
						}
						var page operatortool.ChangeResult
						if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
							response.Body.Close()
							return nil, err
						}
						response.Body.Close()
						if test.mutate != nil && page.Detail != nil && len(page.Detail.Versions) != 0 {
							test.mutate(page.Detail)
						}
						body, err := json.Marshal(page)
						if err != nil {
							return nil, err
						}
						response.Body = io.NopCloser(strings.NewReader(string(body)))
						return response, nil
					}))
					t.Cleanup(func() { fixtureClient(h.native.Transport()).requests.Store(transport) })
					expected := ref
					if test.previousVersion != "" {
						expected.VersionID = test.previousVersion
						second.change.VersionID = test.previousVersion
					}
					err := second.PrepareReview(t.Context(), expected)
					if test.code == "" {
						var readErr *runner.NativeImplementationReviewError
						if !errors.As(err, &readErr) || !errors.Is(err, ErrUnavailable) {
							t.Fatalf("invalid Hub response = %v, want unavailable review", err)
						}
						return
					}
					var check reviewRefusal
					if !errors.Is(err, runner.ErrNativeRecoveryRequired) || !errors.As(err, &check) || string(check) != test.code || err.Error() != runner.ErrNativeRecoveryRequired.Error()+": "+test.code {
						t.Fatalf("review refusal = %v, want recovery with fixed check %q", err, test.code)
					}
				})
			}
		})
	}
}

func TestRecordValidatorRecoversCommittedLegacyReview(t *testing.T) {
	h := newNativeChangeHub(t, true)
	previousPolicy := h.descriptor.ID
	h.descriptor.Gates.Validator = true
	h.descriptor = h.descriptor.WithID()
	if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: previousPolicy, Policy: h.descriptor}); err != nil {
		t.Fatal(err)
	}
	issue := h.createInProgress(t, "Recover committed validator review")
	item := tracker.NativeWorkItemID(issue.ID)
	h.claim(t, issue.ID)
	execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
	head := nativeChangeFixtureHead("c")
	execution.SetRepository(nativeChangeRepository)
	execution.SetChangeSource(func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
		return nativeChangeSourceFixture(ctx, t, base, head), nil
	})
	execution.SetDiffSource(nativeChangeDiff(head, "README.md"))
	if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "example"}); err != nil {
		t.Fatal(err)
	}
	sessions := api.NewRunnerSessions(h.native)
	ctx := sessions.BindExecution(t.Context(), execution)
	finishSession := func(role string) int64 {
		t.Helper()
		now := h.clock()
		id, err := sessions.StartSession(ctx, store.SessionStart{IssueID: issue.ID, StartedAt: now, AgentRole: role, Model: "example"})
		if err != nil {
			t.Fatal(err)
		}
		if err := sessions.FinishSession(ctx, id, store.SessionFinish{CompletedAt: now, FinalState: "completed"}); err != nil {
			t.Fatal(err)
		}
		if _, err := sessions.RecordUsageEvent(ctx, store.UsageEvent{IssueID: issue.ID, SessionID: id, StartedAt: now, FinishedAt: now, Outcome: "completed"}); err != nil {
			t.Fatal(err)
		}
		if _, err := sessions.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{IssueID: issue.ID, SessionID: id, StartedAt: now, FinishedAt: now, PhaseType: store.WorkflowPhaseTypeAgentSession, PhaseName: "agent_active", Status: "completed"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if err := execution.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{Phase: "implementation", Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: finishSession(runner.RoleCode)}}); err != nil {
		t.Fatal(err)
	}
	if err := execution.Checkpoint(ctx, tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil {
		t.Fatal(err)
	}
	if err := execution.PrepareFinish(ctx, "succeeded", "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", nil); err != nil {
		t.Fatal(err)
	}
	change := execution.NativeChange()
	input, err := execution.ValidatorVersion(ctx)
	if err != nil || input.Version == nil {
		t.Fatalf("validator input: %v", err)
	}
	version := input.Version
	result := gate.ValidatorResult{
		VersionID: version.ID, SessionID: finishSession("validator"), Submitted: true, Verdict: "pass", Score: 1,
		Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, DiffDigest: version.Source.DiffSHA256,
		CriteriaEvidence: []gate.CriterionEvidence{{Criterion: "Complete the requested work.", Kind: "test", Reference: "go test ./...", Behavior: "The test checks the completed work"}},
	}
	command := tracker.ReviewChange{
		Mutation:          tracker.Mutation{IdempotencyKey: fmt.Sprintf("%s:validator:%d", execution.data.AttemptID, result.SessionID), LeaseID: execution.claim.lease.ID, FencingToken: execution.claim.lease.FencingToken},
		ExpectedVersionID: version.ID, Decision: "approved", Validator: &result,
	}
	raw, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	result.CriteriaEvidence[0].Behavior += strings.Repeat("x", maxResponseBytes-len(raw))
	for i := range 201 {
		if _, err := h.admin.DiscussChange(t.Context(), item, change.ChangeID, tracker.DiscussChange{Mutation: nativeMutationKey(), Body: fmt.Sprintf("Retained discussion %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.admin.Change(t.Context(), item, change.ChangeID); err == nil {
		t.Fatal("legacy detail read accepted more than 200 records")
	} else {
		var refusal *APIError
		if !errors.As(err, &refusal) || refusal.Code != "change_detail_too_large" {
			t.Fatal(err)
		}
	}
	cfg := fixtureClient(h.native.Transport()).config
	transport := cfg.HTTPClient.Transport
	posts := 0
	cfg.HTTPClient = &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("view") == "validator_receipt" {
			r = r.Clone(r.Context())
			r.URL.RawQuery = ""
		}
		response, err := transport.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			response.Body.Close()
			response.Body = io.NopCloser(strings.NewReader(`{"version":"0.190.2","features":["native_runtime_evidence"]}`))
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews") && response.StatusCode == http.StatusOK {
			posts++
			ack, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				return nil, err
			}
			t.Logf("committed inline review request=%d bytes acknowledgement=%d bytes", r.ContentLength, len(ack))
			if len(ack) <= maxResponseBytes {
				t.Errorf("committed review acknowledgement has %d bytes", len(ack))
			}
			response.Body = io.NopCloser(bytes.NewReader(ack))
		}
		return response, nil
	})}
	client, err := newTestClient(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := client.Native(h.organization, h.project)
	if err != nil {
		t.Fatal(err)
	}
	execution.claim.source = fixtureConnector(legacy)
	if err := execution.RecordValidator(ctx, result); err != nil {
		t.Fatal(err)
	}
	if posts != 1 || execution.stepReviews == nil || len(execution.stepReviews.Reviews) != 1 {
		t.Fatal("runner did not recover the single committed review")
	}
	review := execution.stepReviews.Reviews[0]
	if review.Validator == nil || review.Validator.SessionID != result.SessionID || review.SessionMachineID != execution.claim.lease.MachineID || !reflect.DeepEqual(review.Validator.CriteriaEvidence, result.CriteriaEvidence) {
		t.Fatal("recovered validator review differs from the committed evidence")
	}
}

func TestRecordedValidatorRequiresExactPublication(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*tracker.ChangeDetail, *gate.ValidatorResult)
		matches bool
	}{
		{name: "matching publication", matches: true},
		{name: "empty stored attribution", matches: true, change: func(d *tracker.ChangeDetail, _ *gate.ValidatorResult) { d.Reviews[0].Validator.Attribution = "" }},
		{name: "current version changed", change: func(d *tracker.ChangeDetail, _ *gate.ValidatorResult) { d.Change.CurrentVersion = "version_new" }},
		{name: "different machine", change: func(d *tracker.ChangeDetail, _ *gate.ValidatorResult) {
			d.Reviews[0].SessionMachineID = "machine_other"
		}},
		{name: "human comment", change: func(d *tracker.ChangeDetail, _ *gate.ValidatorResult) { d.Reviews[0].Actor.Kind = "human" }},
		{name: "different decision", change: func(d *tracker.ChangeDetail, _ *gate.ValidatorResult) { d.Reviews[0].Decision = "changes_requested" }},
		{name: "different body", change: func(d *tracker.ChangeDetail, _ *gate.ValidatorResult) { d.Reviews[0].Body = "Other publication" }},
		{name: "different session", change: func(_ *tracker.ChangeDetail, r *gate.ValidatorResult) { r.SessionID++ }},
		{name: "different attribution", change: func(_ *tracker.ChangeDetail, r *gate.ValidatorResult) { r.Attribution = "independent" }},
		{name: "different source", change: func(_ *tracker.ChangeDetail, r *gate.ValidatorResult) { r.HeadSHA = strings.Repeat("d", 40) }},
		{name: "different evidence", change: func(_ *tracker.ChangeDetail, r *gate.ValidatorResult) {
			r.Commands = []gate.CommandResult{{Command: "other check"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := gate.ValidatorResult{VersionID: "version_current", SessionID: 42, Submitted: true, Verdict: "pass", HeadSHA: strings.Repeat("c", 40)}
			stored := result
			gate.NormalizeAttribution(&stored)
			detail := tracker.ChangeDetail{
				Change:  tracker.ChangeRequest{CurrentVersion: result.VersionID},
				Reviews: []tracker.ChangeReview{{ID: "review_current", VersionID: result.VersionID, SessionMachineID: "machine_current", Actor: tracker.Actor{Kind: "runner"}, Decision: "approved", Body: "Validated", Validator: &stored}},
			}
			if test.change != nil {
				test.change(&detail, &stored)
			}
			if got := recordedValidatorMatches(detail, result, "approved", "Validated", "machine_current"); got != test.matches {
				t.Fatalf("publication matched = %t, want %t", got, test.matches)
			}
		})
	}
}
