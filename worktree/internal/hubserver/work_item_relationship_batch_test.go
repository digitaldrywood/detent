package hubserver

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type relationshipBatchCountingRepository struct {
	tenantstore.Repository
	reads   int
	batches int
}

func (r *relationshipBatchCountingRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	r.reads++
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
		return read(&relationshipBatchCountingReader{Reader: reader, repository: r})
	})
}

type relationshipBatchCountingReader struct {
	tenantstore.Reader
	repository *relationshipBatchCountingRepository
}

func (r *relationshipBatchCountingReader) WorkItemRelationshipFacts(ctx context.Context, project tracker.ProjectID, workItems []tracker.NativeWorkItemID) ([]tenantstore.WorkItemRelationshipFact, error) {
	r.repository.batches++
	batch := r.Reader.(tenantstore.WorkItemRelationshipBatchReader)
	return batch.WorkItemRelationshipFacts(ctx, project, workItems)
}

func TestHydrateWorkItemRelationshipFactsPagePreservesReporterRedaction(t *testing.T) {
	repository, fixture, _, _ := newChangePolicyPostgresFixtureWithAdmin(t, 8)
	if err := fixture.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	tenantReads := &relationshipBatchCountingRepository{Repository: repository}
	accessReads := &relationshipBatchCountingRepository{Repository: repository}
	fixture.service.config.TenantRepository = tenantReads
	fixture.service.config.AccessRepository = accessReads
	organization := tracker.OrganizationID("org_security")
	project := fixture.project
	privateProject := tracker.ProjectID("prj_private")
	sharedProject := tracker.ProjectID("prj_shared")
	const reporterID = "credential_reporter"
	const reporterUser = "user_reporter"
	const reporterMembership = "membership_reporter"
	const otherID = "principal_other"
	now := fixture.service.config.now()
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if _, err := writer.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: privateProject, Name: "Private project", Slug: "private-project", Profile: "native"}); err != nil {
			return err
		}
		if _, err := writer.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: sharedProject, Name: "Shared project", Slug: "shared-project", Profile: "native"}); err != nil {
			return err
		}
		if err := writer.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: reporterID, Name: "Reporter", Hash: "example-hash", Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: reporterUser, MembershipID: reporterMembership, KeyScope: "read", ProjectAccess: "selected", Kind: "personal"}); err != nil {
			return err
		}
		if err := writer.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: reporterUser, Email: "reporter@example.test", MembershipID: reporterMembership, Role: "reporter", Active: true, PrincipalID: reporterID, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		for _, grantedProject := range []tracker.ProjectID{project, sharedProject} {
			if err := writer.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: reporterUser, ProjectID: grantedProject}); err != nil {
				return err
			}
			if err := writer.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: reporterID, ProjectID: grantedProject}); err != nil {
				return err
			}
		}
		items := []corestore.WorkItem{
			{OrganizationID: organization, ProjectID: project, NativeID: "wi_source_one", Number: 10, Title: "Source one", ActorKind: new("human"), ActorPrincipalID: new(reporterID)},
			{OrganizationID: organization, ProjectID: project, NativeID: "wi_source_two", Number: 11, Title: "Source two", ActorKind: new("human"), ActorPrincipalID: new(reporterID)},
			{OrganizationID: organization, ProjectID: project, NativeID: "wi_visible_one", Number: 12, Title: "Visible blocker", ActorKind: new("human"), ActorPrincipalID: new(reporterID)},
			{OrganizationID: organization, ProjectID: project, NativeID: "wi_visible_two", Number: 13, Title: "Second blocker", ActorKind: new("human"), ActorPrincipalID: new(reporterID)},
			{OrganizationID: organization, ProjectID: privateProject, NativeID: "wi_private", Number: 1, Title: "Private blocker", ActorKind: new("human"), ActorPrincipalID: new(otherID)},
			{OrganizationID: organization, ProjectID: sharedProject, NativeID: "wi_wrong_reporter", Number: 1, Title: "Other reporter blocker", ActorKind: new("human"), ActorPrincipalID: new(otherID)},
		}
		for _, item := range items {
			if _, err := writer.CreateWorkItem(t.Context(), item); err != nil {
				return err
			}
		}
		for _, edge := range []corestore.Relationship{
			{OrganizationID: organization, ProjectID: project, WorkItemID: "wi_source_one", RelatedProjectID: project, RelatedWorkItemID: "wi_visible_one", Type: "dependency"},
			{OrganizationID: organization, ProjectID: project, WorkItemID: "wi_source_one", RelatedProjectID: privateProject, RelatedWorkItemID: "wi_private", Type: "dependency"},
			{OrganizationID: organization, ProjectID: project, WorkItemID: "wi_source_two", RelatedProjectID: project, RelatedWorkItemID: "wi_visible_two", Type: "dependency"},
			{OrganizationID: organization, ProjectID: project, WorkItemID: "wi_source_two", RelatedProjectID: sharedProject, RelatedWorkItemID: "wi_wrong_reporter", Type: "dependency"},
		} {
			if err := writer.SetRelationship(t.Context(), edge, "add", fixture.service.config.now()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	newIssue := func(id tracker.NativeWorkItemID) *tracker.NativeIssue {
		return &tracker.NativeIssue{
			NativeReference: tracker.NativeReference{WorkItemID: id},
			BlockedReason:   tracker.DependencyReason{Basis: "previous-basis", Text: "private explanation"},
		}
	}
	first, second, duplicate := newIssue("wi_source_one"), newIssue("wi_source_two"), newIssue("wi_source_one")
	scope := nativeScope{
		organization: organization,
		project:      project,
		credential:   apiCredential{ID: reporterID, Hash: "example-hash", Scope: apiScopeOperator, Hosted: &auth.HostedIdentity{Subject: reporterUser, OrganizationID: "org_provider", SessionID: "session_reporter", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, HostedRole: "reporter", HostedMembership: reporterMembership},
		sourceActor:  &tracker.Actor{PrincipalID: reporterID},
	}
	if err := fixture.service.hydrateWorkItemRelationshipFactsPage(t.Context(), scope, []*tracker.NativeIssue{first, second, duplicate}); err != nil {
		t.Fatal(err)
	}
	if tenantReads.reads != 1 || tenantReads.batches != 1 || accessReads.reads != 1 {
		t.Fatalf("relationship page tenant reads=%d batch queries=%d access reads=%d, want 1 each", tenantReads.reads, tenantReads.batches, accessReads.reads)
	}
	for _, issue := range []*tracker.NativeIssue{first, duplicate} {
		if len(issue.Dependencies) != 1 || issue.Dependencies[0] != "wi_visible_one" || len(issue.Blockers) != 1 || issue.Blockers[0].ID != "wi_visible_one" {
			t.Fatalf("reporter received private cross-project dependency: %+v", issue)
		}
		if issue.BlockedReason.CurrentBasis != "" || issue.BlockedReason.Text != "" {
			t.Fatalf("private dependency basis leaked: %+v", issue.BlockedReason)
		}
	}
	if len(second.Dependencies) != 1 || second.Dependencies[0] != "wi_visible_two" || second.BlockedReason.CurrentBasis != "" || second.BlockedReason.Text != "" {
		t.Fatalf("second entity relationship facts=%+v", second)
	}
}
