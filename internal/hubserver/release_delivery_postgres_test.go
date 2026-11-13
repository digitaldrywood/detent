package hubserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type releaseTenantFixture struct {
	repository       tenantstore.Repository
	f                hostedSecurityFixture
	owner            hostedSecurityUser
	source, delivery tracker.NativeIssue
	change           tracker.ChangeRequest
	version          tracker.ChangeVersion
	request          tracker.AssociateReleasePick
	verifier         *releaseVerifierFixture
}

func newReleaseTenantFixture(t *testing.T) *releaseTenantFixture {
	t.Helper()
	repo, f, owner, _ := newChangePolicyPostgresFixtureWithAdmin(t, 4)
	return newReleaseTenantFixtureFor(t, repo, f, owner)
}

func newReleaseTenantFixtureFor(t *testing.T, repo tenantstore.Repository, f hostedSecurityFixture, owner hostedSecurityUser) *releaseTenantFixture {
	t.Helper()
	organization := tracker.OrganizationID(f.service.config.Hosted.OrganizationID)
	fixture := &releaseTenantFixture{repository: repo, f: f, owner: owner}
	f.service.database.hostedRuntime.Store(&hostedplans.RuntimeConfig{})
	if err := repo.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project)
		if err != nil {
			return err
		}
		project.Core.RequireDependencies = true
		project.Settings.CheckoutRepository = "example-org/example-project"
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	create := func(key string) tracker.NativeIssue {
		response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, Title: "Example " + key, Body: workItemRecordBody, State: "Todo"})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		return issue
	}
	fixture.source, fixture.delivery = create("source"), create("delivery")
	rules := tracker.ChangeReviewPolicy{PolicyID: hubTestPolicy().ID, RequireReview: true}
	requireNativeStatus(t, f.request(t, owner, http.MethodPut, f.base+"/change-review-policy", tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "release-review-policy"}, Policy: rules}), http.StatusOK)

	response := f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(fixture.source.WorkItemID)+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "source-change"}, Title: "Example source change"})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &fixture.change)
	content := eventstore.NewContent(f.service.runDetailStore())
	now := f.service.config.Clock().UTC()
	if err := repo.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		hash := apikey.HashToken("example-release-runner")
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "release-runner-token", Name: "Runner", Hash: hash, Fingerprint: hubapi.TokenFingerprint(hash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ProjectAccess: "all", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: "release-enrollment", RunnerID: "release-runner", MachineID: "release-machine", TokenHash: apikey.HashToken("example-enrollment"), Scope: "organization", Operations: []string{"read"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: "example-admin"}); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: "release-machine", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: "release-runner", MachineID: "release-machine", TokenID: "release-runner-token", EnrollmentID: "release-enrollment", Scope: "organization", Operations: []string{"read"}, DisplayName: "Example runner", CapacityLimit: 1, ReportedCapacity: 1, OS: "linux", Architecture: "amd64", CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
			return err
		}
		return w.ReplaceRunnerCheckoutReport(t.Context(), runnerstore.CheckoutReport{OrganizationID: organization, RunnerID: "release-runner", ProjectID: f.project, Repository: "example-org/example-project", ReportedAt: now})
	}); err != nil {
		t.Fatal(err)
	}

	fixture.version = tracker.ChangeVersion{ID: hubapi.NewID("version"), ChangeID: fixture.change.ID, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), Repository: "https://github.com/example-org/example-project", PolicyID: hubTestPolicy().ID}, Actor: tracker.Actor{Kind: "human", PrincipalID: "source-author"}, CreatedAt: now}
	raw, err := json.Marshal(fixture.version)
	if err != nil {
		t.Fatal(err)
	}
	version, err := content.PrepareVersion(t.Context(), organization, raw)
	if err != nil {
		t.Fatal(err)
	}
	err = repo.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project)
		if err != nil {
			return err
		}
		project.Core.RequireDependencies = true
		project.Settings.CheckoutRepository = "example-org/example-project"
		if _, err = w.UpdateProjectRecord(t.Context(), project); err != nil {
			return err
		}
		source, err := w.WorkItem(t.Context(), f.project, fixture.source.WorkItemID)
		if err != nil {
			return err
		}
		source.WorkflowStateName = new("Done")
		if _, err = w.UpdateWorkItem(t.Context(), source); err != nil {
			return err
		}
		if _, err = w.CreateVersion(t.Context(), version); err != nil {
			return err
		}
		change, err := w.Change(t.Context(), fixture.change.ID)
		if err != nil {
			return err
		}
		change.CurrentVersionID = &version.ID
		change.LandedVersionID = &version.ID
		change.LandedAt = &now
		change.LandedHeadSHA = &version.HeadSHA
		change.LandedMergeSHA = new(strings.Repeat("c", 40))
		change.LandedBaseRef = new("develop")
		if _, err = w.UpdateChange(t.Context(), change); err != nil {
			return err
		}
		return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: organization, ProjectID: f.project, WorkItemID: fixture.delivery.WorkItemID, RelatedProjectID: f.project, RelatedWorkItemID: fixture.source.WorkItemID, Type: "dependency"}, "add", now)
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.request = tracker.AssociateReleasePick{Mutation: tracker.Mutation{IdempotencyKey: "associate-release"}, ExpectedRevision: fixture.delivery.Revision, ReleasePickInput: tracker.ReleasePickInput{SourceIssueID: fixture.source.WorkItemID, SourceChangeID: fixture.change.ID, SourceMergeSHA: strings.Repeat("c", 40), Repository: version.Repository, PRNumber: 7, HeadRef: "pick/example", HeadSHA: strings.Repeat("d", 40), BaseRef: "release/v1.2", BaseSHA: strings.Repeat("e", 40), FrozenSHA: strings.Repeat("f", 40)}}
	fixture.verifier = &releaseVerifierFixture{input: fixture.request.ReleasePickInput}
	f.service.config.ReconcileBackend = fixture.verifier
	return fixture
}

func TestReleaseAssociationPostgres(t *testing.T) {
	t.Parallel()
	f := newReleaseTenantFixture(t)
	path := f.f.base + "/work-items/" + string(f.delivery.WorkItemID) + "/release-pick"
	first := f.f.request(t, f.owner, http.MethodPost, path, f.request)
	requireNativeStatus(t, first, http.StatusOK)
	var delivery tracker.ReleaseDelivery
	decodeHubResponse(t, first, &delivery)
	if delivery.ChangeID == "" || delivery.ReleasePickInput != f.request.ReleasePickInput {
		t.Fatalf("delivery = %+v", delivery)
	}
	replay := f.f.request(t, f.owner, http.MethodPost, path, f.request)
	requireNativeStatus(t, replay, http.StatusOK)
	if replay.Body.String() != first.Body.String() {
		t.Fatal("replay changed delivery")
	}
	same := f.request
	same.IdempotencyKey = "same-delivery"
	replay = f.f.request(t, f.owner, http.MethodPost, path, same)
	requireNativeStatus(t, replay, http.StatusOK)
	var repeated tracker.ReleaseDelivery
	decodeHubResponse(t, replay, &repeated)
	if repeated != delivery {
		t.Fatal("same association changed delivery")
	}
	var issue tracker.NativeIssue
	decodeHubResponse(t, f.f.request(t, f.owner, http.MethodGet, strings.TrimSuffix(path, "/release-pick"), nil), &issue)
	if issue.Revision != f.delivery.Revision+1 {
		t.Fatalf("revision = %d", issue.Revision)
	}
	var changes []tracker.ChangeRequest
	decodeHubResponse(t, f.f.request(t, f.owner, http.MethodGet, strings.TrimSuffix(path, "/release-pick")+"/changes", nil), &changes)
	if len(changes) != 1 || changes[0].ID != delivery.ChangeID {
		t.Fatalf("changes = %+v", changes)
	}
	f.verifier.status = func(input tracker.ReleasePickInput) (tracker.ReleaseDeliveryStatus, error) {
		return tracker.ReleaseDeliveryStatus{ReleasePickInput: input, ReleaseHeadSHA: input.HeadSHA}, nil
	}
	requireNativeStatus(t, f.f.request(t, f.owner, http.MethodGet, path, nil), http.StatusOK)
	for _, test := range []struct {
		name               string
		scan, driftAtClaim bool
		invalid, wantCode  string
	}{
		{name: "delegated explicit claim"},
		{name: "delegated candidate scan", scan: true},
		{name: "changed policy", invalid: "policy", wantCode: "issue_contract_unsatisfied"},
		{name: "changed body", invalid: "body", wantCode: "issue_contract_unsatisfied"},
		{name: "removed dependency", invalid: "dependency", wantCode: "issue_contract_unsatisfied"},
		{name: "ignored dependencies", invalid: "ignored", wantCode: "issue_contract_unsatisfied"},
		{name: "disabled project dependencies", invalid: "project", wantCode: "issue_contract_unsatisfied"},
		{name: "nonterminal source", invalid: "source", wantCode: "no_claimable_work"},
		{name: "changed landing version", invalid: "version", wantCode: "issue_contract_unsatisfied"},
		{name: "changed landing base", invalid: "base", wantCode: "issue_contract_unsatisfied"},
		{name: "changed landing merge", invalid: "merge", wantCode: "issue_contract_unsatisfied"},
		{name: "landing changes after snapshot", invalid: "merge", driftAtClaim: true, wantCode: "no_claimable_work"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newReleaseTenantFixture(t)
			service := f.f.service
			service.config.NativeRunEvents = tenantNativeRunEventParticipant{service: service}
			prompt := "## Release delivery contract\nVerified release picks of reviewed develop fixes may receive the approved delivery contract.\n### Acceptance criteria\nDeliver only the verified source.\n### Must not break\nPreserve gates.\n### How we know it worked\nThe associated pick passes review."
			cfg := config.Default()
			cfg.Tracker.Kind = config.TrackerHubNative
			descriptor, err := config.ResolvePolicy(config.Workflow{Config: cfg, SharedPrompt: prompt, Authored: &policy.Authored{Version: 1, Digest: policy.Digest([]byte(prompt))}})
			if err != nil {
				t.Fatal(err)
			}
			approve := func(descriptor policy.Descriptor) {
				t.Helper()
				row, err := eventstore.NewContent(service.runDetailStore()).PrepareProjectPolicy(t.Context(), "org_security", descriptor)
				if err != nil {
					t.Fatal(err)
				}
				row.ProjectID = f.f.project
				row.ApprovedBy = "example-owner"
				row.ApprovedAt = service.config.Clock()
				if err := f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
					current, err := w.ProjectPolicy(t.Context(), f.f.project)
					if err != nil {
						return err
					}
					return w.ApproveProjectPolicy(t.Context(), row, current.PolicyID)
				}); err != nil {
					t.Fatal(err)
				}
			}
			approve(descriptor)
			contract, err := config.ResolvePolicyIssueContract(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if contract.ReleaseDeliveryBody() == "" {
				t.Fatal("fixture policy lacks delegated release contract")
			}
			f.delivery = writeReleaseAdmissionIssue(t, f, f.delivery, func(issue *tracker.NativeIssue) {
				issue.Body = issueorigin.Stamp(contract.ReleaseDeliveryBody(), issueorigin.Origin{Kind: "worker", Fingerprint: "release-delivery"})
				issue.IssueContract = nil
			})
			rules := tracker.ChangeReviewPolicy{PolicyID: descriptor.ID, RequireReview: true}
			var currentRules tracker.ChangeReviewPolicy
			decodeHubResponse(t, f.f.request(t, f.owner, http.MethodGet, f.f.base+"/change-review-policy", nil), &currentRules)
			requireNativeStatus(t, f.f.request(t, f.owner, http.MethodPut, f.f.base+"/change-review-policy", tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "delegated-review"}, ExpectedID: currentRules.ID, Policy: rules}), http.StatusOK)
			f.request.ExpectedRevision = f.delivery.Revision
			itemPath := f.f.base + "/work-items/" + string(f.delivery.WorkItemID)
			requireNativeStatus(t, f.f.request(t, f.owner, http.MethodPost, itemPath+"/release-pick", f.request), http.StatusOK)
			decodeHubResponse(t, f.f.request(t, f.owner, http.MethodGet, itemPath, nil), &f.delivery)
			if f.delivery.IssueContract == nil || f.delivery.IssueContract.ReleaseDelivery == nil || f.delivery.IssueContract.ReleaseDelivery.PolicyID != descriptor.ID {
				t.Fatalf("association omitted delegation: %+v", f.delivery.IssueContract)
			}
			token, runnerID, machineID := seedReleaseAdmissionRunner(t, f, descriptor.ID)
			invalidate := func() error {
				switch test.invalid {
				case "policy":
					descriptor = hubTestPolicy()
					approve(descriptor)
					return nil
				case "body":
					f.delivery = writeReleaseAdmissionIssue(t, f, f.delivery, func(issue *tracker.NativeIssue) { issue.Body += "\nChanged after approval." })
					return nil
				case "ignored":
					f.delivery = writeReleaseAdmissionIssue(t, f, f.delivery, func(issue *tracker.NativeIssue) { issue.IgnoreDependencies = true })
					return nil
				}
				return f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.LockRelationshipGraph(t.Context()); err != nil {
						return err
					}
					if err := w.LockWorkItemProject(t.Context(), f.f.project); err != nil {
						return err
					}
					switch test.invalid {
					case "dependency":
						return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: "org_security", ProjectID: f.f.project, WorkItemID: f.delivery.WorkItemID, RelatedProjectID: f.f.project, RelatedWorkItemID: f.source.WorkItemID, Type: "dependency"}, "remove", service.config.Clock())
					case "project":
						project, err := w.ProjectRecord(t.Context(), f.f.project)
						if err != nil {
							return err
						}
						project.Core.RequireDependencies = false
						_, err = w.UpdateProjectRecord(t.Context(), project)
						return err
					case "source":
						source, err := w.WorkItem(t.Context(), f.f.project, f.source.WorkItemID)
						if err != nil {
							return err
						}
						source.WorkflowStateName = new("Todo")
						_, err = w.UpdateWorkItem(t.Context(), source)
						return err
					default:
						change, err := w.Change(t.Context(), f.change.ID)
						if err != nil {
							return err
						}
						switch test.invalid {
						case "version":
							change.CurrentVersionID = nil
						case "base":
							change.LandedBaseRef = new("release/v1.2")
						case "merge":
							change.LandedMergeSHA = new(strings.Repeat("d", 40))
						}
						_, err = w.UpdateChange(t.Context(), change)
						return err
					}
				})
			}
			if test.invalid != "" && !test.driftAtClaim {
				if err := invalidate(); err != nil {
					t.Fatal(err)
				}
			}
			response := f.f.request(t, f.owner, http.MethodGet, itemPath+"/runtime", nil)
			requireNativeStatus(t, response, http.StatusOK)
			var evidence tracker.NativeRuntimeEvidence
			decodeHubResponse(t, response, &evidence)
			refused := test.invalid != "" && !test.driftAtClaim
			const refusal = "The verified release delivery contract changed; current approved policy and exact associated delivery are required"
			if refused {
				if evidence.Scheduling.Outcome != "skipped" || evidence.Scheduling.Reason != refusal {
					t.Fatalf("delegation refusal lost: %+v", evidence.Scheduling)
				}
			} else {
				found := false
				for _, admission := range evidence.Admission {
					if admission.RunnerID == runnerID && admission.Outcome == "ready" {
						found = true
					}
				}
				if evidence.Scheduling.Outcome == "skipped" || !found {
					t.Errorf("current delegated release refused: scheduling=%+v admission=%+v", evidence.Scheduling, evidence.Admission)
				}
			}
			if !refused {
				for _, session := range []string{"", "delegated-preview"} {
					preview := tracker.NativeCapacityPreview{NativeClaim: tracker.NativeClaim{PolicyID: descriptor.ID, MachineID: machineID, SessionID: session, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}, Limit: 100}
					response := performHubAPIRequest(t, service, http.MethodPost, f.f.base+"/claims/preview", token, preview)
					requireNativeStatus(t, response, http.StatusOK)
					var page tracker.NativeCapacityPage
					decodeHubResponse(t, response, &page)
					if len(page.Items) != 1 || page.Items[0].WorkItemID != f.delivery.WorkItemID || page.Items[0].IssueContract == nil || page.Items[0].IssueContract.ReleaseDelivery == nil || page.Items[0].IssueContract.ReleaseDelivery.PolicyID != descriptor.ID {
						t.Errorf("delegated preview session=%q: %+v", session, page)
					}
				}
			}
			if test.driftAtClaim {
				service.config.TenantRepository = &releaseAdmissionBeforeWriteRepository{projectUpdateBeforeWriteRepository: &projectUpdateBeforeWriteRepository{Repository: f.repository, before: invalidate}, SnapshotRepository: f.repository.(tenantstore.SnapshotRepository)}
			}
			request := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: f.delivery.WorkItemID, MachineID: machineID, SessionID: "delegated-claim", TTLSeconds: 90, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
			if test.scan {
				request.WorkItemID = ""
			}
			response = performHubAPIRequest(t, service, http.MethodPost, f.f.base+"/claims", token, request)
			if test.invalid != "" {
				requireNativeStatus(t, response, http.StatusConflict)
				if !strings.Contains(response.Body.String(), `"code":"`+test.wantCode+`"`) {
					t.Fatalf("unexpected release refusal: %s", response.Body)
				}
				if err := f.repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
					item, err := r.WorkItem(t.Context(), f.f.project, f.delivery.WorkItemID)
					if err != nil {
						return err
					}
					_, err = r.ActiveLeaseForIssue(t.Context(), item.ID)
					if !errors.Is(err, corestore.ErrNotFound) {
						return errors.New("refused delegation acquired a lease")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				if lease.WorkItemID != f.delivery.WorkItemID || lease.PolicyID != descriptor.ID || lease.FencingToken == 0 {
					t.Fatalf("delegated lease binding: %+v", lease)
				}
			}
		})
	}

}

func TestReleaseAssociationRefreshPostgres(t *testing.T) {
	t.Parallel()
	f := newReleaseTenantFixture(t)
	path := f.f.base + "/work-items/" + string(f.delivery.WorkItemID) + "/release-pick"
	var first tracker.ReleaseDelivery
	response := f.f.request(t, f.owner, http.MethodPost, path, f.request)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &first)
	refresh := f.request
	refresh.IdempotencyKey = "refresh-release"
	refresh.ExpectedRevision++
	refresh.HeadSHA, refresh.BaseSHA = strings.Repeat("1", 40), strings.Repeat("2", 40)
	f.verifier.input = refresh.ReleasePickInput
	requireNativeStatus(t, f.f.request(t, f.owner, http.MethodPost, path, refresh), http.StatusUnprocessableEntity)
	seedReleaseTenantAudit(t, f)
	response = f.f.request(t, f.owner, http.MethodPost, path, refresh)
	requireNativeStatus(t, response, http.StatusOK)
	var updated tracker.ReleaseDelivery
	decodeHubResponse(t, response, &updated)
	if updated.ChangeID != first.ChangeID || updated.VersionID != "" || updated.ReleasePickInput != refresh.ReleasePickInput {
		t.Fatalf("refreshed = %+v", updated)
	}
	var issue tracker.NativeIssue
	decodeHubResponse(t, f.f.request(t, f.owner, http.MethodGet, strings.TrimSuffix(path, "/release-pick"), nil), &issue)
	if issue.Revision != f.delivery.Revision+2 {
		t.Fatalf("revision = %d", issue.Revision)
	}
	var changes []tracker.ChangeRequest
	decodeHubResponse(t, f.f.request(t, f.owner, http.MethodGet, strings.TrimSuffix(path, "/release-pick")+"/changes", nil), &changes)
	if len(changes) != 1 || changes[0].Revision != 2 || changes[0].CurrentVersion != "" {
		t.Fatalf("refreshed changes = %+v", changes)
	}
	replay := f.f.request(t, f.owner, http.MethodPost, path, refresh)
	requireNativeStatus(t, replay, http.StatusOK)
	if replay.Body.String() != response.Body.String() {
		t.Fatal("refresh replay changed")
	}
}

func TestReleaseAssociationRechecksProviderPostgres(t *testing.T) {
	t.Parallel()
	f := newReleaseTenantFixture(t)
	f.verifier.verify = func(tracker.ReleasePickInput) error {
		return f.f.provider.RevokeMembership(t.Context(), "membership_"+f.owner.identity.Subject)
	}
	path := f.f.base + "/work-items/" + string(f.delivery.WorkItemID) + "/release-pick"
	response := f.f.request(t, f.owner, http.MethodPost, path, f.request)
	if response.Code != http.StatusForbidden && response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked provider response = %d: %s", response.Code, response.Body.String())
	}
	err := f.repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		if _, err := r.(eventstore.ReleaseDeliveryReader).ReleaseDelivery(t.Context(), f.f.project, f.delivery.WorkItemID); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("revoked provider wrote delivery: %v", err)
		}
		changes, err := r.Changes(t.Context(), f.f.project, f.delivery.WorkItemID, "", 10)
		if err == nil && len(changes) != 0 {
			t.Fatalf("revoked provider wrote %d changes", len(changes))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func seedReleaseTenantAudit(t *testing.T, f *releaseTenantFixture) {
	t.Helper()
	now := f.f.service.config.Clock()
	audit := &tracker.SecurityAuditReview{SessionID: 42, DiffDigest: policy.Digest([]byte("diff")), Verdict: "pass", Run: securityaudit.Run{ID: 42, InvocationID: "source-audit", ProjectID: string(f.f.project), IssueID: string(f.source.WorkItemID), Repository: f.version.Repository, BaseSHA: f.version.BaseSHA, HeadSHA: f.version.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), AuthenticationMode: "api_key", ExitStatus: securityaudit.ExitStatusSuccess, WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: now, StartedAt: now.Add(-time.Second), CompletedAt: now, OutputBytes: 1, OutputDigest: securityaudit.OutputDigest("result"), Verdict: "pass", Summary: "No findings"}}
	review := tracker.ChangeReview{ID: hubapi.NewID("review"), VersionID: f.version.ID, Decision: "approved", SecurityAudit: audit, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-reviewer"}, CreatedAt: now}
	raw, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	row, err := eventstore.NewContent(f.f.service.runDetailStore()).PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: tracker.OrganizationID(f.f.service.config.Hosted.OrganizationID), ChangeID: f.change.ID, Kind: "review"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.repository.WriteTenant(t.Context(), tracker.OrganizationID(f.f.service.config.Hosted.OrganizationID), corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), row); return err }); err != nil {
		t.Fatal(err)
	}
}

func writeReleaseAdmissionIssue(t *testing.T, f *releaseTenantFixture, issue tracker.NativeIssue, change func(*tracker.NativeIssue)) tracker.NativeIssue {
	t.Helper()
	var row corestore.WorkItem
	if err := f.repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.WorkItem(t.Context(), f.f.project, issue.WorkItemID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	change(&issue)
	issue.Revision++
	issue.UpdatedAt = f.f.service.config.Clock()
	issue.LastActivityAt = issue.UpdatedAt
	fields, err := f.f.service.prepareWorkItemFields(t.Context(), "org_security", hubrecords.WorkItemRecordPlan{Issue: issue})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(f.f.service.runDetailStore())
	version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: "org_security", ProjectID: f.f.project, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, raw)
	if err != nil {
		t.Fatal(err)
	}
	search, err := searchstore.NewContent(f.f.service.runDetailStore()).Prepare(t.Context(), searchstore.Document{OrganizationID: "org_security", ProjectID: f.f.project, WorkItemID: issue.WorkItemID, ID: "issue:" + string(issue.WorkItemID), SourceID: string(issue.WorkItemID), Kind: "issue", Scope: "issue", Revision: int64(issue.Revision), UpdatedAt: issue.UpdatedAt}, searchstore.Text{Title: issue.Title, Body: issue.Body, Labels: issue.Labels})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		row.BodyObjectKey = &fields.Body
		row.IssueContractObjectKey = fields.Contract
		row.NativeUpdatedAt = issue.UpdatedAt
		row.UpdatedAt = issue.UpdatedAt
		row.LastActivityAt = issue.LastActivityAt
		updated, err := w.UpdateWorkItem(t.Context(), row)
		if err != nil {
			return err
		}
		if updated.Revision != int64(issue.Revision) {
			return corestore.ErrRevisionConflict
		}
		if err := w.PutSearch(t.Context(), search, row.Revision); err != nil {
			return err
		}
		_, err = w.CreateCollaborationVersion(t.Context(), version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	decodeHubResponse(t, f.f.request(t, f.owner, http.MethodGet, f.f.base+"/work-items/"+string(issue.WorkItemID), nil), &issue)
	return issue
}

func seedReleaseAdmissionRunner(t *testing.T, f *releaseTenantFixture, policyID string) (string, string, tracker.MachineID) {
	t.Helper()
	service := f.f.service
	now := service.config.Clock().UTC()
	binding := runnerauth.NewBinding()
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	response := f.f.request(t, f.owner, http.MethodPost, "/api/v2/organizations/org_security/runner-enrollments", runnerauth.EnrollmentRequest{Scope: "projects", Binding: binding, ProjectIDs: []tracker.ProjectID{f.f.project}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat}, TTLSeconds: 60})
	requireNativeStatus(t, response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	decodeHubResponse(t, response, &enrollment)
	response = performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/org_security/runner-enrollments/redeem", enrollment.Token, runnerauth.Redemption{BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: "example-host", DisplayName: "Example runner", Capacity: 2, Version: "test"})
	requireNativeStatus(t, response, http.StatusCreated)
	response = performHubAPIRequest(t, service, http.MethodPost, f.f.base+"/machines/"+string(binding.MachineID)+"/heartbeat", credential, map[string]any{"display_name": "Example runner", "capacity": 2, "version": "test", "backend_isolation": isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, "capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: now}, "admission": map[string]any{"context": tracker.NativeAdmissionContext{PolicyID: policyID, ObservedAt: now, WorkflowStates: []string{"Todo"}}, "runner_revision": 1}})
	requireNativeStatus(t, response, http.StatusOK)
	return credential, binding.RunnerID, binding.MachineID
}

type releaseAdmissionBeforeWriteRepository struct {
	*projectUpdateBeforeWriteRepository
	tenantstore.SnapshotRepository
}
