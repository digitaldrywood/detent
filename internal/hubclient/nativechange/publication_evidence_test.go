package nativechange

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPublicationRetainsAttemptEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, role string
		existing   bool
	}{
		{name: "creation", role: runner.RoleCode},
		{name: "same head continuation", role: runner.RoleRework, existing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, tc.name)
			item := tracker.NativeWorkItemID(issue.ID)
			h.claim(t, issue.ID)
			e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			e.SetRepository(nativeChangeRepository)
			e.SetChangeSource(func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
				return nativeChangeSourceFixture(ctx, t, base, head), nil
			})
			if err := e.Start(ctx, tracker.NativeExecutionIdentity{Role: tc.role, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			head := nativeChangeFixtureHead("c")
			diffSource := nativeChangeDiff(head, "README.md")
			e.SetDiffSource(diffSource)
			if err := e.Checkpoint(ctx, tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil {
				t.Fatal(err)
			}
			diff, _ := diffSource(ctx)
			caption := "Screenshot captured from " + nativeChangeFixtureHead("d")
			seedEvidence := func(id, workItem, attempt string, deletedAt *time.Time) attachment.Metadata {
				t.Helper()
				evidence := attachment.Metadata{ID: id, ProjectID: string(h.project), WorkItemID: workItem, Name: "screen.png", ContentType: "image/png", Size: 100, SHA256: strings.Repeat("f", 64), Width: 2, Height: 2, CreatedAt: time.Now().UTC(), DeletedAt: deletedAt}
				testsupport.NativeChangeWrite(t, h.config, func(w tenantstore.Writer) error {
					identity, err := w.RunnerForLease(ctx, string(e.claim.lease.ID))
					if err != nil {
						return err
					}
					evidence.Uploader = identity.TokenID
					return w.CreateAttachment(ctx, attachmentstore.Record{OrganizationID: h.organization, Metadata: evidence, Kind: "attachment", EvidenceAttemptID: attempt, EvidenceCaption: caption})
				})
				return evidence
			}
			evidence := seedEvidence("att_0123456789abcdef0123456789abcdef", issue.ID, e.data.AttemptID, nil)
			wantEvidence := []attachment.Metadata{evidence}
			var original tracker.ChangeRequest
			var originalVersion string
			if tc.existing {
				var err error
				original, err = h.admin.CreateChange(ctx, item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Human title", Body: "Human description"})
				if err != nil {
					t.Fatal(err)
				}
				e.preparedOutcome = "succeeded"
				originalVersion, _, err = e.publishChangeVersion(ctx, original.ID, diff)
				if err != nil {
					t.Fatal(err)
				}
				wantEvidence = append(wantEvidence,
					seedEvidence("att_1123456789abcdef0123456789abcdef", issue.ID, e.data.AttemptID, nil),
					seedEvidence("att_2123456789abcdef0123456789abcdef", issue.ID, e.data.AttemptID, new(time.Now().UTC())),
				)
			}

			var published tracker.PublishChangeVersion
			transport := fixtureClient(h.native.Transport()).requests.Load()
			fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/versions") {
					body, err := r.GetBody()
					if err != nil {
						return nil, err
					}
					defer body.Close()
					if err := json.NewDecoder(body).Decode(&published); err != nil {
						return nil, err
					}
				}
				return transport.RoundTrip(r)
			}))
			const rationale = "Replace the old command. The published evidence covers the same UI sources."
			status := "status: complete"
			if tc.existing {
				status = "status: blocked\nreason_code: host_publication_pending"
			}
			if err := e.PrepareFinish(ctx, "succeeded", rationale+"\n\n```detent-status\nschema: 1\n"+status+"\nblockers: []\nhuman_action: null\n```", nil); err != nil {
				t.Fatal(err)
			}
			fixtureClient(h.native.Transport()).requests.Store(transport)
			change := e.NativeChange()
			if change == nil || change.VersionID == "" || change.VersionError != "" {
				t.Fatalf("completion did not publish: %+v", change)
			}
			if tc.existing && (change.ChangeID != original.ID || change.VersionID == originalVersion) {
				t.Fatalf("continuation did not publish a version on the existing Change: %+v", change)
			}
			detail, err := h.admin.Change(ctx, item, change.ChangeID)
			if err != nil {
				t.Fatal(err)
			}
			var version tracker.ChangeVersion
			for _, candidate := range detail.Versions {
				if candidate.ID == detail.Change.CurrentVersion {
					version = candidate
				}
			}
			if len(version.Artifacts) != len(wantEvidence) {
				t.Fatalf("retained screenshots missing or duplicated: %+v", version.Artifacts)
			}
			if len(version.EvidenceCaptures) != len(wantEvidence) {
				t.Fatalf("retained screenshot provenance missing or duplicated: %+v", version.EvidenceCaptures)
			}
			for i, meta := range wantEvidence {
				got := version.Artifacts[i]
				availability := "available"
				if meta.DeletedAt != nil {
					availability = "missing"
				}
				if got.URI != "attachment:"+meta.ID || got.SHA256 != meta.SHA256 || got.Availability != availability {
					t.Fatalf("screenshot lost identity or availability: %+v", got)
				}
				capture := version.EvidenceCaptures[i]
				if capture.AttachmentID != meta.ID || capture.AttemptID != e.data.AttemptID || capture.Caption != caption {
					t.Fatalf("screenshot lost capture provenance: %+v", capture)
				}
			}
			if !strings.Contains(detail.Change.Body, rationale) || strings.Contains(detail.Change.Body, "detent-status") {
				t.Fatalf("Change description lost the outcome: %q", detail.Change.Body)
			}
			if tc.existing && !strings.HasPrefix(detail.Change.Body, original.Body+"\n\n") {
				t.Fatalf("human description replaced: %q", detail.Change.Body)
			}
			if tc.existing {
				for _, old := range detail.Versions {
					if old.ID == originalVersion && (len(old.Artifacts) != 1 || old.Outcome != "" || old.Artifacts[0].URI != "attachment:"+evidence.ID) {
						t.Fatalf("publication edited old version: %+v", old)
					}
				}
			}
			bounded, err := h.native.RuntimeChange(ctx, item, change.ChangeID)
			if err != nil || bounded.Change.Body != detail.Change.Body {
				t.Fatalf("bounded Change read lost the description: %q, %v", bounded.Change.Body, err)
			}
			current := slices.IndexFunc(bounded.Versions, func(v tracker.ChangeVersion) bool { return v.ID == version.ID })
			if current < 0 || !slices.Equal(bounded.Versions[current].EvidenceCaptures, version.EvidenceCaptures) {
				t.Fatalf("bounded Change read lost capture provenance: %+v", bounded.Versions)
			}
			replay, err := h.native.PublishChangeVersion(ctx, item, change.ChangeID, published)
			if err != nil || replay.ID != version.ID || !slices.Equal(replay.EvidenceCaptures, version.EvidenceCaptures) {
				t.Fatalf("publication replay = %s, %v", replay.ID, err)
			}
			again, _, err := e.publishChangeVersion(ctx, change.ChangeID, diff)
			if err != nil || again != version.ID {
				t.Fatalf("completion replay = %s, %v", again, err)
			}

			if tc.existing {
				otherIssue := h.createInProgress(t, "Evidence belongs to another work item")
				other := seedEvidence("att_3123456789abcdef0123456789abcdef", otherIssue.ID, "", nil)
				for _, refusal := range []struct {
					name   string
					client *api.NativeClient
					edit   func(*tracker.PublishChangeVersion)
					status int
				}{
					{"foreign attachment", h.native, func(q *tracker.PublishChangeVersion) {
						q.EvidenceAttachmentIDs = []string{other.ID}
					}, http.StatusUnprocessableEntity},
					{"foreign attachment linked in outcome", h.native, func(q *tracker.PublishChangeVersion) {
						q.EvidenceAttachmentIDs = nil
						q.Outcome = other.Markdown(string(h.organization))
					}, http.StatusUnprocessableEntity},
					{"operator publication cannot claim an implementation outcome", h.admin, func(q *tracker.PublishChangeVersion) {}, http.StatusUnprocessableEntity},
					{"stale fence", h.native, func(q *tracker.PublishChangeVersion) {
						q.FencingToken++
					}, http.StatusConflict},
				} {
					t.Run(refusal.name, func(t *testing.T) {
						request := published
						request.ExpectedVersionID = version.ID
						request.IdempotencyKey = nativeMutationKey().IdempotencyKey
						refusal.edit(&request)
						_, err := refusal.client.PublishChangeVersion(t.Context(), item, change.ChangeID, request)
						var apiErr *api.APIError
						if !errors.As(err, &apiErr) || apiErr.Status != refusal.status {
							t.Fatalf("publication refusal = %v, want HTTP %d", err, refusal.status)
						}
					})
				}
			}

		})
	}
}
