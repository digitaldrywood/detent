package hubserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
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
	fixture := &releaseTenantFixture{repository: repo, f: f, owner: owner}
	f.service.database.hostedRuntime.Store(&hostedRuntimeConfig{})
	if err := repo.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
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
	now := f.service.config.now().UTC()
	if err := repo.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		hash := apikey.HashToken("example-release-runner")
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_security", ID: "release-runner-token", Name: "Runner", Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ProjectAccess: "all", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: "org_security", ID: "release-enrollment", RunnerID: "release-runner", MachineID: "release-machine", TokenHash: apikey.HashToken("example-enrollment"), Scope: "organization", Operations: []string{"read"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: "example-admin"}); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_security", ID: "release-machine", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: "org_security", RunnerID: "release-runner", MachineID: "release-machine", TokenID: "release-runner-token", EnrollmentID: "release-enrollment", Scope: "organization", Operations: []string{"read"}, DisplayName: "Example runner", CapacityLimit: 1, ReportedCapacity: 1, OS: "linux", Architecture: "amd64", CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
			return err
		}
		return w.ReplaceRunnerCheckoutReport(t.Context(), runnerstore.CheckoutReport{OrganizationID: "org_security", RunnerID: "release-runner", ProjectID: f.project, Repository: "example-org/example-project", ReportedAt: now})
	}); err != nil {
		t.Fatal(err)
	}

	fixture.version = tracker.ChangeVersion{ID: newNativeID("version"), ChangeID: fixture.change.ID, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), Repository: "https://github.com/example-org/example-project", PolicyID: hubTestPolicy().ID}, Actor: tracker.Actor{Kind: "human", PrincipalID: "source-author"}, CreatedAt: now}
	raw, err := json.Marshal(fixture.version)
	if err != nil {
		t.Fatal(err)
	}
	version, err := content.PrepareVersion(t.Context(), "org_security", raw)
	if err != nil {
		t.Fatal(err)
	}
	err = repo.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
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
		return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: "org_security", ProjectID: f.project, WorkItemID: fixture.delivery.WorkItemID, RelatedProjectID: f.project, RelatedWorkItemID: fixture.source.WorkItemID, Type: "dependency"}, "add", now)
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
}

func TestReleaseAssociationRefreshPostgres(t *testing.T) {
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
	now := f.f.service.config.now()
	audit := &tracker.SecurityAuditReview{SessionID: 42, DiffDigest: policy.Digest([]byte("diff")), Verdict: "pass", Run: securityaudit.Run{ID: 42, InvocationID: "source-audit", ProjectID: string(f.f.project), IssueID: string(f.source.WorkItemID), Repository: f.version.Repository, BaseSHA: f.version.BaseSHA, HeadSHA: f.version.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), AuthenticationMode: "api_key", ExitStatus: securityaudit.ExitStatusSuccess, WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: now, StartedAt: now.Add(-time.Second), CompletedAt: now, OutputBytes: 1, OutputDigest: securityaudit.OutputDigest("result"), Verdict: "pass", Summary: "No findings"}}
	review := tracker.ChangeReview{ID: newNativeID("review"), VersionID: f.version.ID, Decision: "approved", SecurityAudit: audit, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-reviewer"}, CreatedAt: now}
	raw, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	row, err := eventstore.NewContent(f.f.service.runDetailStore()).PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: "org_security", ChangeID: f.change.ID, Kind: "review"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), row); return err }); err != nil {
		t.Fatal(err)
	}
}
