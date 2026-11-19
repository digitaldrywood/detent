package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"errors"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeClaimAndReleaseCheckRunnerAuthority(t *testing.T) {
	t.Parallel()
	f := newRunnerPGFixture(t)

	for index, binding := range f.bindings {
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.ReplaceRunnerBackendIsolation(t.Context(), binding.RunnerID, isolation.Report{"codex": {isolation.NativeTrusted, isolation.Sandbox}}); err != nil {
				return err
			}
			return w.UpdateRunnerCapacityConfig(t.Context(), binding.RunnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, ObservedAt: f.service.config.Clock().UTC()})
		}); err != nil {
			t.Fatal(err)
		}
		var version eventstore.CollaborationVersion
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			version, err = r.CollaborationVersion(t.Context(), string(f.leases[index].WorkItemID), 1)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		content := eventstore.NewContent(f.service.runDetailStore())
		raw, err := content.CollaborationVersion(t.Context(), version)
		if err != nil {
			t.Fatal(err)
		}
		var issue tracker.NativeIssue
		if err := json.Unmarshal(raw, &issue); err != nil {
			t.Fatal(err)
		}
		issue.IssueContract = &issuecontract.State{Exempt: true}
		issue.Revision++
		version.Revision++
		raw, err = json.Marshal(issue)
		if err != nil {
			t.Fatal(err)
		}
		version, err = content.PrepareCollaborationVersion(t.Context(), version, raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			item, err := w.WorkItem(t.Context(), "prj_runner", f.leases[index].WorkItemID)
			if err != nil {
				return err
			}
			if _, err := w.UpdateWorkItem(t.Context(), item); err != nil {
				return err
			}
			_, err = w.CreateCollaborationVersion(t.Context(), version)
			return err
		}); err != nil {
			t.Fatal(err)
		}

		claimPath := "/api/v2/organizations/org_startup/projects/prj_runner/claims"
		claim := tracker.NativeClaim{PolicyID: f.policy.PolicyID, WorkItemID: f.leases[index].WorkItemID, MachineID: binding.MachineID, SessionID: f.leases[index].SessionID, TTLSeconds: 300, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
		releasePath := "/api/v2/organizations/org_startup/projects/prj_runner/leases/" + string(f.leases[index].ID) + "/release"
		for _, operation := range []struct {
			path   string
			body   any
			status int
		}{
			{claimPath, claim, http.StatusOK},
			{releasePath, tracker.NativeLeaseMutation{FencingToken: f.leases[index].FencingToken, Reason: "released"}, http.StatusNoContent},
		} {
			response := httptest.NewRecorder()
			request := newHubAPIRequest(t, http.MethodPost, operation.path, f.tokens[index], operation.body)
			f.service.Handler().ServeHTTP(response, request)
			requireNativeStatus(t, response, operation.status)
		}
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE api_tokens SET revoked_at=clock_timestamp() WHERE organization_id='org_startup' AND id=$1`, fmt.Sprintf("credential_%d", index+1)); err != nil {
			t.Fatal(err)
		}
		denied := httptest.NewRecorder()
		deniedRequest := newHubAPIRequest(t, http.MethodPost, claimPath, f.tokens[index], claim)
		f.service.Handler().ServeHTTP(denied, deniedRequest)
		requireNativeStatus(t, denied, http.StatusUnauthorized)
	}
}

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
