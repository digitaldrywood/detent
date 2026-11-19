package nativechange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/stoptest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestStoppedGateReconciliation(t *testing.T) {
	stoptest.GateMatrix(t, func(t *testing.T) stoptest.GateFixture {
		h := newNativeChangeHub(t, true)
		issue := h.createInProgress(t, "Stop after validation")
		h.claim(t, issue.ID)
		e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
		return stoptest.GateFixture{GateExecution: e, Scope: h.native.Scope(), Transport: fixtureClient(h.native.Transport()).requests,
			Observe: func(ctx context.Context) error { return e.append(ctx, "run.observed", "", nil) },
			Release: func(reason string) error { return h.scheduler.ReleaseClaim(t.Context(), issue.ID, reason) },
			Attempt: func() (tracker.NativeAttempt, error) {
				return h.admin.NativeAttempt(t.Context(), e.claim.lease.WorkItemID, e.data.AttemptID)
			},
		}
	})
}

func TestGateDiffDelivery(t *testing.T) {
	for _, mode := range []string{"matched", "413 fallback", "unsubmitted fallback", "files differ", "runner differs", "fence differs", "generation differs"} {
		t.Run(mode, func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			item := h.createInProgress(t, "Record validation")
			h.claim(t, item.ID)
			e := h.scheduler.RunExecution(item.ID).(*nativeExecution)
			if err := e.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			head, digest := strings.Repeat("b", 40), strings.Repeat("d", 64)
			e.SetDiffSource(func(ctx context.Context) (tracker.AttemptDiffRequest, bool) {
				req, ok := nativeChangeDiff(head, "example.go")(ctx)
				req.WorkspaceDigest = digest
				return req, ok
			})
			source := tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "dirty", HeadSHA: head, WorkspaceDigest: digest, ExternalEffect: "none", EffectState: "none"}
			requests := fixtureClient(h.native.Transport()).requests
			next := requests.Load()
			var submitted []byte
			reads, posts := 0, 0
			requests.Store(executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(r.URL.Path, "/diff") {
					return next.RoundTrip(r)
				}
				if r.Method == http.MethodPost {
					posts++
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					r.Body = io.NopCloser(bytes.NewReader(raw))
					if mode == "413 fallback" && posts == 1 {
						return &http.Response{StatusCode: http.StatusRequestEntityTooLarge, Header: make(http.Header), Request: r, Body: io.NopCloser(strings.NewReader(`{"code":"diff_too_large"}`))}, nil
					}
					if submitted == nil {
						submitted = raw
						res, err := next.RoundTrip(r)
						if err != nil {
							return nil, err
						}
						res.Body.Close()
						if res.StatusCode != http.StatusAccepted {
							t.Fatalf("original diff status %d", res.StatusCode)
						}
						return nil, io.ErrUnexpectedEOF
					}
					if !bytes.Equal(submitted, raw) {
						t.Fatal("diff replay changed the submitted body")
					}
					return next.RoundTrip(r)
				}
				res, err := next.RoundTrip(r)
				if err != nil {
					return nil, err
				}
				reads++
				var diff tracker.AttemptDiff
				if err := json.NewDecoder(res.Body).Decode(&diff); err != nil {
					return nil, err
				}
				res.Body.Close()
				switch mode {
				case "413 fallback":
					if len(diff.Files) != 1 || diff.Files[0].Patch != "" || !diff.Files[0].Truncated {
						t.Fatal("Hub did not store fallback payload")
					}
				case "unsubmitted fallback":
					diff.Files = tracker.StripDiffPatches(diff.Files)
				case "files differ":
					diff.Files[0].Additions++
				case "runner differs":
					diff.Producer.RunnerID = "runner_other"
				case "fence differs":
					diff.Producer.FencingToken++
				case "generation differs":
					diff.Generation.Seq++
				}
				raw, err := json.Marshal(diff)
				res.Body = io.NopCloser(bytes.NewReader(raw))
				return res, err
			}))
			result := gate.CommandResult{Command: "make check", HeadSHA: head}
			if err := e.RecordStep(t.Context(), tracker.NativeStepReceipt{Step: "validation", Source: source, CompletedAt: time.Now().UTC(), Validation: &result}); !nativeTransportUnavailable(err) {
				t.Fatalf("original error %v", err)
			}
			var evidence []tracker.NativeEvidence
			if mode == "matched" {
				evidence = seedAttemptEvidence(t, h, e)
			}
			err := h.scheduler.ReleaseClaim(t.Context(), item.ID, runner.FinalStateOperatorStopped)
			attempt, readErr := h.admin.NativeAttempt(t.Context(), e.claim.lease.WorkItemID, e.data.AttemptID)
			if readErr != nil || reads != 1 {
				t.Fatalf("diff reads %d, attempt error %v", reads, readErr)
			}
			if !reflect.DeepEqual(attempt.Evidence, evidence) {
				t.Fatalf("released attempt evidence %v, want %v", attempt.Evidence, evidence)
			}
			if mode == "matched" || mode == "413 fallback" {
				if err != nil || attempt.ClaimReleasedAt == nil || attempt.Outcome != "interrupted" || len(attempt.Steps) != 1 || !reflect.DeepEqual(attempt.Steps[0].Validation, &result) {
					t.Fatalf("release %v, outcome %s, steps %+v, error %v", attempt.ClaimReleasedAt, attempt.Outcome, attempt.Steps, err)
				}
			} else if err != nil || attempt.ClaimReleasedAt == nil || attempt.Outcome != "interrupted" || len(attempt.Steps) != 0 {
				t.Fatalf("mismatched diff: release %v, steps %d, error %v", attempt.ClaimReleasedAt, len(attempt.Steps), err)
			}
		})
	}
}

func TestStopWithUndeliveredStart(t *testing.T) {
	h := newNativeChangeHub(t, true)
	issue := h.createInProgress(t, "Stop before start delivery")
	h.claim(t, issue.ID)
	e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
	ctx, lease := t.Context(), e.claim.lease
	requests := fixtureClient(h.native.Transport()).requests
	next := requests.Load()
	var original tracker.NativeRunEvent
	posts := 0
	requests.Store(executionRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events") {
			posts++
			if posts == 1 {
				if err := json.NewDecoder(r.Body).Decode(&original); err != nil {
					return nil, err
				}
				return nil, io.ErrUnexpectedEOF
			}
		}
		return next.RoundTrip(r)
	}))
	if err := e.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); !nativeTransportUnavailable(err) || original.Type != "run.started" || original.Data.Sequence != 1 {
		t.Fatalf("start = %s at %d, error %v", original.Type, original.Data.Sequence, err)
	}
	if _, err := h.native.ValidateLease(ctx, lease); err != nil {
		t.Fatal(err)
	}
	stopErr := h.scheduler.ReleaseClaim(ctx, issue.ID, runner.FinalStateOperatorStopped)
	if _, err := h.native.ValidateLease(ctx, lease); stopErr != nil || !nativeLeaseLost(err) || posts != 1 {
		t.Fatalf("stop error %v, lease validation %v, event POSTs %d", stopErr, err, posts)
	}
	if err := h.native.AppendEvent(ctx, lease.WorkItemID, original); !nativeLeaseLost(err) {
		t.Fatalf("late start error %v", err)
	}
	_, err := h.admin.NativeAttempt(ctx, lease.WorkItemID, original.Data.AttemptID)
	var refusal *APIError
	if !errors.As(err, &refusal) || refusal.Status != http.StatusNotFound || refusal.Code != "not_found" {
		t.Fatalf("late start attempt lookup %v", err)
	}
}

func TestStopWithCheckpointArtifacts(t *testing.T) {
	h := newNativeChangeHub(t, true)
	issue := h.createInProgress(t, "Stop after checkpoint upload")
	h.claim(t, issue.ID)
	e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
	ctx, lease := t.Context(), e.claim.lease
	if err := e.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	data := e.data
	data.Sequence++
	data.ArtifactIDs = []string{"artifact_" + strings.Repeat("a", 32)}
	data.Handoff = &tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "dirty", ExternalEffect: "none", EffectState: "none"}
	event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: data.AttemptID + ":checkpoint"}, Type: "run.checkpointed", SchemaVersion: 1, Data: data}
	if err := h.native.AppendEvent(ctx, lease.WorkItemID, event); err != nil {
		t.Fatal(err)
	}
	stopErr := h.scheduler.ReleaseClaim(ctx, issue.ID, runner.FinalStateOperatorStopped)
	attempt, err := h.admin.NativeAttempt(ctx, lease.WorkItemID, data.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if stopErr != nil || attempt.Sequence != 3 || attempt.Outcome != "interrupted" || attempt.ClaimReleasedAt == nil || !reflect.DeepEqual(attempt.Checkpoint, data.Handoff) || !reflect.DeepEqual(attempt.ArtifactIDs, data.ArtifactIDs) {
		t.Fatalf("stop error %v, sequence %d, outcome %q, released %v, checkpoint %+v, artifacts %v", stopErr, attempt.Sequence, attempt.Outcome, attempt.ClaimReleasedAt, attempt.Checkpoint, attempt.ArtifactIDs)
	}
}

func seedAttemptEvidence(t *testing.T, h *nativeChangeHub, e *nativeExecution) []tracker.NativeEvidence {
	t.Helper()
	ctx, item := t.Context(), e.claim.lease.WorkItemID
	uploader, _ := testsupport.NativeChangePublisher(t, h.config, h.project)
	metadata := attachment.Metadata{ID: "att_" + strings.Repeat("a", 32), ProjectID: string(h.project), WorkItemID: string(item), Uploader: uploader, Name: "verification.txt", ContentType: "text/plain", Size: 1, SHA256: strings.Repeat("a", 64), CreatedAt: h.clock().UTC()}
	const caption = "Verification output"
	testsupport.NativeChangeWrite(t, h.config, func(w tenantstore.Writer) error {
		return w.CreateAttachment(ctx, attachmentstore.Record{OrganizationID: h.organization, Kind: "attachment", EvidenceAttemptID: e.data.AttemptID, EvidenceCaption: caption, Metadata: metadata})
	})
	attempt, err := h.native.NativeAttempt(ctx, item, e.data.AttemptID)
	want := []tracker.NativeEvidence{{AttachmentID: metadata.ID, Caption: caption, Reference: metadata.Markdown(string(h.organization))}}
	if err != nil || !reflect.DeepEqual(attempt.Evidence, want) {
		t.Fatalf("evidence before stop %v, want %v, error %v", attempt.Evidence, want, err)
	}
	return want
}

func TestStopWithAttachedEvidence(t *testing.T) {
	h := newNativeChangeHub(t, true)
	issue := h.createInProgress(t, "Stop with attached verification")
	h.claim(t, issue.ID)
	e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
	ctx, lease := t.Context(), e.claim.lease
	if err := e.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	evidence := seedAttemptEvidence(t, h, e)
	stopErr := h.scheduler.ReleaseClaim(ctx, issue.ID, runner.FinalStateOperatorStopped)
	attempt, err := h.admin.NativeAttempt(ctx, lease.WorkItemID, e.data.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	_, leaseErr := h.native.ValidateLease(ctx, lease)
	if stopErr != nil || !nativeLeaseLost(leaseErr) || attempt.ClaimReleasedAt == nil || attempt.Sequence != 2 || attempt.Outcome != "interrupted" || !reflect.DeepEqual(attempt.Evidence, evidence) {
		t.Fatalf("stop %v, lease %v, released %v, sequence %d, outcome %q, evidence %v", stopErr, leaseErr, attempt.ClaimReleasedAt, attempt.Sequence, attempt.Outcome, attempt.Evidence)
	}
}

func TestRecoveredCheckpointSurvivesObservation(t *testing.T) {
	h := newNativeChangeHub(t, true)
	issue := h.createInProgress(t, "Continue a customer-store checkpoint")
	h.claim(t, issue.ID)
	e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
	ctx, item := t.Context(), e.claim.lease.WorkItemID
	identity := tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}
	if err := e.Start(ctx, identity); err != nil {
		t.Fatal(err)
	}
	evidence := seedAttemptEvidence(t, h, e)
	handoff := tracker.NativeCheckpoint{Resume: "manual_recovery", Storage: "customer_store", Availability: "unverified", WorktreeState: "dirty", ExternalEffect: "none", EffectState: "none"}
	data := e.data
	data.Sequence++
	data.ArtifactIDs = []string{"artifact_" + strings.Repeat("b", 32)}
	data.Handoff = &handoff
	if err := h.native.AppendEvent(ctx, item, tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: data.AttemptID + ":checkpoint"}, Type: "run.checkpointed", SchemaVersion: 1, Data: data}); err != nil {
		t.Fatal(err)
	}
	attempt, err := h.native.NativeAttempt(ctx, item, data.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	view, ok := e.scheduler.host.ExecutionClaim(issue.ID)
	if !ok {
		t.Fatal("missing live claim")
	}
	view.Recovery.Attempts = []tracker.NativeAttempt{attempt}
	recovered := NewExecution(e.scheduler.host, view).(*nativeExecution)
	if err := recovered.Start(ctx, identity); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	handoff.Resume = "fresh_checkout"
	if err := recovered.Checkpoint(ctx, handoff); err != nil {
		t.Fatalf("checkpoint after reconnect: %v", err)
	}
	stored, err := h.admin.NativeAttempt(ctx, item, data.AttemptID)
	if err != nil || stored.Sequence != 4 || !reflect.DeepEqual(stored.Checkpoint, &handoff) || !reflect.DeepEqual(stored.ArtifactIDs, data.ArtifactIDs) || !reflect.DeepEqual(stored.Evidence, evidence) {
		t.Fatalf("sequence %d, checkpoint %+v, artifacts %v, evidence %v, error %v", stored.Sequence, stored.Checkpoint, stored.ArtifactIDs, stored.Evidence, err)
	}
}

func TestExecutionEventFields(t *testing.T) {
	common := tracker.NativeRunData{
		AttemptID: "attempt_example", RunID: "run_example", LeaseID: "lease_example", PolicyID: "policy_example",
		FencingToken: 3, Sequence: 5, MachineID: "machine_example", RunnerID: "runner_example", SessionID: "session_example",
		Identity: &tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"},
		Steps:    []tracker.NativeStepReceipt{{Step: "validation"}}, ProcessUsage: &procgroup.Usage{PeakMemoryBytes: 64},
		Runtime: &tracker.NativeRuntimeObservation{Phase: "merging", Landing: &tracker.NativeLandingReceipt{RefusalKind: "conflict"}},
	}
	current := common
	observation, landing := *common.Runtime, *common.Runtime.Landing
	landing.FromState, landing.TargetState = "Merging", "Rework"
	observation.Landing = &landing
	current.Runtime = &observation
	current.Evidence = []tracker.NativeEvidence{{Caption: "attached"}}
	current.Handoff = &tracker.NativeCheckpoint{Storage: "local_only"}
	current.ArtifactIDs = []string{"artifact_example"}
	current.Usage = []tracker.NativeUsage{{Provider: "example"}}
	current.Finalization, current.Disposition = &tracker.NativeFinalization{}, &tracker.NativeDisposition{}
	current.TerminalFailure, current.CompletionBody = &tracker.NativeTerminalFailure{}, "finished"
	for _, tc := range []struct {
		kind, previousOutcome                string
		usage, checkpoint, terminal, failure bool
	}{
		{kind: "run.started", previousOutcome: "failed"},
		{kind: "run.observed", previousOutcome: "interrupted", usage: true},
		{kind: "run.checkpointed", previousOutcome: "interrupted", usage: true, checkpoint: true},
		{kind: "run.finished", previousOutcome: "interrupted", usage: true, terminal: true, failure: true},
		{kind: "run.finished", previousOutcome: "succeeded", usage: true, terminal: true},
	} {
		t.Run(tc.kind+"/"+tc.previousOutcome, func(t *testing.T) {
			input, want := current, common
			input.Outcome = tc.previousOutcome
			if tc.usage {
				want.Usage = current.Usage
			}
			if tc.checkpoint {
				want.Handoff, want.ArtifactIDs = current.Handoff, current.ArtifactIDs
			}
			if tc.terminal {
				want.Outcome, want.Finalization = tc.previousOutcome, current.Finalization
				want.Disposition, want.CompletionBody = current.Disposition, current.CompletionBody
			}
			if tc.failure {
				want.TerminalFailure = current.TerminalFailure
			}
			if got := executionEventData(tc.kind, input); !reflect.DeepEqual(got, want) {
				t.Fatalf("event data %+v, want %+v", got, want)
			}
			if input.Runtime.Landing.FromState != "Merging" || input.Runtime.Landing.TargetState != "Rework" {
				t.Fatal("event changed the hydrated landing transition")
			}
		})
	}
}
