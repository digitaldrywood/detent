package runnerruntime_test

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestPostgresBootstrapWorkspaceRespectsTenantScope(t *testing.T) {
	t.Parallel()
	const terminalOrg = hubfixture.StartupOrganization
	admin := hubtest.Database(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	public, signer, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	type tenant struct {
		hub   *hubfixture.Hub
		owner hubtest.SeededMember
	}
	var tenants []tenant
	for _, org := range []tracker.OrganizationID{terminalOrg, "org_other"} {
		cfg := hubfixture.StartupConfig()
		cfg.Clock = func() time.Time { return now }
		cfg.Hosted.OrganizationID = string(org)
		cfg.Hosted.WorkOSOrganizationID = "provider_" + string(org)
		cfg.Hosted.SharedEntry = &hostedplans.HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{public}, Generation: 1}
		plans := hostedplans.PilotPlans()
		cfg.Hosted.Plans = &plans
		cfg.Workspace = &workspacehub.Config{Enabled: true, Terminal: workspacehub.TerminalConfig{Enabled: true}}
		hub := hubfixture.OpenStartupOn(t, admin, cfg)
		project := hubtest.Project(t, hub.Repository, org, hubtest.ProjectOptions{ID: "project_shared", Name: "Example"})
		projectIDs := []tracker.ProjectID{project.ID}
		owner := hubtest.Member(t, hub.Repository, org, hubtest.MemberOptions{Role: "owner", Projects: projectIDs, ManageRunners: true, Now: now})
		for range 2 {
			runner := hubtest.Runner(t, hub.Repository, org, hubtest.RunnerOptions{Projects: projectIDs, Now: now})
			caps := workspacesession.Capabilities{Files: true, Exec: true, Terminal: org == terminalOrg}
			if err := hub.Repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.ReplaceRunnerWorkspaceReport(t.Context(), runner.Identity.RunnerID, caps, workspacesession.IsolationContainer)
			}); err != nil {
				t.Fatal(err)
			}
		}
		tenants = append(tenants, tenant{hub: hub, owner: owner})
	}
	for _, tenant := range tenants {
		hub, owner := tenant.hub, tenant.owner.Member
		t.Run(string(hub.Primary), func(t *testing.T) {
			org := string(hub.Primary)
			path := "/organizations/" + org + "/app/bootstrap"
			id, err := cloudassert.NewID()
			if err != nil {
				t.Fatal(err)
			}
			const session = "session"
			binding := cloudassert.AuthorizationBinding("shared-"+owner.UserID, org, session)
			claims := cloudassert.Claims{
				Issuer: "entry", Audience: org, Generation: 1, Kind: cloudassert.KindBrowser,
				Method: http.MethodGet, Path: path, BodyDigest: cloudassert.BodyDigest(nil),
				IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), ID: id,
				Subject: owner.UserID, Email: owner.Email, Role: owner.Role,
				ProviderOrganization: hub.Config.Hosted.WorkOSOrganizationID, ProviderSession: session,
				SessionCreatedAt: now, SessionExpiresAt: now.Add(time.Hour), AccessExpiresAt: now.Add(time.Hour),
				Binding: binding, CSRF: cloudassert.CSRFToken("shared-"+owner.UserID, org),
			}
			assertion, err := cloudassert.Sign(signer, claims)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
			request.Header.Set(cloudassert.Header, assertion)
			response := httptest.NewRecorder()
			hub.Handler().ServeHTTP(response, request)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var payload struct {
				Feature struct {
					Workspaces bool `json:"workspaces"`
				} `json:"feature"`
				Plan     *json.RawMessage `json:"plan"`
				Projects []struct {
					ID           string                        `json:"id"`
					Capabilities workspacesession.Capabilities `json:"capabilities"`
				} `json:"projects"`
			}
			hubfixture.Decode(t, response, &payload)
			if !payload.Feature.Workspaces || payload.Plan == nil || len(payload.Projects) != 1 || payload.Projects[0].ID != "project_shared" ||
				!payload.Projects[0].Capabilities.Files || payload.Projects[0].Capabilities.Terminal != (hub.Primary == terminalOrg) {
				t.Fatalf("tenant %s bootstrap capabilities or plan incorrect: %s", org, response.Body.String())
			}
		})
	}
}
