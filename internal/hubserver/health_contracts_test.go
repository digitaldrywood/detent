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
	t.Run("linked intake", func(t *testing.T) {
		service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
		for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
			f := newNativeFixture(t, service, organization, "Example linked contract")
			if err := service.accessWrite(t.Context(), organization, func(writer tenantstore.Writer) error {
				project, err := writer.ProjectRecord(t.Context(), f.project.ID)
				if err != nil {
					return err
				}
				project.Settings.RepositoryEnabled, project.Settings.Repository = true, "example-org/example-project"
				_, err = writer.UpdateProjectRecord(t.Context(), project)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			const sourceURL = "https://github.com/example-org/example-project/issues/1"
			response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "live-linked-contract"}, Title: "Example linked issue", Body: "Incomplete contract", GitHubIssueURL: sourceURL, State: "Todo"})
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			issue = updateNativeWorkPageFixtureIssue(t, f, issue.WorkItemID, func(issue *tracker.NativeIssue) { issue.State = "Todo" })
			credential, _, err := service.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken(f.token), "", "")
			if err != nil {
				t.Fatal(err)
			}
			scope := hubscope.NativeScope{Organization: organization, Project: f.project.ID, Credential: credential}
			request := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID}
			for step, wantAllowed := range []bool{true, false, false} {
				if step == 1 {
					ref, err := eventstore.NewContent(service.runDetailStore()).PrepareLinkedIssueSnapshot(t.Context(), organization, tracker.GitHubIssueSnapshot{URL: sourceURL})
					if err != nil {
						t.Fatal(err)
					}
					if err := service.accessWrite(t.Context(), organization, func(writer tenantstore.Writer) error {
						linked, err := writer.LinkedSource(t.Context(), f.project.ID, issue.WorkItemID)
						if err != nil {
							return err
						}
						if linked.SnapshotSHA256 != "" {
							linked.ExpectedSnapshotSHA256 = new(linked.SnapshotSHA256)
						}
						linked.SnapshotObjectKey, linked.SnapshotSHA256, linked.SnapshotBytes, linked.SnapshotContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
						_, err = writer.UpdateLinkedSource(t.Context(), linked)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				plan, err := service.readNativeClaimPlan(service.freshRunDetailContext(t.Context()), scope, request, service.config.now())
				if err != nil {
					t.Fatal(err)
				}
				if plan.Item.Revision != int64(issue.Revision) || plan.Issue.Revision != issue.Revision || nativeClaimIssueContractAllowed(plan) != wantAllowed {
					t.Fatalf("organization=%s step=%d revision=%d/%d allowed=%t want revision=%d allowed=%t", organization, step, plan.Item.Revision, plan.Issue.Revision, nativeClaimIssueContractAllowed(plan), issue.Revision, wantAllowed)
				}
				context := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: hubmaintenance.ByteLimit}.Context(t.Context(), hubmaintenance.Tenant{Organization: string(organization), Generation: 1})
				if err := service.maintainHealthWaitItem(context, organization, corestore.WorkItemReference{ProjectID: f.project.ID, WorkItemID: issue.WorkItemID}); err != nil {
					t.Fatal(err)
				}
				page, err := service.readHealthFindings(t.Context(), scope, "open", "", "", 100)
				if err != nil {
					t.Fatal(err)
				}
				waiting := false
				for _, finding := range page.Items {
					if finding.Signal == "unanswered_human_wait" && finding.Subject.ID == string(issue.WorkItemID) {
						waiting = true
						if finding.Evidence.Reason != "issue_contract_unsatisfied" {
							t.Fatalf("current contract wait reason=%+v", finding)
						}
					}
				}
				if waiting == wantAllowed {
					t.Fatalf("health contract projection reused stale source: step=%d waiting=%t allowed=%t", step, waiting, wantAllowed)
				}

			}
		}
	})
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
		prepared.ProjectID, prepared.ApprovedBy, prepared.ApprovedAt = f.f.project, "example-owner", service.config.now()
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
					return writer.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: "org_security", ProjectID: f.f.project, WorkItemID: f.delivery.WorkItemID, RelatedProjectID: f.f.project, RelatedWorkItemID: f.source.WorkItemID, Type: "dependency"}, operation, service.config.now())
				}); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := service.readNativeClaimPlan(service.freshRunDetailContext(t.Context()), scope, request, service.config.now())
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
