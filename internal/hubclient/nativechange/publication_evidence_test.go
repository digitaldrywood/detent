package nativechange

import (
	"context"
	"encoding/json"
	"github.com/digitaldrywood/detent/internal/attachment"
	"io"

	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPublicationRetainsAttemptEvidence(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "creation"
		if existing {
			name = "continuation"
		}
		t.Run(name, func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, name)
			h.claim(t, issue.ID)
			e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			e.SetRepository(nativeChangeRepository)
			e.SetChangeSource(func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
				return nativeChangeSourceFixture(ctx, t, base, head), nil
			})
			if err := e.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			head := nativeChangeFixtureHead("c")
			e.SetDiffSource(nativeChangeDiff(head, "README.md"))
			if err := e.Checkpoint(t.Context(), tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil {
				t.Fatal(err)
			}
			diff, _ := nativeChangeDiff(head, "README.md")(t.Context())
			var changeID string
			if existing {
				c, err := h.admin.CreateChange(t.Context(), tracker.NativeWorkItemID(issue.ID), tracker.CreateChange{Mutation: api.NativeMutationKey(), Title: "Human title", Body: "Human description"})
				if err != nil {
					t.Fatal(err)
				}
				changeID = c.ID
				if _, _, err := e.publishChangeVersion(t.Context(), changeID, diff); err != nil {
					t.Fatal(err)
				}
			}

			caption := "Captured at source " + head
			evidence := attachment.Metadata{ID: "att_0123456789abcdef0123456789abcdef", ProjectID: string(h.project), Name: "screen.png", ContentType: "image/png", Size: 100, SHA256: strings.Repeat("f", 64), Width: 2, Height: 2}
			attempt, err := h.native.NativeAttempt(t.Context(), tracker.NativeWorkItemID(issue.ID), e.data.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			attempt.Evidence = []tracker.NativeEvidence{{AttachmentID: evidence.ID, Caption: caption, Reference: evidence.Markdown(string(h.organization))}}
			transport := fixtureClient(h.native.Transport()).requests.Load()
			fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				var value any
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/attempts/"+e.data.AttemptID) {
					value = attempt
				}
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/attachments/"+evidence.ID+"/metadata") {
					value = evidence
				}
				if value == nil {
					return transport.RoundTrip(r)
				}
				raw, err := json.Marshal(value)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, err
			}))
			defer fixtureClient(h.native.Transport()).requests.Store(transport)
			e.preparedMessage = "Replace the old command; request cost is bounded by supplied evidence."
			opened, err := e.openChange(t.Context(), diff)
			if err != nil {
				t.Fatal(err)
			}
			if existing && opened != changeID {
				t.Fatal("continuation replaced Change")
			}
			version, _, err := e.publishChangeVersion(t.Context(), opened, diff)
			if err != nil {
				t.Fatal(err)
			}
			if again, _, err := e.publishChangeVersion(t.Context(), opened, diff); err != nil || again != version {
				t.Fatalf("replay: %s %v", again, err)
			}
			detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), opened)
			if err != nil {
				t.Fatal(err)
			}
			current := detail.Versions[len(detail.Versions)-1]
			if len(current.Artifacts) != 1 || current.Artifacts[0].URI != "attachment:"+evidence.ID || current.Artifacts[0].SHA256 != evidence.SHA256 || current.Artifacts[0].Availability != "available" {
				t.Fatalf("retained screenshot missing: %+v", current.Artifacts)
			}
			if len(detail.Discussion) != 1 || detail.Discussion[0].VersionID != version || !strings.Contains(detail.Discussion[0].Body, caption) || !strings.Contains(detail.Discussion[0].Body, e.preparedMessage) || !strings.Contains(detail.Discussion[0].Body, e.data.AttemptID) {
				t.Fatalf("outcome and capture provenance missing or duplicated: %+v", detail.Discussion)
			}
			if existing && detail.Change.Body != "Human description" {
				t.Fatal("human description replaced")
			}
		})
	}
}
