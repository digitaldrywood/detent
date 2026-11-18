package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func newExportPostgresFixture(t *testing.T) (tenantstore.Repository, hostedSecurityFixture, hostedSecurityUser, *sql.DB) {
	t.Helper()
	return newExportPostgresFixtureInOrganization(t, postgresStartupTemplate.Database(t), "org_security")
}

func newExportPostgresFixtureInOrganization(t *testing.T, admin *sql.DB, organization tracker.OrganizationID, configure ...func(*Config)) (tenantstore.Repository, hostedSecurityFixture, hostedSecurityUser, *sql.DB) {
	t.Helper()
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	cfg.Hosted.OrganizationID = string(organization)
	cfg.Hosted.WorkOSOrganizationID = "org_provider"
	plans := hostedplans.PilotPlans()
	cfg.Hosted.Plans = &plans
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	now := time.Now().UTC().Truncate(time.Microsecond)
	cfg.Clock = func() time.Time { return now }
	for _, apply := range configure {
		apply(&cfg)
	}
	now = cfg.Clock()
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	project := tracker.ProjectID("project_example")
	policy, err := eventstore.NewContent(cfg.RunDetails).PrepareProjectPolicy(t.Context(), organization, hubTestPolicy())
	if err != nil {
		t.Fatal(err)
	}
	policy.ProjectID, policy.ApprovedBy, policy.ApprovedAt = project, "example-owner", now
	if err := store.postgres.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", CreatedAt: now}); err != nil {
			return err
		}
		deployment, generation := cfg.Hosted.Deployment()
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: cfg.Hosted.WorkOSOrganizationID, BootstrapSubject: cfg.Hosted.BootstrapSubject, Deployment: deployment, PublicURL: cfg.Hosted.PublicURL, AllocationGeneration: generation}); err != nil {
			return err
		}
		cursor, err := cfg.SecretKeys.Seal(bytes.Repeat([]byte{0x36}, 32), accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			return err
		}
		if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: cursor, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: cursor.Version, RecordedAt: now}); err != nil {
			return err
		}
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
			return err
		}
		return w.ApproveProjectPolicy(t.Context(), policy, "")
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	f := hostedSecurityFixture{service: service, provider: cfg.Hosted.Provider.(*hostedSecurityProvider), project: project, base: "/api/v2/organizations/" + string(organization) + "/projects/" + string(project)}
	owner := f.user(t, "review_admin", "owner", "review-admin@example.test", "", "")
	repository := service.config.TenantRepository
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), owner.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: member.UserID, ProjectID: project, CanWrite: true}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: member.PrincipalID, ProjectID: project})
	}); err != nil {
		t.Fatal(err)
	}
	return repository, f, owner, admin
}

func newPostgresExportLinkedFixture(t *testing.T) (tenantstore.Repository, hostedSecurityFixture, hostedSecurityUser, map[tracker.NativeWorkItemID]string, map[tracker.NativeWorkItemID]string) {
	t.Helper()
	repository, f, owner, admin := newExportPostgresFixture(t)
	expected := map[tracker.NativeWorkItemID]string{}
	changes := map[tracker.NativeWorkItemID]string{}
	for _, name := range []string{"first", "second"} {
		response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: name}, Title: name, Body: workItemRecordBody, State: "Todo"})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		expected[issue.WorkItemID] = name
		response = f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment-" + name}, Body: name + " discussion"})
		requireNativeStatus(t, response, http.StatusOK)
		response = f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change-" + name}, Title: name + " change", Body: "Change detail"})
		requireNativeStatus(t, response, http.StatusOK)
		var change tracker.ChangeRequest
		decodeHubResponse(t, response, &change)
		changes[issue.WorkItemID] = change.ID
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO release_deliveries(organization_id,project_id,work_item_id,source_issue_id,source_change_id,source_merge_sha,repository,pr_number,head_ref,head_sha,base_ref,base_sha,frozen_sha,change_id,principal_id,actor_kind,via_key,associated_at) VALUES('org_security',$1,$2,$2,$3,'source','https://github.com/example-org/example-project',$4,'fix','head','release/v1.1','base','frozen',$3,'example','human','',now())`, f.project, issue.WorkItemID, change.ID, len(expected)); err != nil {
			t.Fatal(err)
		}
	}
	return repository, f, owner, expected, changes
}

type pullRequestRefreshBackend func(context.Context, ReconcileRequest) (ReconcileSnapshot, error)

func (f pullRequestRefreshBackend) Reconcile(ctx context.Context, request ReconcileRequest) (ReconcileSnapshot, error) {
	return f(ctx, request)
}

const pullRequestTestURL = "https://github.com/example/repo/pull/1"
