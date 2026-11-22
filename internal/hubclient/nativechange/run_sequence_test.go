package nativechange

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSSHCheckpointRefusesCachedDiffAfterHeadMoves(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(map[bool]string{false: "checkpoint diff", true: "already stored finish diff"}[staged], func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, "Use the live source head")
			h.claim(t, issue.ID)
			e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			ctx := t.Context()
			base, final, moved := nativeChangeFixtureHead("a"), nativeChangeFixtureHead("b"), nativeChangeFixtureHead("c")
			source := nativeChangeSourceFixture(ctx, t, base, final)
			worker, closePeers := nativeSSHExecution(t, ctx, e, t.TempDir())
			worker.(runner.RepositoryExecution).SetRepository(nativeChangeRepository)
			worker.(runner.ChangeSourceExecution).SetChangeSource(func(context.Context, string, string) (tracker.ChangeSourceCapture, error) {
				return source, nil
			})
			var headMoved atomic.Bool
			finalDiff := nativeChangeDiff(final, "README.md")
			worker.(runner.DiffExecution).SetDiffSource(func(ctx context.Context) (tracker.AttemptDiffRequest, bool) {
				if headMoved.Load() {
					return tracker.AttemptDiffRequest{HeadSHA: moved}, false
				}
				return finalDiff(ctx)
			})
			if err := worker.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			checkpoint := tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: final, ExternalEffect: "none", EffectState: "none"}
			if err := worker.Checkpoint(ctx, checkpoint); err != nil {
				t.Fatal(err)
			}
			if err := e.RecordSourceValidation(ctx, gate.CommandResult{Command: "go test ./internal/example", HeadSHA: final, TreeSHA: strings.Repeat("b", 40), DurationNS: 1, StartedAt: time.Now().UTC().Add(-time.Second), FinishedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			if staged {
				if err := e.postDiff(ctx, e.data.Sequence+1); err != nil {
					t.Fatal(err)
				}
			}
			if e.lastDiff == nil || e.lastDiff.HeadSHA != final || e.retainedSource == nil || e.retainedSource.Source.HeadSHA != final {
				t.Fatal("checkpoint did not retain the finalized diff and bundle")
			}
			stored := e.storedSeq
			assertUnpublished := func() {
				t.Helper()
				detail, err := h.admin.CurrentChange(ctx, tracker.NativeWorkItemID(issue.ID))
				if err != nil || detail.Change.CurrentVersion != "" || e.storedSeq != stored {
					t.Fatalf("moved head reused cached source: sequence=%d (was %d), version=%s, error=%v", e.storedSeq, stored, detail.Change.CurrentVersion, err)
				}
			}
			headMoved.Store(true)
			if err := e.PrepareFinish(ctx, "succeeded", "", nil); err != nil {
				t.Fatal(err)
			}
			assertUnpublished()
			if change := e.NativeChange(); change == nil || change.VersionError == "" || e.lastDiff != nil {
				t.Fatalf("the moved checkout did not invalidate the cached diff: change=%+v diff=%+v", change, e.lastDiff)
			}
			if err := worker.Checkpoint(ctx, checkpoint); err != nil {
				t.Fatalf("a moved diff head blocked checkpoint publication: %v", err)
			}
			closePeers()
			if err := e.postDiff(ctx, stored+1); err == nil {
				t.Fatal("disconnect allowed the superseded diff to be uploaded")
			}
			assertUnpublished()
		})
	}
}

func TestCheckpointDoesNotCaptureAnOlderDiffHead(t *testing.T) {
	h := newNativeChangeHub(t, true)
	issue := h.createInProgress(t, "Resume after publishing the next phase")
	h.claim(t, issue.ID)
	e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
	if err := e.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleRework, Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	old, current := nativeChangeFixtureHead("c"), nativeChangeFixtureHead("d")
	e.SetDiffSource(nativeChangeDiff(old, "README.md"))
	checkpoint := tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "dirty", HeadSHA: old, WorkspaceDigest: strings.Repeat("a", 64), ExternalEffect: "none", EffectState: "none"}
	if err := e.Checkpoint(t.Context(), checkpoint); err != nil {
		t.Fatal(err)
	}
	if e.lastDiff == nil || e.lastDiff.HeadSHA != old {
		t.Fatal("the first checkpoint did not cache the earlier diff")
	}
	e.SetDiffSource(func(context.Context) (tracker.AttemptDiffRequest, bool) {
		return tracker.AttemptDiffRequest{}, false
	})
	var captured []string
	e.SetChangeSource(func(_ context.Context, _, head string) (tracker.ChangeSourceCapture, error) {
		captured = append(captured, head)
		return tracker.ChangeSourceCapture{}, io.ErrUnexpectedEOF
	})
	checkpoint.HeadSHA, checkpoint.WorkspaceDigest, checkpoint.WorktreeState = current, strings.Repeat("b", 64), "unpushed"
	for range 2 {
		if err := e.Checkpoint(t.Context(), checkpoint); err != nil {
			t.Fatalf("checkpoint should survive an unavailable diagnostic diff: %v", err)
		}
	}
	if len(captured) != 0 || e.data.Handoff.HeadSHA != current || e.storedSeq == e.data.Sequence {
		t.Fatalf("stale diff was used for the new checkpoint: captured=%v checkpoint=%s diff sequence=%d run sequence=%d", captured, e.data.Handoff.HeadSHA, e.storedSeq, e.data.Sequence)
	}
}

func TestNativePublicationRetainsSourceAfterRunProgress(t *testing.T) {
	for _, progress := range []bool{false, true} {
		t.Run(map[bool]string{false: "gate passed", true: "progress after final diff"}[progress], func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, "Publish validated source")
			h.claim(t, issue.ID)
			e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			head := nativeChangeFixtureHead("c")
			e.SetDiffSource(nativeChangeDiff(head, "README.md"))
			e.SetRepository(nativeChangeRepository)
			if err := e.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			if err := e.Checkpoint(t.Context(), tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: "unpushed", HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil {
				t.Fatal(err)
			}
			if err := e.RecordSourceValidation(t.Context(), gate.CommandResult{Command: "go test ./internal/example", HeadSHA: head, TreeSHA: strings.Repeat("b", 40), DurationNS: 1, StartedAt: time.Now().UTC().Add(-time.Second), FinishedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			original := fixtureClient(h.native.Transport()).requests.Load()
			fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if progress && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/versions") {
					for range 2 {
						if err := e.append(request.Context(), "run.observed", "", nil, true); err != nil {
							t.Fatal(err)
						}
					}
				}
				return original.RoundTrip(request)
			}))
			if err := e.PrepareFinish(t.Context(), "succeeded", "", nil); err != nil {
				t.Fatal(err)
			}
			change := e.NativeChange()
			if change == nil || change.VersionID == "" || change.VersionError != "" {
				t.Fatalf("gate-passed source was not published: %+v", change)
			}
			detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), change.ChangeID)
			if err != nil || len(detail.Versions) != 1 || detail.Versions[0].HeadSHA != head || detail.Versions[0].Validation == nil {
				t.Fatalf("published source = %+v, error = %v", detail.Versions, err)
			}
		})
	}
}

func TestDeferredNativePublisherDoesNotReuseIdentity(t *testing.T) {
	for _, reason := range []string{"dispatch_deferred", "provider_capacity_unavailable"} {
		t.Run(reason, func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, "Resume Change publication")
			h.claim(t, issue.ID)
			e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			identity := tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}
			if err := e.Start(t.Context(), identity); err != nil {
				t.Fatal(err)
			}
			e.SetDiffSource(nativeChangeDiff(nativeChangeFixtureHead("c"), "README.md"))
			e.SetRepository(nativeChangeRepository)
			checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: "unpushed", ExternalEffect: "none", EffectState: "none"}
			if err := e.Checkpoint(t.Context(), checkpoint); err != nil {
				t.Fatal(err)
			}
			original := fixtureClient(h.native.Transport()).requests.Load()
			fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/versions") {
					return &http.Response{StatusCode: http.StatusConflict, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"run_sequence_conflict","message":"run sequence conflict"}`)), Request: request}, nil
				}
				return original.RoundTrip(request)
			}))
			if err := e.Finish(t.Context(), "succeeded"); err != nil {
				t.Fatal(err)
			}
			change := e.NativeChange()
			if change == nil || change.ChangeID == "" || change.VersionID != "" || e.data.Outcome != "failed" {
				t.Fatalf("publication refusal lost source: %+v", change)
			}
			changeID := change.ChangeID
			fixtureClient(h.native.Transport()).requests.Store(original)
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "released"); err != nil {
				t.Fatal(err)
			}
			h.claim(t, issue.ID)
			e = h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			if err := e.Start(t.Context(), identity); err != nil {
				t.Fatal(err)
			}
			attempt := e.data.AttemptID
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, reason); err != nil {
				t.Fatal(err)
			}
			recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil || len(recovery.Attempts) != 2 || recovery.Attempts[1].Status != "cancelled" || recovery.Attempts[1].ClaimReleasedAt == nil {
				t.Fatalf("deferred identity retained: %+v, %v", recovery.Attempts, err)
			}
			h.claim(t, issue.ID)
			e = h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			identity.ReasoningEffort = "high"
			if err := e.Start(t.Context(), identity); err != nil {
				t.Fatalf("retry could not select its identity: %v", err)
			}
			if e.data.AttemptID == attempt {
				t.Fatal("retry reused a deferred attempt")
			}
			retryBase, retryHead := nativeChangeFixtureHead("b"), nativeChangeFixtureHead("d")
			retryDiff := nativeChangeDiff(retryHead, "README.md")
			e.SetDiffSource(func(ctx context.Context) (tracker.AttemptDiffRequest, bool) {
				diff, ok := retryDiff(ctx)
				diff.BaseSHA = retryBase
				return diff, ok
			})
			e.SetRepository(nativeChangeRepository)
			if err := e.Checkpoint(t.Context(), tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: "unpushed", ExternalEffect: "none", EffectState: "none"}); err != nil {
				t.Fatal(err)
			}
			if err := e.RecordSourceValidation(t.Context(), gate.CommandResult{Command: "go test ./internal/example", HeadSHA: retryHead, TreeSHA: strings.Repeat("b", 40), DurationNS: 1, StartedAt: time.Now().UTC().Add(-time.Second), FinishedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			if err := e.PrepareFinish(t.Context(), "succeeded", "", nil); err != nil {
				t.Fatal(err)
			}
			if change := e.NativeChange(); change == nil || change.ChangeID != changeID || change.VersionID == "" || change.VersionError != "" {
				t.Fatalf("retry did not publish: %+v", change)
			}
			detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), changeID)
			if err != nil || len(detail.Versions) != 1 || detail.Versions[0].BaseSHA != retryBase || detail.Versions[0].HeadSHA != retryHead || detail.Versions[0].Validation == nil {
				t.Fatalf("retry did not retain its validated source: versions=%+v error=%v", detail.Versions, err)
			}
		})
	}
}
