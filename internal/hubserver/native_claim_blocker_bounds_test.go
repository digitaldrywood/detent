package hubserver

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeClaimPreviewBlockerBounds(t *testing.T) {
	for _, tc := range []struct {
		name         string
		count        int
		read         bool
		crossProject bool
		scope        string
	}{
		{"read_grant", 1, true, false, "projects"},
		{"claim_grant", 1, false, false, "projects"},
		{"large_lists", 320, true, false, "projects"},
		{"hidden_project", 2, true, true, "projects"},
		{"organization_runner", 2, true, true, "organization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "Example project")
			ready := f.create(t, "Ready candidate")
			waiting := f.create(t, "Waiting candidate")
			owner := f
			if tc.crossProject {
				owner = newNativeFixture(t, f.service, f.project.OrganizationID, "Example blocker project")
			}
			seedClaimPreviewBlockers(t, f, owner.project.ID, ready, waiting, tc.count)
			operations := []string{runnerauth.Claim, runnerauth.Heartbeat}
			if tc.read {
				operations = append(operations, runnerauth.Read)
			}
			runner := prepareRunnerForScope(t, f, tc.scope, operations...)
			runner.enroll(t)
			runner.reportCapacity(t)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			request := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, MachineID: runner.binding.MachineID, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			preview := func() []claimPreviewTestItem {
				t.Helper()
				resp := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", runner.redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: request})
				requireNativeStatus(t, resp, http.StatusOK)
				var page struct{ Items []claimPreviewTestItem }
				decodeHubResponse(t, resp, &page)
				if resp.Body.Len() > 16<<10 {
					t.Errorf("preview size = %d bytes, want at most 16 KiB", resp.Body.Len())
				}
				return page.Items
			}
			page := preview()
			if len(page) != 1 || page[0].WorkItemID != ready.WorkItemID {
				t.Fatalf("expected only the ready candidate, got %d items", len(page))
			}
			checkClaimPreviewSummary(t, page[0], 0)
			claim := request
			claim.WorkItemID, claim.SessionID, claim.TTLSeconds = waiting.WorkItemID, "blocked", 90
			requireNativeError(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, claim), http.StatusConflict, "no_claimable_work")
			claim.WorkItemID, claim.SessionID = ready.WorkItemID, "ready"
			resp := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, claim)
			requireNativeStatus(t, resp, http.StatusOK)
			var lease claimPreviewTestItem
			decodeHubResponse(t, resp, &lease)
			if lease.WorkItemID != ready.WorkItemID || resp.Body.Len() > 16<<10 {
				t.Fatalf("ready lease item = %s, size = %d", lease.WorkItemID, resp.Body.Len())
			}
			checkClaimPreviewSummary(t, lease, 0)
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				project, err := w.ProjectRecord(t.Context(), f.project.ID)
				if err != nil {
					return err
				}
				project.Core.RequireDependencies = false
				_, err = w.UpdateProjectRecord(t.Context(), project)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			page = preview()
			if len(page) != 1 || page[0].WorkItemID != waiting.WorkItemID {
				t.Fatalf("expected the candidate with optional dependencies, got %d items", len(page))
			}
			if !page[0].IgnoreDependencies {
				t.Fatal("preview retained the previous dependency policy")
			}
			open := tc.count
			if tc.crossProject && tc.scope == "projects" {
				open = 0
			}
			checkClaimPreviewSummary(t, page[0], open)
			claim.WorkItemID, claim.SessionID = waiting.WorkItemID, "optional"
			resp = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, claim)
			requireNativeStatus(t, resp, http.StatusOK)
			var optional claimPreviewTestItem
			decodeHubResponse(t, resp, &optional)
			if optional.WorkItemID != waiting.WorkItemID || resp.Body.Len() > 16<<10 {
				t.Fatalf("optional lease item = %s, size = %d", optional.WorkItemID, resp.Body.Len())
			}
			checkClaimPreviewSummary(t, optional, open)
		})
	}
}

type claimPreviewTestItem struct {
	IgnoreDependencies bool                       `json:"ignore_dependencies"`
	WorkItemID         tracker.NativeWorkItemID   `json:"work_item_id"`
	Blockers           []map[string]any           `json:"blockers"`
	Dependencies       []tracker.NativeWorkItemID `json:"dependencies"`
	Ready              bool                       `json:"dependency_ready"`
	Truncated          bool                       `json:"blockers_truncated"`
}

func checkClaimPreviewSummary(t *testing.T, item claimPreviewTestItem, open int) {
	t.Helper()
	if item.Ready != (open == 0) || item.Blockers == nil || len(item.Blockers) != min(open, 1) || item.Truncated != (open > 1) || len(item.Dependencies) != 0 {
		t.Errorf("open = %d, response has ready=%t, %d blockers, %d IDs, truncated=%t", open, item.Ready, len(item.Blockers), len(item.Dependencies), item.Truncated)
		return
	}
	if open > 0 && (len(item.Blockers[0]) != 1 || item.Blockers[0]["terminal"] != false) {
		t.Error("claim blocker must contain only terminal=false")
	}
}

func seedClaimPreviewBlockers(t *testing.T, f nativeFixture, blockerProject tracker.ProjectID, ready, waiting tracker.NativeIssue, count int) {
	t.Helper()
	ctx := t.Context()
	err := f.service.config.TenantRepository.WriteTenant(ctx, f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		for i := range count {
			for j, target := range []tracker.NativeIssue{ready, waiting} {
				number := 10 + i*2 + j
				state := []string{"Done", "Todo"}[j]
				blocker, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: f.project.OrganizationID, ProjectID: blockerProject, Number: int64(number), NativeID: tracker.NativeWorkItemID(fmt.Sprintf("wi_fixture_%d", number)), Title: "Example prerequisite", Archived: true, WorkflowStateName: &state})
				if err != nil {
					return err
				}
				edge := corestore.Relationship{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: target.WorkItemID, RelatedProjectID: blockerProject, RelatedWorkItemID: blocker.NativeID, Type: "dependency", Provenance: "native"}
				if err := w.SetRelationship(ctx, edge, "add", f.service.config.Clock()); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
