package hubserver

import (
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestArtifactProjectRetirementCommitsBoundedProgress(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 4)
	type fixture struct {
		hostedSecurityFixture
		owner hostedSecurityUser
	}
	var fixtures []fixture
	for range 2 {
		org := tracker.OrganizationID(artifact.NewID("org"))
		f, owner := newChangePolicyPostgresTenantFixture(t, repository, org, &memoryRunDetails{}, tracker.ProjectID(artifact.NewID("prj")))
		if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
			t.Fatal(err)
		}
		now := f.service.config.now().UTC().Truncate(time.Microsecond)
		created := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "retirement-item"}, Title: "Example retained artifact", Body: workItemRecordBody, State: "Todo"})
		requireNativeStatus(t, created, http.StatusOK)
		var item tracker.NativeIssue
		decodeHubResponse(t, created, &item)
		publisher, service := artifact.NewID("publisher"), artifact.NewID("service")
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: publisher, Name: "Example publisher", Hash: artifact.Digest([]byte(publisher)), Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, ProjectID: f.project, TokenID: publisher}); err != nil {
				return err
			}
			if err := w.BindArtifactService(t.Context(), f.project, artifact.Binding{ServiceID: service, Origin: "https://example.invalid", Mode: "customer", PublisherTokenID: publisher}); err != nil {
				return err
			}
			reference, err := w.AppendArtifactReference(t.Context(), artifact.Reference{SchemaVersion: 1, Scope: artifact.Scope{OrganizationID: string(org), ProjectID: string(f.project), WorkItemID: string(item.WorkItemID), RunID: artifact.NewID("run"), AttemptID: artifact.NewID("attempt")}, ServiceID: service, ArtifactID: artifact.NewID("artifact"), ManifestID: artifact.NewID("manifest"), Revision: 1, SHA256: artifact.Digest([]byte("example")), Kind: "log", State: "complete", Availability: "available", ExpiresAt: now.Add(time.Hour), ObservedAt: now})
			if err != nil {
				return err
			}
			for index := range 33 {
				if err := w.CreateArtifactReadGrant(t.Context(), artifactstore.ReadGrant{ProjectID: f.project, WorkItemID: item.WorkItemID, TokenHash: artifact.Digest([]byte(string(org) + "/" + strconv.Itoa(index))), PrincipalID: publisher, ServiceID: service, ArtifactID: reference.ArtifactID, Revision: 1, ExpiresAt: now.Add(time.Hour)}, now); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, fixture{f, owner})
	}
	for index, f := range fixtures {
		org := tracker.OrganizationID(f.service.config.Hosted.OrganizationID)
		key := "retire-artifact-project"
		request := deleteCloudProjectRequest{Mutation: tracker.Mutation{IdempotencyKey: key}, ConfirmName: "Example project"}
		wrong := request
		wrong.ConfirmName = "Different name"
		requireNativeStatus(t, f.request(t, f.owner, http.MethodDelete, f.base, wrong), http.StatusUnprocessableEntity)
		var before int
		if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM artifact_grants WHERE organization_id=$1 AND project_id=$2", org, f.project).Scan(&before); err != nil || before != 33 {
			t.Fatalf("rejected confirmation changed grants: count=%d error=%v", before, err)
		}
		response := f.request(t, f.owner, http.MethodDelete, f.base, request)
		requireNativeStatus(t, response, http.StatusConflict)
		var grants, receipts int
		if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM artifact_grants WHERE organization_id=$1 AND project_id=$2),(SELECT count(*) FROM native_commands WHERE organization_id=$1 AND command_key=$3)", org, f.project, key).Scan(&grants, &receipts); err != nil || grants != 1 || receipts != 0 {
			t.Fatalf("pending cleanup lost progress or committed final receipt: grants=%d receipts=%d error=%v", grants, receipts, err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			project, err := r.ProjectRecord(t.Context(), f.project)
			if err == nil && project.Core.DeletedAt != nil {
				t.Fatal("pending cleanup retired the project")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, f.request(t, f.owner, http.MethodDelete, f.base, request), http.StatusOK)
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.ProjectRecord(t.Context(), f.project)
			return err
		}); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("finished cleanup retained an active project: %v", err)
		}
		if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE organization_id=$1 AND command_key=$2", org, key).Scan(&receipts); err != nil || receipts != 1 {
			t.Fatalf("completed retirement receipts=%d error=%v", receipts, err)
		}
		for _, other := range fixtures[index+1:] {
			otherOrg := tracker.OrganizationID(other.service.config.Hosted.OrganizationID)
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM artifact_grants WHERE organization_id=$1 AND project_id=$2", otherOrg, other.project).Scan(&grants); err != nil || grants != 33 {
				t.Fatalf("cleanup crossed tenant boundary: grants=%d error=%v", grants, err)
			}
		}
	}
}
