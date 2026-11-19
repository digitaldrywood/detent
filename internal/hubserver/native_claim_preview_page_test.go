package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeClaimPreviewPageBytes(t *testing.T) {
	f := newNativeFixture(t, nil, "", "Example project")
	runner := prepareRunnerForScope(t, f, "projects", runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	runner.enroll(t)
	runner.reportCapacity(t)
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	want := map[tracker.NativeWorkItemID]tracker.NativeIssue{}
	var largeID tracker.NativeWorkItemID
	for index := range 6 {
		issue := f.create(t, fmt.Sprintf("Example candidate %d", index))
		preamble := strings.Repeat("ordinary description ", 12000)
		if index == 2 {
			preamble = strings.Repeat("\"", 250<<10)
		} else if index == 3 {
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
		storeClaimPreviewRevision(t, f, issue, historyTestJSON(t, issue))
		want[issue.WorkItemID] = issue
	}

	request := tracker.NativeCapacityPreview{NativeClaim: tracker.NativeClaim{PolicyID: hubTestPolicy().ID, MachineID: runner.binding.MachineID}}
	seen := map[tracker.NativeWorkItemID]bool{}
	for pageNumber := 1; ; pageNumber++ {
		resp := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview?pretty", runner.redemption.Credential, request)
		requireNativeStatus(t, resp, http.StatusOK)
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
	resp := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, claim)
	requireNativeStatus(t, resp, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, resp, &lease)
	if lease.WorkItemID != largeID || lease.FencingToken == 0 || resp.Body.Len() > 1<<20 {
		t.Fatalf("large candidate lease item=%s bytes=%d", lease.WorkItemID, resp.Body.Len())
	}
}
