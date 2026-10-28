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
	_, err = f.service.database.db.ExecContext(t.Context(), `INSERT INTO organization_sprite_pools(organization_id,max_runners,bootstrap,configured_by,placement_json) SELECT ?,?,'true',id,? FROM api_tokens WHERE token_hash=? ON CONFLICT(organization_id) DO UPDATE SET placement_json=excluded.placement_json,max_runners=excluded.max_runners`, f.project.OrganizationID, ceiling, string(raw), apikey.HashToken(testHubAdminToken))
	if err != nil {
		t.Fatal(err)
	}
	return nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
}

func placementFixtureRunner(t *testing.T, f nativeFixture, name, account string, sprite bool, capacity int, models ...string) runnerFixture {
	t.Helper()
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
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
		want, desired               int
	}{
		{"blended warm Sprite", "blended", "free", "sprite", 1, http.StatusOK, -1},
		{"blended local", "blended", "free", "local", 1, http.StatusOK, -1},
		{"unsigned local remains eligible", "blended", "signing_warning", "local", 1, http.StatusOK, -1},
		{"unsigned local retains local-first preference", "local_first", "signing_warning", "sprite", 4, http.StatusConflict, -1},
		{"invalid local settings do not reserve preferred capacity", "local_first", "invalid_settings", "sprite", 4, http.StatusOK, -1},
		{"signing warning does not hide invalid settings", "local_first", "signing_and_settings", "sprite", 4, http.StatusOK, -1},
		{"Sprites-only excludes local", "sprites_only", "free", "local", 3, http.StatusConflict, -1},
		{"Sprites-only ignores free local", "sprites_only", "free", "sprite", 1, http.StatusOK, -1},
		{"local-first warm Sprite yields to local", "local_first", "free", "sprite", 4, http.StatusConflict, -1},
		{"local-first local remains eligible", "local_first", "free", "local", 4, http.StatusOK, -1},
		{"below threshold", "local_first", "full", "sprite", 2, http.StatusConflict, -1},
		{"equal threshold", "local_first", "full", "sprite", 3, http.StatusOK, -1},
		{"above threshold", "local_first", "full", "sprite", 4, http.StatusOK, -1},
		{"incompatible local", "local_first", "incompatible", "sprite", 3, http.StatusOK, -1},
		{"offline local", "local_first", "offline", "sprite", 3, http.StatusOK, -1},
		{"draining local", "local_first", "draining", "sprite", 3, http.StatusOK, -1},
		{"no local", "local_first", "absent", "sprite", 3, http.StatusOK, -1},
		{"no local below threshold", "local_first", "absent", "sprite", 2, http.StatusConflict, -1},
		{"non-Todo work uses Todo overflow eligibility", "local_first", "full", "sprite", 4, http.StatusOK, -1},
		{"managed blended manual Sprite desired 2", "blended", "free", "sprite", 3, http.StatusOK, 2},
		{"managed blended manual Sprite desired 0", "blended", "free", "sprite", 3, http.StatusOK, 0},
		{"managed manual Sprite preserves project grant", "blended", "grant", "sprite", 3, http.StatusNotFound, 2},
		{"managed manual Sprite preserves disabled state", "blended", "disabled", "sprite", 3, http.StatusConflict, 2},
		{"managed manual Sprite preserves unavailable execution", "blended", "execution", "sprite", 3, http.StatusConflict, 2},
		{"managed manual Sprite preserves dependency hold", "blended", "dependency", "sprite", 3, http.StatusConflict, 2},
		{"managed manual Sprite retains local-first preference", "local_first", "free", "sprite", 3, http.StatusConflict, 2},
		{"managed sprites_only admits manual Sprite", "sprites_only", "free", "sprite", 3, http.StatusOK, 2},
		{"managed blended local desired 2", "blended", "free", "local", 3, http.StatusOK, 2},
		{"managed blended local desired 0", "blended", "free", "local", 3, http.StatusOK, 0},
		{"managed local_first local desired 2", "local_first", "free", "local", 3, http.StatusOK, 2},
		{"managed local_first local desired 0", "local_first", "free", "local", 3, http.StatusOK, 0},
		{"managed sprites_only local desired 2", "sprites_only", "free", "local", 3, http.StatusConflict, 2},
		{"managed sprites_only local desired 0", "sprites_only", "free", "local", 3, http.StatusConflict, 0},
		{"managed local preserves runner capacity", "blended", "runner_full", "local", 3, http.StatusConflict, 2},
		{"managed local preserves host capacity", "blended", "host_full", "local", 3, http.StatusConflict, 2},
		{"managed local preserves provider capacity", "blended", "provider_full", "local", 3, http.StatusConflict, 2},
		{"managed local preserves policy approval", "blended", "policy", "local", 3, http.StatusConflict, 2},
		{"managed local preserves project grant", "blended", "grant", "local", 3, http.StatusNotFound, 2},
		{"managed local preserves source refusal", "blended", "source", "local", 3, http.StatusConflict, 2},
		{"managed local preserves dependency hold", "blended", "dependency", "local", 3, http.StatusConflict, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			sprite := placementFixtureRunner(t, f, "sprite-runner", "sprite", true, 4, "gpt-6.1-sol", "test-model")
			var live []tracker.NativeLease
			var local runnerFixture
			if test.local != "absent" {
				models := []string{"gpt-6.1-sol", "test-model"}
				if test.local == "incompatible" {
					models = []string{"other-model"}
				}
				capacity := 1
				if test.desired >= 0 {
					capacity = 2
				}
				local = placementFixtureRunner(t, f, "local-runner", "local", false, capacity, models...)
				if test.desired >= 0 {
					other := placementFixtureRunner(t, f, "other-local", "other", false, 2, models...)
					placementFixtureRunner(t, f, "idle-local", "idle", false, 1, models...)
					for i, r := range []runnerFixture{local, other} {
						busy := f.create(t, fmt.Sprintf("running %d", i))
						response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, busy, fmt.Sprintf("busy-%d", i)))
						requireNativeStatus(t, response, http.StatusOK)
						var lease tracker.NativeLease
						decodeHubResponse(t, response, &lease)
						live = append(live, lease)
					}
				}
				switch test.local {
				case "signing_warning", "invalid_settings", "signing_and_settings":
					problem := runnerauth.NewProblem("settings_invalid")
					problem.Check = "commit_signing"
					problems := []runnerauth.Problem{problem}
					switch test.local {
					case "invalid_settings":
						problems[0].Check = "routing"
					case "signing_and_settings":
						problems = append(problems, runnerauth.NewProblem("settings_invalid"))
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(local.binding.MachineID)+"/heartbeat", local.redemption.Credential,
						map[string]any{"backend_isolation": local.redemption.BackendIsolation, "display_name": "Runner", "capacity": 1, "version": "test", "problems": problems}), http.StatusOK)
					view, err := readRunner(t.Context(), f.service.database.db, f.project.OrganizationID, local.binding.RunnerID, f.service.config.now())
					if err != nil || len(view.Problems) != len(problems) || view.Health != "needs_attention" {
						t.Fatalf("reported problems lost: %+v, %v", view, err)
					}
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
			scope := setPlacementFixture(t, f, placement, 4)
			if test.desired >= 0 {
				scope = setPlacementFixture(t, f, placement, 0)
				setSpriteAllocationFixture(t, f, test.desired, 0)
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE project_sprite_allocations SET provision_failures=3,revision=2 WHERE project_id=?`, f.project.ID); err != nil {
					t.Fatal(err)
				}
				if test.claimant == "sprite" {
					for i := range 2 {
						historical := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim)
						now := formatHubTime(f.service.config.now())
						if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO organization_sprite_members(organization_id,allocation_project_id,name,provider_organization,enrollment_id,state,bootstrap_log,idle_since,created_at) VALUES(?,?,?,'test',?,'deleted','bootstrap failed',?,?)`, f.project.OrganizationID, f.project.ID, fmt.Sprintf("historical-%d", i), historical.enrollment.ID, now, now); err != nil {
							t.Fatal(err)
						}
					}
				}
				allocation, err := readProjectSpriteAllocation(t.Context(), f.service.database.reader, scope)
				if err != nil || allocation.AllocatedCount != 0 || allocation.ProvisionFailures != 3 || allocation.Shortfall != test.desired {
					t.Fatalf("allocation=%+v err=%v", allocation, err)
				}
			}
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
			claim := providerClaim(r, issue, "placement")
			switch test.local {
			case "disabled":
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE runner_identities SET state='disabled' WHERE id=?`, r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			case "execution":
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE runner_identities SET backend_isolation_json='{}' WHERE id=?`, r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			case "runner_full":
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE runner_identities SET capacity_limit=1 WHERE id=?`, r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			case "host_full":
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE machines SET capacity=1 WHERE id=?`, r.binding.MachineID); err != nil {
					t.Fatal(err)
				}
			case "provider_full":
				report := capacityReport(f.service.config.now())
				report.Availability = "exhausted"
				publishCapacity(t, f, r, report)
			case "policy":
				if _, err := f.service.database.db.ExecContext(t.Context(), `DELETE FROM project_policies WHERE scope=?`, string(f.project.OrganizationID)+"/"+string(f.project.ID)); err != nil {
					t.Fatal(err)
				}
			case "grant":
				if _, err := f.service.database.db.ExecContext(t.Context(), `DELETE FROM token_grants WHERE token_id=(SELECT token_id FROM runner_identities WHERE id=?)`, r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			case "dependency":
				blocker := f.create(t, "unfinished dependency")
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO issue_dependencies(dependent_issue_id,blocker_issue_id,provenance,created_at,updated_at) SELECT d.id,b.id,'native',d.created_at,d.updated_at FROM issues d,issues b WHERE d.native_id=? AND b.native_id=?`, issue.WorkItemID, blocker.WorkItemID); err != nil {
					t.Fatal(err)
				}
			case "source":
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, issue, "source"))
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				event := nativeStartedEvent(lease)
				path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, event), http.StatusOK)
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "missing-source", 2
				event.Data.Handoff = nativeTestCheckpoint()
				event.Data.Handoff.Availability = "missing"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, event), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
			requireNativeStatus(t, response, test.want)
			if test.desired >= 0 {
				if test.claimant == "sprite" {
					var adopted int
					if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM organization_sprite_members WHERE organization_id=? AND enrollment_id=?`, f.project.OrganizationID, sprite.enrollment.ID).Scan(&adopted); err != nil || adopted != 0 {
						t.Fatalf("manual runner adopted=%d err=%v", adopted, err)
					}
					allocation, err := readProjectSpriteAllocation(t.Context(), f.service.database.reader, scope)
					if err != nil || allocation.DesiredCount != test.desired || allocation.AllocatedCount != 0 || allocation.ProvisionFailures != 3 {
						t.Fatalf("allocation changed: %+v err=%v", allocation, err)
					}
				}
				runners, err := readPlacementRunners(t.Context(), f.service.database.reader, scope, f.service.config.now())
				if err != nil {
					t.Fatal(err)
				}
				locals := 0
				for _, runner := range runners {
					if !runner.Sprite {
						locals++
					}
				}
				wantLocals := 3
				if test.local == "grant" && test.claimant == "local" {
					wantLocals = 2
				}
				if test.mode == "sprites_only" {
					wantLocals = 0
				}
				if locals != wantLocals {
					t.Fatalf("placement lost local runners: got %d, want %d", locals, wantLocals)
				}
				for _, lease := range live {
					var active bool
					if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT released_at IS NULL AND fencing_token=? FROM leases WHERE lease_id=?`, lease.FencingToken, lease.ID).Scan(&active); err != nil || !active {
						t.Fatalf("existing lease changed: %s active=%t err=%v", lease.ID, active, err)
					}
				}
			}
		})
	}
}

func TestPlacementDemandAndCapacity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, excluded, model  string
		locals, providerMax    int
		shared, pending, local bool
		organization           bool
		wantDemand, wantLocal  int
		wantPending            int
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
		{name: "runner local accounts add capacity with identical aliases", locals: 2, providerMax: 1, local: true, wantDemand: 3, wantLocal: 2},
		{name: "host and runner limit", locals: 1, providerMax: 8, wantDemand: 3, wantLocal: 2},
		{name: "pending creation is not double counted", pending: true, wantDemand: 3, wantPending: 1},
		{name: "organization local capacity is counted once", organization: true, locals: 1, providerMax: 8, wantDemand: 6, wantLocal: 2},
		{name: "organization pending member is counted once", organization: true, pending: true, wantDemand: 6, wantPending: 1},
	} {
		for _, typed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/typed=%t", test.name, typed), func(t *testing.T) {
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
					if test.local {
						report.SharedAccountAlias = ""
					}
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
					if _, err := db.ExecContext(t.Context(), `INSERT INTO organization_sprite_members(organization_id,name,provider_organization,enrollment_id,state,idle_since,created_at) VALUES(?,?,'test',?,'bootstrapping',?,?)`, f.project.OrganizationID, sprite.redemption.Hostname, sprite.enrollment.ID, formatHubTime(f.service.config.now()), formatHubTime(f.service.config.now())); err != nil {
						t.Fatal(err)
					}
					if _, err := db.ExecContext(t.Context(), `UPDATE runner_identities SET last_heartbeat_at=? WHERE id=?`, formatHubTime(f.service.config.now().Add(-time.Minute)), sprite.binding.RunnerID); err != nil {
						t.Fatal(err)
					}
					if _, err := db.ExecContext(t.Context(), `UPDATE machines SET machine_facts_ready=NULL,capabilities_json=json_set(capabilities_json,'$.sprite_woken_at',?) WHERE id=?`, formatHubTime(f.service.config.now()), sprite.binding.MachineID); err != nil {
						t.Fatal(err)
					}
				}
				scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
				if test.organization {
					later := newNativeFixture(t, f.service, f.project.OrganizationID, "later")
					approveHubTestPolicy(t, f.service, later.base+"/policy", hubTestPolicy())
					for i := range 3 {
						later.create(t, fmt.Sprintf("later %d", i))
					}
					if _, err := db.ExecContext(t.Context(), "UPDATE runner_identities SET scope='organization' WHERE organization_id=?", scope.organization); err != nil {
						t.Fatal(err)
					}
				}
				setSpriteFactsFixtureMode(t, f, typed)
				usage, err := readPlacementSpriteUsage(t.Context(), f.service.database.reader, scope, f.service.config.now())
				wantUsage := 0
				if test.excluded == "leased" {
					wantUsage = 1
				}
				if err != nil || usage != wantUsage {
					t.Fatalf("Sprite usage=%d err=%v, want %d", usage, err, wantUsage)
				}

				decision, err := readPlacementSnapshot(t.Context(), db, scope, f.service.config.now(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if test.organization {
					decision, err = readOrganizationPlacementSnapshot(t.Context(), db, scope, f.service.config.now())
					if err != nil {
						t.Fatal(err)
					}
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
}

func TestPlacementConcurrentClaims(t *testing.T) {
	for _, test := range []struct {
		name, mode                     string
		ceiling, overflow, limit, want int
		organization                   bool
		allocation                     string
	}{
		{"overflow bounds concurrent warm claims", "local_first", 4, 2, 0, 2, false, ""},
		{"smaller Sprite maximum bounds overflow", "local_first", 4, 2, 1, 1, false, ""},
		{"explicit Sprite maximum bounds concurrent warm claims", "blended", 1, 0, 1, 1, false, ""},
		{"legacy blended retains deployed capacity", "blended", 1, 0, 0, 6, false, ""},
		{"organization Sprite maximum spans projects", "blended", 1, 0, 1, 1, true, ""},
		{"managed zero permits granted manual organization runners", "blended", 4, 0, 0, 6, true, "zero"},
		{"managed project permits granted manual Sprites", "blended", 4, 0, 0, 6, false, "manual"},
		{"desired members do not cap job slots", "blended", 4, 0, 0, 6, false, "owned"},
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
			if test.allocation != "" {
				desired := 1
				if test.allocation == "zero" {
					desired = 0
				}
				setSpriteAllocationFixture(t, f, desired, 0)
				if test.allocation == "owned" {
					now := formatHubTime(f.service.config.now())
					if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO organization_sprite_members(organization_id,allocation_project_id,name,provider_organization,enrollment_id,state,idle_since,created_at) VALUES(?,?,?,'test',?,'enrolled',?,?)`, f.project.OrganizationID, f.project.ID, sprite.redemption.Hostname, sprite.enrollment.ID, now, now); err != nil {
						t.Fatal(err)
					}
					setSpriteMemberChecks(t, f, spriteMemberReady)
				}
			}
			var items []tracker.NativeIssue
			for i := range 6 {
				items = append(items, f.create(t, fmt.Sprintf("queued %d", i)))
			}
			if test.organization && test.allocation == "" {
				later := newNativeFixture(t, f.service, f.project.OrganizationID, "later")
				approveHubTestPolicy(t, f.service, later.base+"/policy", hubTestPolicy())
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET scope='organization' WHERE id=?", sprite.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				for i := range 3 {
					items[i] = later.create(t, fmt.Sprintf("later %d", i))
				}
			}
			if test.allocation == "zero" {
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE runner_identities SET scope='organization' WHERE id=?`, sprite.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			start := make(chan struct{})
			statuses := make(chan int, len(items))
			var workers sync.WaitGroup
			for i, issue := range items {
				workers.Go(func() {
					<-start
					response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(issue.OrganizationID)+"/projects/"+string(issue.ProjectID)+"/claims", sprite.redemption.Credential, providerClaim(sprite, issue, fmt.Sprintf("concurrent-%d", i)))
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
	if _, err := db.ExecContext(t.Context(), `INSERT INTO organization_sprite_pools(organization_id,configured_by) VALUES(?,?)`, scope.organization, principal); err != nil {
		t.Fatal(err)
	}
	view, err := f.service.readSpritePool(t.Context(), scope)
	if err != nil || view.Placement == nil || view.Placement.Mode != "blended" {
		t.Fatalf("migrated settings=%+v err=%v", view, err)
	}
	settings := view.spritePoolSettings
	settings.Placement = &policy.Placement{Mode: "local_first", TodoThreshold: 12, OverflowSlots: 2}
	if err := f.service.updateSpritePool(t.Context(), scope, settings, nil); err != nil {
		t.Fatal(err)
	}
	view, err = f.service.readSpritePool(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	legacy := view.spritePoolSettings
	legacy.Placement = nil
	legacy.IdleSeconds = 600
	if err := f.service.updateSpritePool(t.Context(), scope, legacy, nil); err != nil {
		t.Fatal(err)
	}
	updated, err := f.service.readSpritePool(t.Context(), scope)
	if err != nil || updated.Placement == nil || *updated.Placement != *settings.Placement || updated.IdleSeconds != 600 {
		t.Fatalf("legacy PUT changed placement: %+v err=%v", updated, err)
	}
	if err := f.service.updateSpritePool(t.Context(), scope, legacy, nil); err == nil {
		t.Fatal("stale settings revision accepted")
	}
}
