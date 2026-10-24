package hubclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
				name    string
				mutate  func(*tracker.ChangeDetail)
				prepare func(*nativeExecution)
			}{
				{name: "head", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].HeadSHA = nativeChangeFixtureHead("d") }},
				{name: "base", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].BaseSHA = nativeChangeFixtureHead("b") }},
				{name: "policy", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].PolicyID = "policy_other" }},
				{name: "frozen policy", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].Policy.ID = "policy_other" }},
				{name: "item", mutate: func(d *tracker.ChangeDetail) { d.Change.WorkItemID = "wi_other" }},
				{name: "project", mutate: func(d *tracker.ChangeDetail) { d.Change.ProjectID = "prj_other" }},
				{name: "attempt", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].AttemptID = "attempt_other" }},
				{name: "run", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].RunID = "run_other" }},
				{name: "unrecovered source", prepare: func(e *nativeExecution) { e.recoveredSource = nil }},
				{name: "unrecorded source owner", prepare: func(e *nativeExecution) { e.claim.recovery.Attempts = nil }},
			} {
				t.Run(test.name, func(t *testing.T) {
					source, recovery := second.recoveredSource, second.claim.recovery
					t.Cleanup(func() { second.recoveredSource, second.claim.recovery = source, recovery })
					if test.prepare != nil {
						test.prepare(second)
					}
					transport := h.native.client.httpClient.Transport
					h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
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
						if test.mutate != nil {
							test.mutate(page.Detail)
						}
						body, err := json.Marshal(page)
						if err != nil {
							return nil, err
						}
						response.Body = io.NopCloser(strings.NewReader(string(body)))
						return response, nil
					})
					t.Cleanup(func() { h.native.client.httpClient.Transport = transport })
					if err := second.PrepareReview(t.Context(), ref); err == nil {
						t.Fatalf("mismatched %s accepted: %v", test.name, err)
					}
				})
			}
		})
	}
}
