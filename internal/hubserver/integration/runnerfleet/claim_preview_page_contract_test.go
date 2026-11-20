package runnerfleet_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeClaimPreviewPageBytes(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	runner := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	runner.Enroll(t)
	runner.ReportCapacity(t)
	want := map[tracker.NativeWorkItemID]tracker.NativeIssue{}
	var largeID tracker.NativeWorkItemID
	for index := range 6 {
		issue := f.Create(t, fmt.Sprintf("Example candidate %d", index))
		preamble := strings.Repeat("ordinary description ", 12000)
		switch index {
		case 2:
			preamble = strings.Repeat("\"", 250<<10)
		case 3:
			preamble = strings.Repeat("<>&", 80000)
		}
		issue.Body = preamble + "\n\n```detent-agent\nschema: 1\nmodel: example-model\n```\n\n" + issue.Body
		if index == 2 {
			largeID = issue.WorkItemID
			const source = "https://github.com/example-org/example-project/issues/7"
			issue.LinkedSource = &tracker.LinkedIssueSource{URL: source, Status: "complete", Snapshot: &tracker.GitHubIssueSnapshot{
				URL: source, Title: issue.Title, Body: issue.Body, Provenance: tracker.Provenance{Provider: "github", ExternalID: "7", AuthorID: "example"},
			}}
			issue.Workpad = &tracker.NativeWorkpad{Body: strings.Repeat("Review notes. ", 3000)}
		}
		issue.Revision++
		seedClaimPreviewRevision(t, f, issue)
		want[issue.WorkItemID] = issue
	}

	request := tracker.NativeCapacityPreview{NativeClaim: tracker.NativeClaim{PolicyID: hubfixture.Policy().ID, MachineID: runner.Binding.MachineID}}
	seen := map[tracker.NativeWorkItemID]bool{}
	for pageNumber := 1; ; pageNumber++ {
		resp := f.Hub.Request(t, http.MethodPost, f.Base+"/claims/preview?pretty", runner.Redemption.Credential, request)
		hubfixture.RequireStatus(t, resp, http.StatusOK)
		if resp.Body.Len() > 1<<20 {
			t.Fatalf("page %d has %d bytes, exceeds installed runner limit", pageNumber, resp.Body.Len())
		}
		var page tracker.NativeCapacityPage
		if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 0 {
			t.Fatalf("page %d contains no candidates", pageNumber)
		}
		for _, got := range page.Items {
			expected, ok := want[got.WorkItemID]
			if !ok || seen[got.WorkItemID] {
				t.Fatalf("unexpected or repeated candidate %s", got.WorkItemID)
			}
			if got.Workpad != nil {
				t.Fatalf("preview exposed workpad for %s", got.WorkItemID)
			}
			if !reflect.DeepEqual(got.IssueContract, expected.IssueContract) || got.Actor.PrincipalID != expected.Actor.PrincipalID || got.Body != expected.Body || got.Revision != expected.Revision || got.Title != expected.Title || got.State != expected.State || !got.DependencyReady {
				t.Fatalf("candidate %s lost scheduling inputs", got.WorkItemID)
			}
			seen[got.WorkItemID] = true
		}
		if page.Next == 0 {
			if len(seen) != len(want) || pageNumber < 2 {
				t.Fatalf("saw %d of %d candidates in %d pages", len(seen), len(want), pageNumber)
			}
			break
		}
		if page.Next == request.After || pageNumber >= len(want) {
			t.Fatalf("continuation stopped progressing on page %d", pageNumber)
		}
		request.After = page.Next
	}
	claim := request.NativeClaim
	claim.WorkItemID, claim.SessionID, claim.TTLSeconds = largeID, "large-candidate", 90
	claim.ProtocolMajor, claim.Capabilities = tracker.NativeProtocolMajor, []string{"native_issues", "scoped_collaboration"}
	resp := f.Hub.Request(t, http.MethodPost, f.Base+"/claims", runner.Redemption.Credential, claim)
	hubfixture.RequireStatus(t, resp, http.StatusOK)
	var lease tracker.NativeLease
	hubfixture.Decode(t, resp, &lease)
	if lease.WorkItemID != largeID || lease.FencingToken == 0 || resp.Body.Len() > 1<<20 {
		t.Fatalf("large candidate lease item=%s bytes=%d", lease.WorkItemID, resp.Body.Len())
	}
}

func seedClaimPreviewRevision(t *testing.T, f hubfixture.Native, issue tracker.NativeIssue) {
	t.Helper()
	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	version, err := eventstore.NewContent(f.Hub.Config.RunDetails).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{
		OrganizationID: f.Project.OrganizationID, ProjectID: f.Project.ID, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision),
	}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Hub.Repository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		row, err := w.WorkItem(t.Context(), f.Project.ID, issue.WorkItemID)
		if err != nil {
			return err
		}
		if _, err := w.UpdateWorkItem(t.Context(), row); err != nil {
			return err
		}
		_, err = w.CreateCollaborationVersion(t.Context(), version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
