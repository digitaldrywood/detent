package hubserver

import (
	"errors"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tenantauth"
)

func TestRunnerAuthorityAndScopedReads(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, newPostgresNativeService(t, Config{}))
	f.publish(t, "current", "")
	other := f.create(t, "related")
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}
	edge := corestore.Relationship{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: f.issue.WorkItemID, RelatedProjectID: scope.Project, RelatedWorkItemID: other.WorkItemID, Type: "related"}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetRelationship(t.Context(), edge, "add", f.service.config.Clock())
	}); err != nil {
		t.Fatal(err)
	}
	r := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim)
	r.enroll(t)
	ctx := t.Context()
	repository, ok := f.service.config.TenantRepository.(*postgres.Repository)
	if !ok {
		t.Fatal("runner authority requires the PostgreSQL owner")
	}
	authority, capacity, err := repository.ReadRunnerAuthority(ctx, tenantauth.RequestIdentity{Organization: scope.Organization, CredentialHash: apikey.HashToken(r.redemption.Credential)}, f.service.config.Clock)
	if err != nil || authority.RunnerID() != r.binding.RunnerID || capacity < 1 {
		t.Fatalf("runner authority lost identity/capacity: runner=%s capacity=%d error=%v", authority.RunnerID(), capacity, err)
	}
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), scope.Organization, apikey.HashToken(r.redemption.Credential), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("runner auth status=%d error=%v", status, err)
	}
	if err := f.service.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		page, err := reader.Relationships(ctx, scope.Project, f.issue.WorkItemID, corestore.RelationshipCursor{}, corestore.MaxPageSize)
		if err != nil {
			return err
		}
		if len(page.Items) != 1 || page.Items[0].RelatedWorkItemID != other.WorkItemID || page.Items[0].Type != "related" {
			t.Fatalf("scoped related issues=%+v", page.Items)
		}
		_, err = reader.Receipt(ctx, credential.ID, "POST /example", "missing")
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("missing command receipt: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
