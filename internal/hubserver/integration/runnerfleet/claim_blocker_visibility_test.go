package runnerfleet_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeClaimPreviewBlockerBounds(t *testing.T) {
	h := hubfixture.Open(t, hubfixture.Config{})
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
			f := hubfixture.NewNative(t, h, h.CreateOrganization(t, tc.name), "Example project")
			ready := f.Create(t, "Ready candidate")
			waiting := f.Create(t, "Waiting candidate")
			owner := f
			if tc.crossProject {
				owner = hubfixture.NewNative(t, h, f.Project.OrganizationID, "Example blocker project")
			}
			seedClaimPreviewBlockers(t, f, owner.Project.ID, ready, waiting, tc.count)
			operations := []string{runnerauth.Claim, runnerauth.Heartbeat}
			if tc.read {
				operations = append(operations, runnerauth.Read)
			}
			runner := hubfixture.PrepareRunnerForScope(t, f, tc.scope, operations...)
			runner.Enroll(t)
			runner.ReportCapacity(t)

			request := tracker.NativeClaim{PolicyID: hubfixture.Policy().ID, MachineID: runner.Binding.MachineID, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			preview := func() []claimPreviewTestItem {
				t.Helper()
				resp := f.Hub.Request(t, http.MethodPost, f.Base+"/claims/preview", runner.Redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: request})
				hubfixture.RequireStatus(t, resp, http.StatusOK)
				var page struct{ Items []claimPreviewTestItem }
				hubfixture.Decode(t, resp, &page)

				return page.Items
			}
			page := preview()
			if len(page) != 1 || page[0].WorkItemID != ready.WorkItemID {
				t.Fatalf("expected only the ready candidate, got %d items", len(page))
			}
			checkClaimPreviewSummary(t, page[0], 0)
			claim := request
			claim.WorkItemID, claim.SessionID, claim.TTLSeconds = waiting.WorkItemID, "blocked", 90
			hubfixture.RequireCode(t, f.Hub.Request(t, http.MethodPost, f.Base+"/claims", runner.Redemption.Credential, claim), http.StatusConflict, "no_claimable_work")
			claim.WorkItemID, claim.SessionID = ready.WorkItemID, "ready"
			resp := f.Hub.Request(t, http.MethodPost, f.Base+"/claims", runner.Redemption.Credential, claim)
			hubfixture.RequireStatus(t, resp, http.StatusOK)
			var lease claimPreviewTestItem
			hubfixture.Decode(t, resp, &lease)
			if lease.WorkItemID != ready.WorkItemID {
				t.Fatalf("ready lease item = %s, want %s", lease.WorkItemID, ready.WorkItemID)
			}
			checkClaimPreviewSummary(t, lease, 0)
			if err := f.Hub.Repository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				project, err := w.ProjectRecord(t.Context(), f.Project.ID)
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
			resp = f.Hub.Request(t, http.MethodPost, f.Base+"/claims", runner.Redemption.Credential, claim)
			hubfixture.RequireStatus(t, resp, http.StatusOK)
			var optional claimPreviewTestItem
			hubfixture.Decode(t, resp, &optional)
			if optional.WorkItemID != waiting.WorkItemID {
				t.Fatalf("optional lease item = %s, want %s", optional.WorkItemID, waiting.WorkItemID)
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

func seedClaimPreviewBlockers(t *testing.T, f hubfixture.Native, blockerProject tracker.ProjectID, ready, waiting tracker.NativeIssue, count int) {
	t.Helper()
	ctx := t.Context()
	err := f.Hub.Repository.WriteTenant(ctx, f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		for i := range count {
			for j, target := range []tracker.NativeIssue{ready, waiting} {
				number := 10 + i*2 + j
				state := []string{"Done", "Todo"}[j]
				blocker, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: f.Project.OrganizationID, ProjectID: blockerProject, Number: int64(number), NativeID: tracker.NativeWorkItemID(fmt.Sprintf("wi_fixture_%d", number)), Title: "Example prerequisite", Archived: true, WorkflowStateName: &state})
				if err != nil {
					return err
				}
				edge := corestore.Relationship{OrganizationID: f.Project.OrganizationID, ProjectID: f.Project.ID, WorkItemID: target.WorkItemID, RelatedProjectID: blockerProject, RelatedWorkItemID: blocker.NativeID, Type: "dependency", Provenance: "native"}
				if err := w.SetRelationship(ctx, edge, "add", f.Hub.Now()); err != nil {
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
