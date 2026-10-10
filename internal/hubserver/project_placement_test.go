package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func setPlacementFixture(t *testing.T, f nativeFixture, placement policy.Placement, ceiling int) nativeScope {
	t.Helper()
	raw, err := json.Marshal(placement)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.database.db.ExecContext(t.Context(), `INSERT INTO project_sprite_pools(organization_id,project_id,max_runners,bootstrap,configured_by,placement_json) SELECT ?,?,?,'true',id,? FROM api_tokens WHERE token_hash=? ON CONFLICT(organization_id,project_id) DO UPDATE SET placement_json=excluded.placement_json,max_runners=excluded.max_runners`, f.project.OrganizationID, f.project.ID, ceiling, string(raw), apikey.HashToken(testHubAdminToken))
	if err != nil {
		t.Fatal(err)
	}
	return nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
}

func placementFixtureRunner(t *testing.T, f nativeFixture, name, account string, sprite bool, capacity int, models ...string) runnerFixture {
	t.Helper()
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.redemption.Hostname, r.redemption.Capacity = name, capacity
	if sprite {
		r.redemption.SpriteName = name
	}
	r.enroll(t)
	report := capacityReport(f.service.config.now())
	report.Models, report.MaxConcurrent, report.SharedAccountAlias = models, capacity, account
	publishCapacity(t, f, r, report)
	return r
}

func TestPlacementClaims(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, mode, local, claimant string
		depth                       int
		want                        int
	}{
		{"blended warm Sprite", "blended", "free", "sprite", 1, http.StatusOK},
		{"blended local", "blended", "free", "local", 1, http.StatusOK},
		{"Sprites-only excludes local", "sprites_only", "free", "local", 3, http.StatusConflict},
		{"Sprites-only ignores free local", "sprites_only", "free", "sprite", 1, http.StatusOK},
		{"local-first warm Sprite yields to local", "local_first", "free", "sprite", 4, http.StatusConflict},
		{"local-first local remains eligible", "local_first", "free", "local", 4, http.StatusOK},
		{"below threshold", "local_first", "full", "sprite", 2, http.StatusConflict},
		{"equal threshold", "local_first", "full", "sprite", 3, http.StatusOK},
		{"above threshold", "local_first", "full", "sprite", 4, http.StatusOK},
		{"incompatible local", "local_first", "incompatible", "sprite", 3, http.StatusOK},
		{"offline local", "local_first", "offline", "sprite", 3, http.StatusOK},
		{"draining local", "local_first", "draining", "sprite", 3, http.StatusOK},
		{"no local", "local_first", "absent", "sprite", 3, http.StatusOK},
		{"no local below threshold", "local_first", "absent", "sprite", 2, http.StatusConflict},
		{"non-Todo work uses Todo overflow eligibility", "local_first", "full", "sprite", 4, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			sprite := placementFixtureRunner(t, f, "sprite-runner", "sprite", true, 4, "gpt-6.1-sol", "test-model")
			var local runnerFixture
			if test.local != "absent" {
				models := []string{"gpt-6.1-sol", "test-model"}
				if test.local == "incompatible" {
					models = []string{"other-model"}
				}
				local = placementFixtureRunner(t, f, "local-runner", "local", false, 1, models...)
				switch test.local {
				case "full":
					busy := f.create(t, "already running")
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", local.redemption.Credential, providerClaim(local, busy, "busy")), http.StatusOK)
				case "offline":
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE runner_identities SET last_heartbeat_at=? WHERE id=?`, formatHubTime(f.service.config.now().Add(-3*time.Minute)), local.binding.RunnerID); err != nil {
						t.Fatal(err)
					}
				case "draining":
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE runner_identities SET state='draining' WHERE id=?`, local.binding.RunnerID); err != nil {
						t.Fatal(err)
					}
				}
			}
			placement := policy.Placement{Mode: test.mode}
			if test.mode == "local_first" {
				placement.TodoThreshold, placement.OverflowSlots = 3, 2
			}
			setPlacementFixture(t, f, placement, 4)
			var issue tracker.NativeIssue
			for i := range test.depth {
				created := f.create(t, fmt.Sprintf("ready %d", i))
				if i == 0 {
					issue = created
				}
			}
			r := sprite
			if test.name == "non-Todo work uses Todo overflow eligibility" {
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='In Progress') WHERE native_id=?`, f.project.ID, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			if test.claimant == "local" {
				r = local
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, issue, "placement"))
			requireNativeStatus(t, response, test.want)
		})
	}
}

func TestPlacementDemandAndCapacity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, excluded, model string
		locals, providerMax   int
		shared, pending       bool
		wantDemand, wantLocal int
		wantPending           int
	}{
		{name: "ready Todo", wantDemand: 3},
		{name: "Backlog excluded even if dispatchable", excluded: "backlog", wantDemand: 2},
		{name: "other dispatchable states excluded", excluded: "in_progress", wantDemand: 2},
		{name: "dependency excluded", excluded: "dependency", wantDemand: 2},
		{name: "leased excluded", excluded: "leased", wantDemand: 2},
		{name: "incompatible model excluded", model: "other-model", wantDemand: 2},
		{name: "heterogeneous demand overflows only incompatible local work", model: "gpt-6-astra", locals: 1, providerMax: 8, wantDemand: 3, wantLocal: 2},
		{name: "shared account capacity once", locals: 2, providerMax: 1, shared: true, wantDemand: 3, wantLocal: 1},
		{name: "independent accounts add capacity", locals: 2, providerMax: 1, wantDemand: 3, wantLocal: 2},
		{name: "host and runner limit", locals: 1, providerMax: 8, wantDemand: 3, wantLocal: 2},
		{name: "pending creation is not double counted", pending: true, wantDemand: 3, wantPending: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			setPlacementFixture(t, f, policy.Placement{Mode: "local_first", TodoThreshold: 3, OverflowSlots: 2}, 5)
			sprite := placementFixtureRunner(t, f, "sprite-runner", "sprite", true, 1, "gpt-6.1-sol", "gpt-6-astra", "test-model")
			for i := range test.locals {
				account := fmt.Sprintf("local%d", i)
				if test.shared {
					account = "local"
				}
				r := placementFixtureRunner(t, f, account, account, false, 2, "gpt-6.1-sol", "test-model")
				report := capacityReport(f.service.config.now())
				report.Models, report.SharedAccountAlias, report.MaxConcurrent = []string{"gpt-6.1-sol", "test-model"}, account, test.providerMax
				publishCapacity(t, f, r, report)
			}
			first := f.create(t, "first")
			f.create(t, "second")
			third := f.create(t, "third")
			db := f.service.database.db
			switch test.excluded {
			case "in_progress":
				if _, err := db.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='In Progress') WHERE native_id=?`, f.project.ID, third.WorkItemID); err != nil {
					t.Fatal(err)
				}
			case "backlog":
				if _, err := db.ExecContext(t.Context(), `INSERT INTO workflow_states(project_id,source_name,detent_state,dispatchable,terminal,created_at,updated_at) SELECT project_id,'Backlog','Backlog',1,0,created_at,updated_at FROM workflow_states WHERE project_id=? AND detent_state='Todo'`, f.project.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Backlog') WHERE native_id=?`, f.project.ID, third.WorkItemID); err != nil {
					t.Fatal(err)
				}
			case "dependency":
				if _, err := db.ExecContext(t.Context(), `INSERT INTO issue_dependencies(dependent_issue_id,blocker_issue_id,provenance,created_at,updated_at) SELECT d.id,b.id,'native',d.created_at,d.updated_at FROM issues d,issues b WHERE d.native_id=? AND b.native_id=?`, third.WorkItemID, first.WorkItemID); err != nil {
					t.Fatal(err)
				}
			case "leased":
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", sprite.redemption.Credential, providerClaim(sprite, third, "leased")), http.StatusOK)
			}
			if test.model != "" {
				if _, err := db.ExecContext(t.Context(), `UPDATE issues SET body=? WHERE native_id=?`, "```detent-agent\nschema: 1\nmodel: "+test.model+"\n```", third.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			if test.pending {
				if _, err := db.ExecContext(t.Context(), `INSERT INTO project_sprite_members(organization_id,project_id,name,provider_organization,enrollment_id,state,idle_since,created_at) VALUES(?,?,?,'test',?,'bootstrapping',?,?)`, f.project.OrganizationID, f.project.ID, sprite.redemption.Hostname, sprite.enrollment.ID, formatHubTime(f.service.config.now()), formatHubTime(f.service.config.now())); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), `UPDATE runner_identities SET last_heartbeat_at=? WHERE id=?`, formatHubTime(f.service.config.now().Add(-time.Minute)), sprite.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), `UPDATE machines SET capabilities_json=json_set(capabilities_json,'$.sprite_woken_at',?) WHERE id=?`, formatHubTime(f.service.config.now()), sprite.binding.MachineID); err != nil {
					t.Fatal(err)
				}
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			decision, err := readPlacementSnapshot(t.Context(), db, scope, f.service.config.now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if decision.ReadyTodo != test.wantDemand || decision.LocalFree != test.wantLocal || decision.Pending != test.wantPending {
				t.Fatalf("decision=%+v; want demand=%d local=%d pending=%d", decision.placementDecision, test.wantDemand, test.wantLocal, test.wantPending)
			}
			if test.model == "gpt-6-astra" && (decision.SpriteTarget != 1 || decision.SpriteFree != 1) {
				t.Fatalf("heterogeneous demand decision=%+v; want one Sprite slot", decision.placementDecision)
			}
		})
	}
}

func TestPlacementConcurrentClaims(t *testing.T) {
	for _, test := range []struct {
		name, mode                     string
		ceiling, overflow, limit, want int
	}{
		{"overflow bounds concurrent warm claims", "local_first", 4, 2, 0, 2},
		{"smaller Sprite maximum bounds overflow", "local_first", 4, 2, 1, 1},
		{"explicit Sprite maximum bounds concurrent warm claims", "blended", 1, 0, 1, 1},
		{"legacy blended retains deployed capacity", "blended", 1, 0, 0, 6},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			placement := policy.Placement{Mode: test.mode, OverflowSlots: test.overflow, MaxSpriteSlots: test.limit}
			if test.mode == "local_first" {
				placement.TodoThreshold = 1
			}
			setPlacementFixture(t, f, placement, test.ceiling)
			sprite := placementFixtureRunner(t, f, "sprite-runner", "sprite", true, 8, "gpt-6.1-sol", "test-model")
			var items []tracker.NativeIssue
			for i := range 6 {
				items = append(items, f.create(t, fmt.Sprintf("queued %d", i)))
			}
			start := make(chan struct{})
			statuses := make(chan int, len(items))
			var workers sync.WaitGroup
			for i, issue := range items {
				workers.Go(func() {
					<-start
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", sprite.redemption.Credential, providerClaim(sprite, issue, fmt.Sprintf("concurrent-%d", i)))
					statuses <- response.Code
				})
			}
			close(start)
			workers.Wait()
			close(statuses)
			claimed := 0
			for status := range statuses {
				if status == http.StatusOK {
					claimed++
				} else if status != http.StatusConflict {
					t.Fatalf("claim status=%d", status)
				}
			}
			if claimed != test.want {
				t.Fatalf("claimed=%d want=%d", claimed, test.want)
			}
			var running, promoted int
			if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT count(*) FROM leases WHERE released_at IS NULL`).Scan(&running); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT count(*) FROM issues i JOIN workflow_states w ON w.id=i.workflow_state_id WHERE i.project_id=? AND w.detent_state<>'Todo'`, f.project.ID).Scan(&promoted); err != nil {
				t.Fatal(err)
			}
			if running != claimed || promoted != 0 {
				t.Fatalf("running=%d claimed=%d promoted=%d", running, claimed, promoted)
			}
			if placement.SpriteLimit() > 0 {
				decision, err := readPlacementSnapshot(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, f.service.config.now(), nil)
				if err != nil || decision.Reason != "Configured maximum Sprite concurrency is full" || decision.SpriteTarget != 0 {
					t.Fatalf("concurrency diagnostic=%+v err=%v", decision.placementDecision, err)
				}
			}
		})
	}
}

func TestPlacementExhaustedSpriteBudget(t *testing.T) {
	for _, claimant := range []string{"local", "sprite"} {
		t.Run(claimant, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			sprite := placementFixtureRunner(t, f, "sprite-runner", "sprite", true, 2, "gpt-6.1-sol", "test-model")
			local := placementFixtureRunner(t, f, "local-runner", "local", false, 1, "gpt-6.1-sol", "test-model")
			report := capacityReport(f.service.config.now())
			report.Models, report.SharedAccountAlias, report.Availability = []string{"gpt-6.1-sol", "test-model"}, "sprite", "exhausted"
			publishCapacity(t, f, sprite, report)
			scope := setPlacementFixture(t, f, policy.Placement{Mode: "local_first", TodoThreshold: 1, OverflowSlots: 2}, 3)
			issue := f.create(t, "first")
			f.create(t, "second")
			decision, err := readPlacementSnapshot(t.Context(), f.service.database.db, scope, f.service.config.now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Provisionable != 0 || decision.LocalFree != 1 {
				t.Fatalf("exhausted Sprite account decision=%+v", decision.placementDecision)
			}
			r, status := local, http.StatusOK
			if claimant == "sprite" {
				r, status = sprite, http.StatusConflict
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, issue, "budget")), status)
		})
	}
}

func TestPlacementWake(t *testing.T) {
	for _, test := range []struct {
		name, mode           string
		depth                int
		localFree, exhausted bool
		want                 int
	}{
		{"local-first below threshold", "local_first", 2, false, false, 0},
		{"local-first at threshold", "local_first", 3, false, false, 1},
		{"local-first above threshold", "local_first", 4, false, false, 1},
		{"local-first yields to free local", "local_first", 3, true, false, 0},
		{"Sprites-only ignores free local", "sprites_only", 1, true, false, 1},
		{"blended unmet demand", "blended", 2, true, false, 1},
		{"exhausted account does not wake", "sprites_only", 3, false, true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			client := &http.Client{Transport: spritesTestTransport(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return spritesTestResponse(`{"type":"complete"}`, http.StatusOK), nil
			})}
			f, scope, _ := newSpriteWakeFixture(t, client, true, 0)
			keys := f.service.config.SecretKeys
			f.service.config.SecretKeys = nil
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			sprite := placementFixtureRunner(t, f, "sprite-runner", "sprite", true, 2, "gpt-6.1-sol")
			if test.exhausted {
				report := capacityReport(f.service.config.now())
				report.Models, report.SharedAccountAlias, report.Availability = []string{"gpt-6.1-sol"}, "sprite", "exhausted"
				publishCapacity(t, f, sprite, report)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE runner_identities SET last_heartbeat_at=? WHERE id=?`, formatHubTime(f.service.config.now().Add(-time.Minute)), sprite.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			if test.localFree {
				placementFixtureRunner(t, f, "local-runner", "local", false, 1, "gpt-6.1-sol")
			}
			for i := range test.depth {
				f.create(t, fmt.Sprintf("ready %d", i))
			}
			placement := policy.Placement{Mode: test.mode}
			if test.mode == "local_first" {
				placement.TodoThreshold, placement.OverflowSlots = 3, 1
			}
			setPlacementFixture(t, f, placement, 2)
			f.service.config.SecretKeys = keys
			woken, err := f.service.wakeSpriteRunners(t.Context(), scope, "Todo")
			if err != nil {
				t.Fatal(err)
			}
			if woken != test.want || int(calls.Load()) != test.want {
				t.Fatalf("woken=%d calls=%d want=%d", woken, calls.Load(), test.want)
			}
			if test.want > 0 {
				again, err := f.service.wakeSpriteRunners(t.Context(), scope, "Todo")
				if err != nil || again != 0 || int(calls.Load()) != test.want {
					t.Fatalf("pending wake repeated: again=%d calls=%d err=%v", again, calls.Load(), err)
				}
			}
		})
	}
}

func TestPlacementSettingsCompatibility(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	db := f.service.database.db
	var principal string
	if err := db.QueryRowContext(t.Context(), `SELECT id FROM api_tokens WHERE token_hash=?`, apikey.HashToken(testHubAdminToken)).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: principal, Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO project_sprite_pools(organization_id,project_id,configured_by) VALUES(?,?,?)`, scope.organization, scope.project, principal); err != nil {
		t.Fatal(err)
	}
	view, err := f.service.readSpritePool(t.Context(), scope)
	if err != nil || view.Placement == nil || view.Placement.Mode != "blended" {
		t.Fatalf("migrated settings=%+v err=%v", view, err)
	}
	settings := view.spritePoolSettings
	settings.Placement = &policy.Placement{Mode: "local_first", TodoThreshold: 12, OverflowSlots: 2}
	if err := f.service.updateSpritePool(t.Context(), scope, settings); err != nil {
		t.Fatal(err)
	}
	view, err = f.service.readSpritePool(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	legacy := view.spritePoolSettings
	legacy.Placement = nil
	legacy.IdleSeconds = 600
	if err := f.service.updateSpritePool(t.Context(), scope, legacy); err != nil {
		t.Fatal(err)
	}
	updated, err := f.service.readSpritePool(t.Context(), scope)
	if err != nil || updated.Placement == nil || *updated.Placement != *settings.Placement || updated.IdleSeconds != 600 {
		t.Fatalf("legacy PUT changed placement: %+v err=%v", updated, err)
	}
	if err := f.service.updateSpritePool(t.Context(), scope, legacy); err == nil {
		t.Fatal("stale settings revision accepted")
	}
}
