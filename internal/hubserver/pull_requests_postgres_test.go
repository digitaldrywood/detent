package hubserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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

func TestPostgresLinkedChanges(t *testing.T) {
	repository, f, owner, expected, changes := newPostgresExportLinkedFixture(t)
	t.Run("linked", func(t *testing.T) {
		for id, name := range expected {
			response := f.request(t, owner, http.MethodGet, f.base+"/work-items/"+string(id)+"/pull-requests", nil)
			requireNativeStatus(t, response, http.StatusOK)
			var views []pullRequestView
			decodeHubResponse(t, response, &views)
			if len(views) != 1 || views[0].Title != name+" change" || views[0].Number < 1 || views[0].Head.SHA != "head" || views[0].Base.Ref != "release/v1.1" || views[0].Connector != nil || views[0].Checks == nil || views[0].Reviews == nil || views[0].Labels == nil {
				t.Fatalf("linked changes=%+v", views)
			}
		}
	})
	t.Run("current_version", func(t *testing.T) {
		for id, changeID := range changes {
			for number := int64(1); number <= 2; number++ {
				input := changeTestInput()
				input.AttemptID = "attempt_" + string(id)
				if number == 2 {
					input.External = &tracker.ChangeExternalReference{Provider: "github", ID: "42", URL: "https://github.com/example-org/example-project/pull/42"}
				}
				content := eventstore.NewContent(f.service.runDetailStore())
				source, err := content.PrepareSourceBundle(t.Context(), "org_security", changeTestSource(t, &input))
				if err != nil {
					t.Fatal(err)
				}
				version := tracker.ChangeVersion{ID: fmt.Sprintf("version_%s_%d", id, number), ChangeID: changeID, Number: number, ChangeVersionInput: input, Policy: hubTestPolicy(), ReviewPolicy: defaultChangeReviewPolicy(hubTestPolicy()), Actor: tracker.Actor{Kind: "human", PrincipalID: owner.identity.Subject}, CreatedAt: f.service.config.now()}
				version.PolicyID = version.Policy.ID
				raw, err := json.Marshal(version)
				if err != nil {
					t.Fatal(err)
				}
				row, err := content.PrepareVersion(t.Context(), "org_security", raw)
				if err != nil {
					t.Fatal(err)
				}
				row.SourceObjectKey, row.SourceSHA256, row.SourceBytes, row.SourceContentBytes = source.Key, source.SHA256, source.CompressedSize, source.UncompressedSize
				if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.CreateVersion(t.Context(), row); err != nil {
						return err
					}
					change, err := w.Change(t.Context(), changeID)
					if err != nil {
						return err
					}
					change.CurrentVersionID = new(version.ID)
					_, err = w.UpdateChange(t.Context(), change)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				f.service.pullRequests.invalidate(pullRequestCacheKey(nativeScope{organization: "org_security", project: f.project}, string(id)))
				response := f.request(t, owner, http.MethodGet, f.base+"/work-items/"+string(id)+"/pull-requests", nil)
				requireNativeStatus(t, response, http.StatusOK)
				var views []pullRequestView
				decodeHubResponse(t, response, &views)
				if len(views) != 1 || views[0].Head.SHA != input.HeadSHA || views[0].Base.SHA != input.BaseSHA || views[0].Author != owner.identity.Subject {
					t.Fatalf("current source lost: %+v", views)
				}
				if number == 2 && (views[0].Number != 42 || views[0].URL != input.External.URL) {
					t.Fatalf("external reference lost: %+v", views)
				}
			}
		}
	})
	t.Run("refresh", func(t *testing.T) {
		now := f.service.config.now()
		f.service.config.now = func() time.Time { return now }
		for id := range expected {
			path := f.base + "/work-items/" + string(id) + "/pull-requests"
			requireNativeStatus(t, f.request(t, owner, http.MethodGet, path+"?refresh=1", nil), http.StatusOK)
			requireNativeCode(t, f.request(t, owner, http.MethodGet, path+"?refresh=1", nil), http.StatusTooManyRequests, "refresh_limited")
			now = now.Add(pullRequestRefreshInterval)
			requireNativeStatus(t, f.request(t, owner, http.MethodGet, path+"?refresh=1", nil), http.StatusOK)
			requireNativeCode(t, f.request(t, owner, http.MethodGet, path+"?refresh=2", nil), http.StatusUnprocessableEntity, "invalid_request")
			now = now.Add(pullRequestRefreshInterval)
		}
	})

}

func newExportPostgresFixture(t *testing.T) (tenantstore.Repository, hostedSecurityFixture, hostedSecurityUser, *sql.DB) {
	t.Helper()
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	cfg.Hosted.OrganizationID = "org_security"
	cfg.Hosted.WorkOSOrganizationID = "org_provider"
	plans := pilotHostedPlans()
	cfg.Hosted.Plans = &plans
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	now := time.Now().UTC()
	cfg.now = func() time.Time { return now }
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	project := tracker.ProjectID("project_example")
	policy, err := eventstore.NewContent(cfg.RunDetails).PrepareProjectPolicy(t.Context(), "org_security", hubTestPolicy())
	if err != nil {
		t.Fatal(err)
	}
	policy.ProjectID, policy.ApprovedBy, policy.ApprovedAt = project, "example-owner", now
	if err := store.postgres.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_security", Name: "Example", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_security", ProviderID: "org_provider", AllocationGeneration: 1}); err != nil {
			return err
		}
		cursor, err := cfg.SecretKeys.Seal(bytes.Repeat([]byte{0x36}, 32), accessstore.SecretAAD("org_security", "", accessstore.NativeCursorKeyKind))
		if err != nil {
			return err
		}
		if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: "org_security", Kind: accessstore.NativeCursorKeyKind, Envelope: cursor, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: "org_security", Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: cursor.Version, RecordedAt: now}); err != nil {
			return err
		}
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_security", ID: project, Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
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
	f := hostedSecurityFixture{service: service, provider: cfg.Hosted.Provider.(*hostedSecurityProvider), project: project, base: "/api/v2/organizations/org_security/projects/" + string(project)}
	owner := f.user(t, "review_admin", "owner", "review-admin@example.test", "", "")
	repository := service.config.TenantRepository
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), owner.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_security", UserID: member.UserID, ProjectID: project, CanWrite: true}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_security", TokenID: member.PrincipalID, ProjectID: project})
	}); err != nil {
		t.Fatal(err)
	}
	return repository, f, owner, admin
}
