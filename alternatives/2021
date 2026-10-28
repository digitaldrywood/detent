package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSpriteIdleSharingCommands(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO organizations(id,name,created_at) VALUES('org_other_sharing','Other organization',?)`, testTimestamp); err != nil {
		t.Fatal(err)
	}
	other := newNativeFixture(t, f.service, "org_other_sharing", "other-organization")
	contexts := make(chan context.Context, 1)
	f.service.echo.GET("/api/v2/organizations/:organization/sprite-sharing-context", func(c echo.Context) error {
		contexts <- c.Request().Context()
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	for _, fixture := range []nativeFixture{f, other} {
		var before spritePoolView
		decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, fixture.base+"/sprite-pool", testHubAdminToken, nil), &before)
		if before.ShareIdleCapacity {
			t.Fatal("new pools share paid capacity by default")
		}
		for _, test := range []struct {
			name, key string
			revision  int64
			share     *bool
			status    int
		}{
			{"enable", "enable", 0, new(true), http.StatusOK},
			{"replay", "enable", 0, new(true), http.StatusOK},
			{"key conflict", "enable", 0, new(false), http.StatusConflict},
			{"stale revision", "stale", 0, new(false), http.StatusConflict},
			{"missing key", "", 1, new(false), http.StatusUnprocessableEntity},
			{"operator denied", "operator", 1, new(false), http.StatusForbidden},
			{"omitted preserves", "", 1, nil, http.StatusOK},
			{"disable", "disable", 2, new(false), http.StatusOK},
			{"old replay after later writes", "enable", 0, new(true), http.StatusOK},
		} {
			t.Run(string(fixture.project.OrganizationID)+"/"+test.name, func(t *testing.T) {
				input := map[string]any{"min_runners": 0, "max_runners": 0, "revision": test.revision}
				if test.share != nil {
					input["share_idle_capacity"] = *test.share
				}
				if test.key != "" {
					input["idempotency_key"] = test.key
				}
				token := testHubAdminToken
				if test.name == "operator denied" {
					token = fixture.token
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, fixture.base+"/sprite-pool", token, input), test.status)
				var saved spritePoolView
				decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, fixture.base+"/sprite-pool", testHubAdminToken, nil), &saved)
				wantRevision, wantSharing := int64(1), true
				if test.name == "omitted preserves" {
					wantRevision = 2
				}
				if test.name == "disable" || test.name == "old replay after later writes" {
					wantRevision, wantSharing = 3, false
				}
				if saved.Revision != wantRevision || saved.ShareIdleCapacity != wantSharing {
					t.Fatalf("saved=%+v", saved)
				}
			})
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations/"+string(fixture.project.OrganizationID)+"/sprite-sharing-context", testHubAdminToken, nil), http.StatusOK)
		ctx := <-contexts
		executor := hubProjectExecutor{f.service}
		for _, test := range []struct {
			name, key    string
			revision     int64
			sharing      *bool
			conflict     bool
			wantRevision int64
			wantSharing  bool
		}{
			{"enable", "tool-enable", 3, new(true), false, 4, true},
			{"disable", "tool-disable", 4, new(false), false, 5, false},
			{"replay after later write", "tool-enable", 3, new(true), false, 5, false},
			{"changed input", "tool-enable", 3, new(false), true, 5, false},
			{"stale revision", "tool-stale", 3, new(true), true, 5, false},
			{"omitted preserves", "tool-omit", 5, nil, false, 6, false},
		} {
			t.Run(string(fixture.project.OrganizationID)+"/tool/"+test.name, func(t *testing.T) {
				call := projectCall(t, operatortool.UpdateSpritePool, string(fixture.project.ID), test.key, operatortool.SpritePoolInput{ShareIdleCapacity: test.sharing, Revision: test.revision})
				_, err := executor.command(ctx, call, projectCommandExecute)
				if (err != nil) != test.conflict {
					t.Fatalf("tool conflict=%t, error=%v", test.conflict, err)
				}
				if !test.conflict {
					raw, err := executor.command(ctx, call, projectCommandReplay)
					if err != nil || len(raw) == 0 {
						t.Fatalf("tool replay=%s, error=%v", raw, err)
					}
				}
				var saved spritePoolView
				decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, fixture.base+"/sprite-pool", testHubAdminToken, nil), &saved)
				if saved.Revision != test.wantRevision || saved.ShareIdleCapacity != test.wantSharing {
					t.Fatalf("saved=%+v", saved)
				}
			})
		}
	}
}

func TestHostedSpriteIdleSharingCommand(t *testing.T) {
	f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false)
	for _, test := range []struct {
		role   string
		status int
	}{
		{"viewer", http.StatusForbidden},
		{"owner", http.StatusOK},
		{"owner", http.StatusOK},
	} {
		t.Run(test.role, func(t *testing.T) {
			cookie := f.cookies[test.role]
			if cookie == nil {
				t.Fatal("missing session")
			}
			response := f.rawAPI(t, test.role, http.MethodPut, "/api/v2/organizations/org_browser_preview/sprite-pool", `{"idempotency_key":"share","revision":0,"min_runners":0,"max_runners":0,"share_idle_capacity":true}`, map[string]string{"X-CSRF-Token": hostedCSRF(cookie.Value)})
			browserHostedStatus(t, response, test.status)
		})
	}
	var sharing bool
	var revision int
	if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT share_idle_capacity,revision FROM organization_sprite_pools WHERE organization_id='org_browser_preview'`).Scan(&sharing, &revision); err != nil {
		t.Fatal(err)
	}
	if !sharing || revision != 1 {
		t.Fatalf("sharing=%t revision=%d", sharing, revision)
	}
}

func TestProjectSpriteIdleSharingClaims(t *testing.T) {
	for _, test := range []struct {
		name                                       string
		sharing, ownReady, granted, ready, managed bool
		budget                                     string
		status                                     int
	}{
		{"default stays isolated", false, false, true, true, false, "", http.StatusConflict},
		{"opted in shares", true, false, true, true, false, "", http.StatusOK},
		{"highest ranked fallback", true, false, true, true, false, "", http.StatusConflict},
		{"own ready work takes precedence", true, true, true, true, false, "", http.StatusConflict},
		{"shared merging beats own todo", true, true, true, true, false, "", http.StatusOK},
		{"own budget exhaustion permits sharing", true, true, true, true, false, "own monthly", http.StatusOK},
		{"grants remain required", true, false, false, true, false, "", http.StatusNotFound},
		{"target checkout remains required", true, false, true, false, false, "", http.StatusConflict},
		{"sharing to another allocation", true, false, true, true, true, "", http.StatusOK},
		{"provider exhaustion", true, false, true, true, false, "provider", http.StatusConflict},
		{"monthly exhaustion", true, false, true, true, false, "monthly", http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			var other nativeFixture
			var landing landingCapacityFixture
			if test.name == "shared merging beats own todo" {
				landing = newLandingCapacityFixture(t, f.service, "other-landings", false)
				other = landing.nativeFixture
			} else {
				other = newNativeFixture(t, f.service, f.project.OrganizationID, "other-project")
			}
			for _, project := range []nativeFixture{f, other} {
				approveHubTestPolicy(t, f.service, project.base+"/policy", hubTestPolicy())
			}
			var ranked nativeFixture
			if test.name == "highest ranked fallback" {
				ranked = newNativeFixture(t, f.service, f.project.OrganizationID, "ranked-project")
				approveHubTestPolicy(t, f.service, ranked.base+"/policy", hubTestPolicy())
			}
			setPlacementFixture(t, f, policy.Placement{Mode: "blended"}, 4)
			setSpriteAllocationFixture(t, f, 1, 0)
			if test.managed {
				setSpriteAllocationFixture(t, other, 1, 0)
			}
			r := placementFixtureRunner(t, f, "project-sprite", "sharing", true, 1, "test-model", "gpt-6.1-sol")
			if test.granted {
				routing := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Project Sprite", State: "active", CapacityLimit: 1, ProjectIDs: []tracker.ProjectID{f.project.ID, other.project.ID}}}
				if ranked.service != nil {
					routing.ProjectIDs = append(routing.ProjectIDs, ranked.project.ID)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing), http.StatusOK)
			}
			now := formatHubTime(f.service.config.now())
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO organization_sprite_members(organization_id,allocation_project_id,name,provider_organization,enrollment_id,state,idle_since,created_at) VALUES(?,?,?,'test',?,'enrolled',?,?)`, f.project.OrganizationID, f.project.ID, r.redemption.Hostname, r.enrollment.ID, now, now); err != nil {
				t.Fatal(err)
			}
			setSpriteMemberChecks(t, f, spriteMemberReady)
			if test.ready {
				raw, err := json.Marshal(spriteMemberReady)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE runner_identities SET local_checks_json=json_set(local_checks_json,?,json(?)) WHERE id=?`, "$."+jsonPathKey(string(other.project.ID)), string(raw), r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE organization_sprite_pools SET share_idle_capacity=? WHERE organization_id=?`, test.sharing, f.project.OrganizationID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE projects SET scheduling_rank=CASE WHEN id=? THEN 1 ELSE 10 END WHERE organization_id=?`, other.project.ID, f.project.OrganizationID); err != nil {
				t.Fatal(err)
			}
			var own tracker.NativeIssue
			if test.ownReady {
				own = f.create(t, "own ready work")
			}
			var foreign tracker.NativeIssue
			if landing.service != nil {
				foreign = landing.ready(t, "reviewed shared landing")
			} else {
				foreign = other.create(t, "other ready work")
			}
			var rankedIssue tracker.NativeIssue
			if ranked.service != nil {
				rankedIssue = ranked.create(t, "highest ranked fallback")
				raw, err := json.Marshal(spriteMemberReady)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE runner_identities SET local_checks_json=json_set(local_checks_json,?,json(?)) WHERE id=?`, "$."+jsonPathKey(string(ranked.project.ID)), string(raw), r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE projects SET scheduling_rank=0 WHERE id=?`, ranked.project.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.budget == "provider" {
				report := capacityReport(f.service.config.now())
				report.Availability = "exhausted"
				publishCapacity(t, f, r, report)
			}
			if test.budget == "monthly" {
				setMonthlyBudgetFixture(t, other, false, budget.MonthlyPolicy{Enabled: true, SpriteMicros: new(int64)})
			}
			if test.budget == "own monthly" {
				setMonthlyBudgetFixture(t, f, false, budget.MonthlyPolicy{Enabled: true, SpriteMicros: new(int64)})
			}
			claim := providerClaim(r, foreign, "sharing")
			if test.ownReady {
				claim.WorkItemID = ""
				claim.ProviderCandidates = append(claim.ProviderCandidates, providerClaim(r, own, "").ProviderCandidates[0])
			}
			if ranked.service != nil {
				claim.WorkItemID = ""
				claim.ProviderCandidates = append(claim.ProviderCandidates, providerClaim(r, rankedIssue, "").ProviderCandidates[0])
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, other.base+"/claims", r.redemption.Credential, claim)
			requireNativeStatus(t, response, test.status)
			if test.status == http.StatusOK {
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				if lease.WorkItemID != foreign.WorkItemID {
					t.Fatalf("claimed %s", lease.WorkItemID)
				}
			}
			if ranked.service != nil {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, ranked.base+"/claims", r.redemption.Credential, providerClaim(r, rankedIssue, "ranked")), http.StatusOK)
			}
			if test.ownReady && test.budget != "own monthly" && landing.service == nil {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, own, "own")), http.StatusOK)
			}
			allocation, err := readProjectSpriteAllocation(t.Context(), f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
			if err != nil || allocation.DesiredCount != 1 || allocation.AllocatedCount != 1 {
				t.Fatalf("allocation changed: %+v %v", allocation, err)
			}
			if test.status == http.StatusOK && !test.managed && landing.service == nil {
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE leases SET released_at=? WHERE machine_id=?`, now, r.binding.MachineID); err != nil {
					t.Fatal(err)
				}
				placement, err := readOrganizationPlacementSnapshot(t.Context(), f.service.database.reader, nativeScope{organization: f.project.OrganizationID}, f.service.config.now())
				if err != nil || placement.SpriteFree != 1 || placement.Provisionable != 0 {
					t.Fatalf("idle sharing provisioned extra capacity: %+v %v", placement, err)
				}
				if test.budget == "own monthly" {
					setMonthlyBudgetFixture(t, f, false, budget.MonthlyPolicy{})
				}
				own = f.create(t, "new own ready work")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, own, "return-own")), http.StatusOK)
			}
		})
	}
}
