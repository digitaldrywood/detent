package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/runnerauth"
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
