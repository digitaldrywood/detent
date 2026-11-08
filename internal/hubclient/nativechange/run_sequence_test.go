package nativechange

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
