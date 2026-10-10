package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSpritePoolConversion(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	for _, test := range []struct {
		name                      string
		count, floor, ceiling     int
		already, captured, edited bool
	}{
		{name: "connected pool", count: 1, floor: 1, ceiling: 3},
		{name: "sum beyond former project limit", count: 2, floor: 80, ceiling: 90},
		{name: "existing organization pool", count: 1, floor: 1, ceiling: 3, already: true},
		{name: "applied capped cutover with preserved totals", count: 2, floor: 80, ceiling: 90, already: true, captured: true},
		{name: "applied capped cutover without original totals", count: 2, floor: 80, ceiling: 90, already: true},
		{name: "admin changed captured bounds", count: 2, floor: 80, ceiling: 90, already: true, captured: true, edited: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{SecretKeys: secretTestKeys(t, "1", "1")})
			second := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
			fixtures := []nativeFixture{f, second}
			var runners []runnerFixture
			for _, fixture := range fixtures[:test.count] {
				r := prepareRunner(t, fixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
				r.enroll(t)
				runners = append(runners, r)
				storeSpriteWakeSecret(t, fixture, spritesSecretSentinel)
			}
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			item := f.create(t, "Pool conversion active work")
			r := runners[0]
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: item.WorkItemID, MachineID: r.binding.MachineID, SessionID: "pool-conversion", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			db := f.service.database.db
			legacy, err := migrationFiles.ReadFile("migrations/00071_migration.sql")
			if err != nil {
				t.Fatal(err)
			}
			up := strings.Split(string(legacy), "-- +goose Down")[0]
			if _, err := db.ExecContext(t.Context(), up+`ALTER TABLE project_sprite_pools ADD COLUMN isolation_tier TEXT NOT NULL DEFAULT 'native-trusted'; ALTER TABLE project_sprite_pools ADD COLUMN placement_json TEXT NOT NULL DEFAULT '{"mode":"blended"}';`); err != nil {
				t.Fatal(err)
			}
			for i, r := range runners {
				fixture := fixtures[i]
				if _, err := db.ExecContext(t.Context(), `INSERT INTO project_sprite_pools(organization_id,project_id,min_runners,max_runners,configured_by) SELECT ?,?,?,?,id FROM api_tokens WHERE token_hash=?`, f.project.OrganizationID, fixture.project.ID, test.floor, test.ceiling, apikey.HashToken(testHubAdminToken)); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), `INSERT INTO project_sprite_members(organization_id,project_id,name,provider_organization,enrollment_id,state,idle_since,created_at) VALUES(?,?,?,?,?,'enrolled',?,?)`, f.project.OrganizationID, fixture.project.ID, "sprite-"+string(fixture.project.ID), "detent-test", r.enrollment.ID, testTimestamp, testTimestamp); err != nil {
					t.Fatal(err)
				}
			}
			versions := "20261009222900,20261009222930,20261010161000"
			if !test.already {
				versions += ",20261009223000"
			}
			if test.captured {
				preserve, err := migrationFiles.ReadFile("migrations/20261009222930_preserve_sprite_pool_bounds.sql")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), string(preserve)); err != nil {
					t.Fatal(err)
				}
				versions = "20261010161000"
			}
			if test.already {
				cutover, err := migrationFiles.ReadFile("migrations/20261009223000_organization_sprite_pool_cutover.sql")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), string(cutover)); err != nil {
					t.Fatal(err)
				}
				if test.edited {
					if _, err := db.ExecContext(t.Context(), "UPDATE organization_sprite_pools SET min_runners=7,max_runners=9,revision=revision+1"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := db.ExecContext(t.Context(), "DELETE FROM "+hubSchemaTable+" WHERE version_id IN ("+versions+")"); err != nil {
				t.Fatal(err)
			}
			if _, err := runMigrations(t.Context(), db, discardLogger()); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID}
			view, err := f.service.readSpritePool(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			wantFloor, wantCeiling := test.count*test.floor, test.count*test.ceiling
			if test.already && !test.captured {
				wantFloor, wantCeiling = min(wantFloor, 100), min(wantCeiling, 100)
			}
			if test.edited {
				wantFloor, wantCeiling = 7, 9
			}
			if view.MinRunners != wantFloor || view.MaxRunners != wantCeiling || len(view.Members) != test.count {
				t.Fatalf("pool totals/members changed: %+v", view)
			}
			if test.already && !test.captured && test.count > 1 {
				if _, err := db.ExecContext(t.Context(), "UPDATE organization_sprite_pools SET min_runners=160,max_runners=180,revision=revision+1 WHERE organization_id=?", f.project.OrganizationID); err != nil {
					t.Fatalf("cannot restore known original bounds after upgrade: %v", err)
				}
			}
			for i, r := range runners {
				found := false
				for _, member := range view.Members {
					if member.RunnerID == r.binding.RunnerID && member.Name == "sprite-"+string(fixtures[i].project.ID) && member.EnrollmentID == r.enrollment.ID {
						found = true
					}
				}
				if !found {
					t.Fatal("migration recreated or lost a connected Sprite")
				}
				identity, err := readRunnerWithClock(t.Context(), db, f.project.OrganizationID, r.binding.RunnerID, f.service.config.now)
				if err != nil {
					t.Fatal(err)
				}
				if identity.Binding != r.binding || identity.Scope != "organization" || identity.ConnectionHealth != "online" {
					t.Fatalf("member identity changed: %+v", identity)
				}
				var source string
				if err := db.QueryRowContext(t.Context(), `SELECT token_project_id FROM organization_sprite_members WHERE enrollment_id=?`, r.enrollment.ID).Scan(&source); err != nil {
					t.Fatal(err)
				}
				if source != string(fixtures[i].project.ID) {
					t.Fatal("member lost project token override")
				}
			}
			var activeLease string
			if err := db.QueryRowContext(t.Context(), "SELECT lease_id FROM leases WHERE lease_id=? AND released_at IS NULL", lease.ID).Scan(&activeLease); err != nil {
				t.Fatal(err)
			}
			if activeLease != string(lease.ID) {
				t.Fatal("pool migration interrupted active work")
			}
			var secrets, legacyTables int
			if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM organization_secrets WHERE organization_id=?`, f.project.OrganizationID).Scan(&secrets); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_master WHERE name IN ('project_sprite_pools','project_sprite_members')`).Scan(&legacyTables); err != nil {
				t.Fatal(err)
			}
			if secrets != 0 || legacyTables != 0 {
				t.Fatal("migration promoted a token without confirmation or retained project pools")
			}
		})
	}
}

func TestPromoteSpritesToken(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	for _, test := range []struct {
		name                               string
		changed, existing, ranked, revoked bool
	}{
		{name: "confirmed token"}, {name: "changed source", changed: true}, {name: "existing organization token", existing: true},
		{name: "first token follows project rank", ranked: true},
		{name: "authority revoked after preview", revoked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{SecretKeys: secretTestKeys(t, "1", "1")})
			storeSpriteWakeSecret(t, f, spritesSecretSentinel)
			promotedToken := spritesSecretSentinel
			sourceProject := f.project.ID
			if test.ranked {
				second := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
				storeSpriteWakeSecret(t, second, "ranked-token")
				promotedToken, sourceProject = "ranked-token", second.project.ID
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET scheduling_rank=CASE WHEN id=? THEN 0 ELSE 2 END WHERE organization_id=?", sourceProject, f.project.OrganizationID); err != nil {
					t.Fatal(err)
				}
			}
			scope := nativeScope{organization: f.project.OrganizationID, credential: apiCredential{Scope: apiScopeAdmin, Hash: apikey.HashToken(testHubAdminToken)}}
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE token_hash=?", scope.credential.Hash).Scan(&scope.credential.ID); err != nil {
				t.Fatal(err)
			}
			change, _, _, err := firstProjectSpritesToken(t.Context(), f.service.database.db, scope)
			if err != nil {
				t.Fatal(err)
			}
			if change.ProjectID != sourceProject {
				t.Fatalf("selected token project=%s want=%s", change.ProjectID, sourceProject)
			}
			raw, err := json.Marshal(change)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), spritesSecretSentinel) {
				t.Fatal("preview exposed token")
			}
			if test.changed {
				storeSpriteWakeSecret(t, f, "replacement")
			}
			if test.existing {
				storeOrganizationSpritesSecret(t, f, "existing")
			}
			if test.revoked {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(f.service.config.now()), scope.credential.ID); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.WithValue(t.Context(), nativeOperatorScopeKey{}, func(context.Context) (nativeScope, error) { return scope, nil })
			result, err := f.service.executePromoteSpritesToken(ctx, chat.Action{Kind: "promote_sprites_token", ProjectID: string(f.project.ID), Arguments: raw})
			if test.changed || test.existing || test.revoked {
				if err == nil {
					t.Fatal("stale or overwriting promotion accepted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(result.Message, spritesSecretSentinel) {
					t.Fatal("result exposed token")
				}
				resolved, _, envelope, err := resolveSpritesSecret(t.Context(), f.service.database.db, scope)
				if err != nil {
					t.Fatal(err)
				}
				token, err := f.service.config.SecretKeys.Open(envelope, secretAAD(string(resolved.organization), "", flySpritesToken))
				if err != nil {
					t.Fatal(err)
				}
				if string(token) != promotedToken {
					t.Fatal("promoted token changed")
				}
				clear(token)
			}
			projectScope := scope
			projectScope.project = f.project.ID
			_, _, envelope, err := resolveSpritesSecret(t.Context(), f.service.database.db, projectScope)
			if err != nil {
				t.Fatal(err)
			}
			token, err := f.service.config.SecretKeys.Open(envelope, secretAAD(string(scope.organization), string(f.project.ID), flySpritesToken))
			if err != nil {
				t.Fatal(err)
			}
			expected := spritesSecretSentinel
			if test.changed {
				expected = "replacement"
			}
			if string(token) != expected {
				t.Fatal("project override was changed")
			}
			clear(token)
		})
	}
}
