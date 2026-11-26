package hubserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthContractsLiveInputs(t *testing.T) {
	t.Parallel()
	t.Run("release delegation", func(t *testing.T) {
		f := newReleaseTenantFixture(t)
		service := f.f.service
		prompt := "## Release delivery contract\nVerified release picks of reviewed develop fixes may receive the approved delivery contract.\n### Acceptance criteria\nDeliver only the verified source.\n### Must not break\nPreserve gates.\n### How we know it worked\nThe associated pick passes review."
		cfg := config.Default()
		cfg.Tracker.Kind = config.TrackerHubNative
		descriptor, err := config.ResolvePolicy(config.Workflow{Config: cfg, SharedPrompt: prompt, Authored: &policy.Authored{Version: 1, Digest: policy.Digest([]byte(prompt))}})
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := eventstore.NewContent(service.runDetailStore()).PrepareProjectPolicy(t.Context(), "org_security", descriptor)
		if err != nil {
			t.Fatal(err)
		}
		prepared.ProjectID, prepared.ApprovedBy, prepared.ApprovedAt = f.f.project, "example-owner", service.config.Clock()
		if err := f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(writer tenantstore.Writer) error {
			current, err := writer.ProjectPolicy(t.Context(), f.f.project)
			if err != nil {
				return err
			}
			return writer.ApproveProjectPolicy(t.Context(), prepared, current.PolicyID)
		}); err != nil {
			t.Fatal(err)
		}
		contract, err := config.ResolvePolicyIssueContract(descriptor)
		if err != nil || contract.ReleaseDeliveryBody() == "" {
			t.Fatalf("release contract unavailable: %v", err)
		}
		f.delivery = writeReleaseAdmissionIssue(t, f, f.delivery, func(issue *tracker.NativeIssue) {
			issue.Body = issueorigin.Stamp(contract.ReleaseDeliveryBody(), issueorigin.Origin{Kind: "worker", Fingerprint: "release-delivery"})
			issue.IssueContract = nil
		})
		var currentRules tracker.ChangeReviewPolicy
		decodeHubResponse(t, f.f.request(t, f.owner, http.MethodGet, f.f.base+"/change-review-policy", nil), &currentRules)
		rules := tracker.ChangeReviewPolicy{PolicyID: descriptor.ID, RequireReview: true}
		requireNativeStatus(t, f.f.request(t, f.owner, http.MethodPut, f.f.base+"/change-review-policy", tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "live-release-review"}, ExpectedID: currentRules.ID, Policy: rules}), http.StatusOK)
		f.request.ExpectedRevision = f.delivery.Revision
		itemPath := f.f.base + "/work-items/" + string(f.delivery.WorkItemID)
		requireNativeStatus(t, f.f.request(t, f.owner, http.MethodPost, itemPath+"/release-pick", f.request), http.StatusOK)
		decodeHubResponse(t, f.f.request(t, f.owner, http.MethodGet, itemPath, nil), &f.delivery)
		if f.delivery.IssueContract == nil || f.delivery.IssueContract.ReleaseDelivery == nil || f.delivery.IssueContract.ReleaseDelivery.PolicyID != descriptor.ID || !strings.Contains(f.delivery.Body, "Acceptance criteria") {
			t.Fatalf("verified release delegation missing: %+v", f.delivery.IssueContract)
		}
		token, _, machineID := seedReleaseAdmissionRunner(t, f, descriptor.ID)
		credential, _, err := service.authenticateTenantAPIHash(t.Context(), "org_security", apikey.HashToken(token), "", "")
		if err != nil {
			t.Fatal(err)
		}
		scope := hubscope.NativeScope{Organization: "org_security", Project: f.f.project, Credential: credential}
		request := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: f.delivery.WorkItemID, MachineID: machineID}
		for step, wantAllowed := range []bool{false, true, true} {
			if step < 2 {
				operation := "remove"
				if step == 1 {
					operation = "add"
				}
				if err := f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(writer tenantstore.Writer) error {
					if err := writer.LockRelationshipGraph(t.Context()); err != nil {
						return err
					}
					if err := writer.LockWorkItemProject(t.Context(), f.f.project); err != nil {
						return err
					}
					return writer.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: "org_security", ProjectID: f.f.project, WorkItemID: f.delivery.WorkItemID, RelatedProjectID: f.f.project, RelatedWorkItemID: f.source.WorkItemID, Type: "dependency"}, operation, service.config.Clock())
				}); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := service.readNativeClaimPlan(service.freshRunDetailContext(t.Context()), scope, request, service.config.Clock())
			if err != nil {
				t.Fatal(err)
			}
			if plan.Item.Revision != int64(f.delivery.Revision) || plan.Issue.Revision != f.delivery.Revision || nativeClaimIssueContractAllowed(plan) != wantAllowed {
				t.Fatalf("step=%d revision=%d/%d allowed=%t want revision=%d allowed=%t", step, plan.Item.Revision, plan.Issue.Revision, nativeClaimIssueContractAllowed(plan), f.delivery.Revision, wantAllowed)
			}
			context := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: hubmaintenance.ByteLimit}.Context(t.Context(), hubmaintenance.Tenant{Organization: "org_security", Generation: 1})
			if err := service.maintainHealthWaitItem(context, "org_security", corestore.WorkItemReference{ProjectID: f.f.project, WorkItemID: f.delivery.WorkItemID}); err != nil {
				t.Fatal(err)
			}
			page, err := service.readHealthFindings(t.Context(), scope, "open", "", "", 100)
			if err != nil {
				t.Fatal(err)
			}
			waiting := false
			for _, finding := range page.Items {
				if finding.Signal == "unanswered_human_wait" && finding.Subject.ID == string(f.delivery.WorkItemID) {
					waiting = true
				}
			}
			if waiting == wantAllowed {
				t.Fatalf("health release contract reused stale association: step=%d waiting=%t allowed=%t", step, waiting, wantAllowed)
			}

		}
	})
}
