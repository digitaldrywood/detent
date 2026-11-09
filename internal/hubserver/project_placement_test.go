package hubserver

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func setPlacementFixture(t *testing.T, f nativeFixture, placement policy.Placement, ceiling int) nativeScope {
	t.Helper()
	placement = placement.Resolved()
	err := f.service.database.postgres.WriteSprite(t.Context(), f.project.OrganizationID, corestore.Serving, func(w postgres.SpriteWriter) error {
		row, err := w.SpritePool(t.Context())
		if errors.Is(err, corestore.ErrNotFound) {
			row = postgres.SpritePool{OrganizationID: f.project.OrganizationID, IdleSeconds: 300, IsolationTier: "native-trusted", ConfiguredBy: "test_hub_admin"}
		} else if err != nil {
			return err
		}
		row.MaxRunners = int32(ceiling)
		row.PlacementMode, row.TodoThreshold, row.OverflowSlots, row.MaxSpriteSlots = placement.Mode, int32(placement.TodoThreshold), int32(placement.OverflowSlots), int32(placement.MaxSpriteSlots)
		_, err = w.PutSpritePool(t.Context(), row, row.Revision)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("fixture authority=%d %v", status, err)
	}
	return nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
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
	configuration := runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: capacity, ClientLimit: capacity, RuntimeLimit: capacity, Manageable: true, ObservedAt: f.service.config.now()}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"backend_isolation": r.redemption.BackendIsolation, "display_name": r.redemption.DisplayName, "capacity": capacity, "version": r.redemption.Version, "sprite_name": r.redemption.SpriteName, "capacity_configuration": configuration, "provider_reports": []providercapacity.Report{report}})
	requireNativeStatus(t, response, http.StatusOK)
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
		{"managed local ignores quota exhaustion", "blended", "provider_full", "local", 3, http.StatusOK, 2},
		{"managed local preserves policy approval", "blended", "policy", "local", 3, http.StatusConflict, 2},
		{"managed local preserves project grant", "blended", "grant", "local", 3, http.StatusNotFound, 2},
		{"managed local preserves source refusal", "blended", "source", "local", 3, http.StatusConflict, 2},
		{"managed local preserves dependency hold", "blended", "dependency", "local", 3, http.StatusConflict, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newSpriteNativeFixture(t, Config{RunDetails: &verifiedAttemptDetails{}})
			scope := setPlacementFixture(t, f, policy.Placement{Mode: "blended"}, 4)
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
					view, err := f.service.readRunnerRouting(t.Context(), scope, local.binding.RunnerID)
					if err != nil || len(view.Problems) != len(problems) || view.Health != "needs_attention" {
						t.Fatalf("reported problems lost: %+v, %v", view, err)
					}
				case "full":
					busy := f.create(t, "already running")
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", local.redemption.Credential, providerClaim(local, busy, "busy")), http.StatusOK)
				case "offline":
					writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
						identity, err := w.RunnerIdentity(t.Context(), local.binding.RunnerID)
						if err != nil {
							return err
						}
						identity.LastHeartbeatAt = f.service.config.now().Add(-3 * time.Minute)
						return w.UpdateRunnerIdentity(t.Context(), identity)
					})
				case "draining":
					writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
						identity, err := w.RunnerIdentity(t.Context(), local.binding.RunnerID)
						if err != nil {
							return err
						}
						identity.State = "draining"
						return w.UpdateRunnerRouting(t.Context(), identity.Revision, identity)
					})
				}
			}
			placement := policy.Placement{Mode: test.mode}
			if test.mode == "local_first" {
				placement.TodoThreshold, placement.OverflowSlots = 3, 2
			}
			scope = setPlacementFixture(t, f, placement, 4)
			if test.desired >= 0 {
				scope = setPlacementFixture(t, f, placement, 0)
				setSpriteAllocationFixture(t, f, test.desired, 0)
				writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
					owner := w.(postgres.SpriteWriter)
					allocation, err := owner.SpriteAllocation(t.Context(), f.project.ID)
					if err != nil {
						return err
					}
					for range 3 {
						if err := owner.RecordSpriteProvisionFailure(t.Context(), f.project.ID, allocation.Revision); err != nil {
							return err
						}
					}
					return nil
				})
				if test.claimant == "sprite" {
					for i := range 2 {
						historical := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim)
						now := f.service.config.now()
						writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
							return w.CreateRunnerSpriteMembership(t.Context(), runnerstore.SpriteMembership{OrganizationID: f.project.OrganizationID, AllocationProjectID: f.project.ID, Name: fmt.Sprintf("historical-%d", i), ProviderOrganization: "detent-test", EnrollmentID: historical.enrollment.ID, State: "deleted", IdleSince: now, CreatedAt: now})
						})
					}
				}
				allocation, err := f.service.projectSpriteAllocation(t.Context(), scope)
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
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", testHubAdminToken, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "active-work"}, ExpectedRevision: issue.Revision, State: "In Progress", Reason: "user_requested"})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &issue)
			}
			if test.claimant == "local" {
				r = local
			}
			claim := providerClaim(r, issue, "placement")
			switch test.local {
			case "disabled":
				writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), r.binding.RunnerID)
					if err != nil {
						return err
					}
					identity.State = "disabled"
					return w.UpdateRunnerRouting(t.Context(), identity.Revision, identity)
				})
			case "execution":
				writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
					return w.ReplaceRunnerBackendIsolation(t.Context(), r.binding.RunnerID, nil)
				})
			case "runner_full":
				writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), r.binding.RunnerID)
					if err != nil {
						return err
					}
					identity.CapacityLimit = 1
					return w.UpdateRunnerIdentity(t.Context(), identity)
				})
			case "host_full":
				view, err := f.service.readRunnerRouting(t.Context(), scope, r.binding.RunnerID)
				if err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/machines/"+string(r.binding.MachineID)+"/routing", testHubAdminToken, runnerauth.HostChange{ExpectedRevision: view.HostRevision, DisplayName: view.HostDisplayName, Capacity: 1}), http.StatusOK)
			case "provider_full":
				report := capacityReport(f.service.config.now())
				report.Availability = "exhausted"
				publishCapacity(t, f, r, report)
			case "policy":
				connection := spritePostgresFixtureConnection(t, f)
				if _, err := connection.Exec(t.Context(), "DELETE FROM project_policies WHERE organization_id=$1 AND project_id=$2", scope.organization, scope.project); err != nil {
					t.Fatal(err)
				}
			case "grant":
				writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), r.binding.RunnerID)
					if err != nil {
						return err
					}
					return w.DeleteAccessTokenGrant(t.Context(), identity.TokenID, scope.project)
				})
			case "dependency":
				blocker := f.create(t, "unfinished dependency")
				writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
					project, err := w.ProjectRecord(t.Context(), scope.project)
					if err != nil {
						return err
					}
					project.Core.RequireDependencies = true
					_, err = w.UpdateProjectRecord(t.Context(), project)
					return err
				})
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/dependencies", testHubAdminToken, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "claim-blocker"}, ExpectedRevision: issue.Revision, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &issue)
				claim = providerClaim(r, issue, "placement")
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
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
						if _, err := reader.RunnerSpriteMembershipByEnrollment(t.Context(), sprite.enrollment.ID); err == nil {
							return errors.New("manual runner adopted")
						} else if !errors.Is(err, corestore.ErrNotFound) {
							return err
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					allocation, err := f.service.projectSpriteAllocation(t.Context(), scope)
					if err != nil || allocation.DesiredCount != test.desired || allocation.AllocatedCount != 0 || allocation.ProvisionFailures != 3 {
						t.Fatalf("allocation changed: %+v err=%v", allocation, err)
					}
				}
				snapshot, err := f.service.spriteProjectPlacementRecord(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				locals := 0
				for _, runner := range snapshot.Runners {
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
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
					for _, lease := range live {
						current, err := reader.Lease(t.Context(), string(lease.ID))
						if err != nil {
							return err
						}
						if current.ReleasedAt != nil || current.FencingToken != int64(lease.FencingToken) {
							return errors.New("existing lease changed")
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
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
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newSpriteNativeFixture(t, Config{RunDetails: &verifiedAttemptDetails{}})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			scope := setPlacementFixture(t, f, policy.Placement{Mode: "local_first", TodoThreshold: 3, OverflowSlots: 2}, 5)
			sprite := placementFixtureRunner(t, f, "sprite-runner", "sprite", true, 1, "gpt-6.1-sol", "gpt-6-astra", "test-model")
			runners := []runnerFixture{sprite}
			for i := range test.locals {
				account := fmt.Sprintf("local%d", i)
				if test.shared {
					account = "local"
				}
				r := placementFixtureRunner(t, f, account, account, false, 2, "gpt-6.1-sol", "test-model")
				runners = append(runners, r)
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
			switch test.excluded {
			case "backlog", "in_progress":
				state := "In Progress"
				if test.excluded == "backlog" {
					state = "Backlog"
					writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
						project, err := w.ProjectRecord(t.Context(), scope.project)
						if err != nil {
							return err
						}
						for i := range project.States {
							if project.States[i].Name == "Todo" {
								project.States[i].Transitions = append(project.States[i].Transitions, state)
							}
						}
						project.States = append(project.States, tracker.NativeState{Name: state, Dispatchable: true})
						_, err = w.UpdateProjectRecord(t.Context(), project)
						return err
					})
				}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(third.WorkItemID)+"/workflow", testHubAdminToken, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "exclude-state"}, ExpectedRevision: third.Revision, State: state, Reason: "user_requested"})
				requireNativeStatus(t, response, http.StatusOK)
			case "dependency":
				writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
					project, err := w.ProjectRecord(t.Context(), scope.project)
					if err != nil {
						return err
					}
					project.Core.RequireDependencies = true
					_, err = w.UpdateProjectRecord(t.Context(), project)
					return err
				})
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(third.WorkItemID)+"/dependencies", testHubAdminToken, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "add-blocker"}, ExpectedRevision: third.Revision, RelatedWorkItemID: first.WorkItemID, Operation: "add"})
				requireNativeStatus(t, response, http.StatusOK)
			case "leased":
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", sprite.redemption.Credential, providerClaim(sprite, third, "leased")), http.StatusOK)
			}
			if test.model != "" {
				body := third.Body + "\n\n```detent-agent\nschema: 1\nmodel: " + test.model + "\n```"
				response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(third.WorkItemID), testHubAdminToken, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "select-model"}, ExpectedRevision: third.Revision, Body: &body})
				requireNativeStatus(t, response, http.StatusOK)
			}
			if test.pending {
				now := f.service.config.now()
				writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
					if err := w.CreateRunnerSpriteMembership(t.Context(), runnerstore.SpriteMembership{OrganizationID: scope.organization, Name: sprite.redemption.Hostname, ProviderOrganization: "detent-test", EnrollmentID: sprite.enrollment.ID, State: "bootstrapping", IdleSince: now, CreatedAt: now}); err != nil {
						return err
					}
					identity, err := w.RunnerIdentity(t.Context(), sprite.binding.RunnerID)
					if err != nil {
						return err
					}
					identity.LastHeartbeatAt = now.Add(-time.Minute)
					return w.UpdateRunnerIdentity(t.Context(), identity)
				})
				storeSpriteWakeReceiptFixture(t, f, scope, sprite.binding.RunnerID, sprite.redemption.Hostname, "committed", now)
			}
			if test.organization {
				later := newNativeFixture(t, f.service, f.project.OrganizationID, "later")
				approveHubTestPolicy(t, f.service, later.base+"/policy", hubTestPolicy())
				for i := range 3 {
					later.create(t, fmt.Sprintf("later %d", i))
				}
				writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
					for _, runner := range runners {
						identity, err := w.RunnerIdentity(t.Context(), runner.binding.RunnerID)
						if err != nil {
							return err
						}
						identity.Scope = "organization"
						if err := w.UpdateRunnerRouting(t.Context(), identity.Revision, identity); err != nil {
							return err
						}
					}
					return nil
				})
			}
			var decision placementSnapshot
			var err error
			if test.organization {
				decision, err = f.service.spritePlacementRecord(t.Context(), scope)
			} else {
				decision, err = f.service.spriteProjectPlacementRecord(t.Context(), scope)
			}
			if err != nil {
				t.Fatal(err)
			}
			wantUsage := 0
			if test.excluded == "leased" {
				wantUsage = 1
			}
			if usage := decision.SpriteUsed; usage != wantUsage {
				t.Fatalf("Sprite usage=%d, want %d", usage, wantUsage)
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
			f := newSpriteNativeFixture(t, Config{})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			placement := policy.Placement{Mode: test.mode, OverflowSlots: test.overflow, MaxSpriteSlots: test.limit}
			if test.mode == "local_first" {
				placement.TodoThreshold = 1
			}
			scope := setPlacementFixture(t, f, placement, test.ceiling)
			sprite := placementFixtureRunner(t, f, "sprite-runner", "sprite", true, 8, "gpt-6.1-sol", "test-model")
			if test.allocation != "" {
				desired := 1
				if test.allocation == "zero" {
					desired = 0
				}
				allocation := setSpriteAllocationFixture(t, f, desired, 0)
				if test.allocation == "owned" {
					now := f.service.config.now()
					writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
						return w.CreateRunnerSpriteMembership(t.Context(), runnerstore.SpriteMembership{OrganizationID: f.project.OrganizationID, AllocationProjectID: f.project.ID, AllocationRevision: int64(allocation.Revision), Name: sprite.redemption.Hostname, ProviderOrganization: "detent-test", EnrollmentID: sprite.enrollment.ID, State: "enrolled", IdleSince: now, CreatedAt: now})
					})
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
				writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), sprite.binding.RunnerID)
					if err != nil {
						return err
					}
					identity.Scope = "organization"
					if err := w.UpdateRunnerRouting(t.Context(), identity.Revision, identity); err != nil {
						return err
					}
					return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: scope.organization, TokenID: identity.TokenID, ProjectID: later.project.ID})
				})
				for i := range 3 {
					items[i] = later.create(t, fmt.Sprintf("later %d", i))
				}
			}
			if test.allocation == "zero" {
				writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), sprite.binding.RunnerID)
					if err != nil {
						return err
					}
					identity.Scope = "organization"
					return w.UpdateRunnerRouting(t.Context(), identity.Revision, identity)
				})
			}
			start := make(chan struct{})
			statuses := make(chan int, len(items))
			var first tracker.NativeLease
			var workers sync.WaitGroup
			for i, issue := range items {
				workers.Go(func() {
					<-start
					response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(issue.OrganizationID)+"/projects/"+string(issue.ProjectID)+"/claims", sprite.redemption.Credential, providerClaim(sprite, issue, fmt.Sprintf("concurrent-%d", i)))
					if i == 0 && response.Code == http.StatusOK {
						decodeHubResponse(t, response, &first)
					}
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
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				for _, issue := range items {
					item, err := r.WorkItem(t.Context(), issue.ProjectID, issue.WorkItemID)
					if err != nil {
						return err
					}
					lease, err := r.ActiveLeaseForIssue(t.Context(), item.ID)
					if err != nil && !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
					if err == nil && lease.ReleasedAt == nil {
						running++
					}
				}
				rows, err := r.WorkItems(t.Context(), scope.project, 0, 200)
				if err != nil {
					return err
				}
				for _, item := range rows {
					if item.WorkflowStateName != nil && *item.WorkflowStateName != "Todo" {
						promoted++
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if running != claimed || promoted != 0 {
				t.Fatalf("running=%d claimed=%d promoted=%d", running, claimed, promoted)
			}
			if test.allocation == "owned" {
				if first.ID == "" || first.FencingToken <= 0 {
					t.Fatal("owned claim did not return a fenced lease")
				}
				setSpriteAllocationFixture(t, f, 0, 1)
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", sprite.redemption.Credential, providerClaim(sprite, items[0], "concurrent-0"))
				requireNativeStatus(t, response, http.StatusOK)
				var replay tracker.NativeLease
				decodeHubResponse(t, response, &replay)
				if replay.ID != first.ID || replay.FencingToken != first.FencingToken {
					t.Fatal("withdrawn allocation changed an existing claim replay")
				}
			}
			if placement.SpriteLimit() > 0 {
				decision, err := f.service.spritePlacementRecord(t.Context(), scope)
				if err != nil || decision.Reason != "Configured maximum Sprite concurrency is full" || decision.SpriteTarget != 0 {
					t.Fatalf("concurrency diagnostic=%+v err=%v", decision.placementDecision, err)
				}
			}
		})
	}
}

func TestPlacementIgnoresExhaustedSubscriptionQuota(t *testing.T) {
	for _, claimant := range []string{"local", "sprite"} {
		t.Run(claimant, func(t *testing.T) {
			f := newSpriteNativeFixture(t, Config{})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			sprite := placementFixtureRunner(t, f, "sprite-runner", "sprite", true, 2, "gpt-6.1-sol", "test-model")
			local := placementFixtureRunner(t, f, "local-runner", "local", false, 1, "gpt-6.1-sol", "test-model")
			report := capacityReport(f.service.config.now())
			report.Models, report.SharedAccountAlias, report.Availability = []string{"gpt-6.1-sol", "test-model"}, "sprite", "exhausted"
			publishCapacity(t, f, sprite, report)
			scope := setPlacementFixture(t, f, policy.Placement{Mode: "blended"}, 3)
			issue := f.create(t, "first")
			f.create(t, "second")
			decision, err := f.service.spritePlacementRecord(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Provisionable != 0 || decision.LocalFree != 1 || decision.SpriteFree != 1 {
				t.Fatalf("exhausted Sprite account decision=%+v", decision.placementDecision)
			}
			r, status := local, http.StatusOK
			if claimant == "sprite" {
				r, status = sprite, http.StatusOK
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
		{"exhausted account wakes within declared capacity", "sprites_only", 3, false, true, 1},
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
			writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
				identity, err := w.RunnerIdentity(t.Context(), sprite.binding.RunnerID)
				if err != nil {
					return err
				}
				identity.LastHeartbeatAt = f.service.config.now().Add(-time.Minute)
				return w.UpdateRunnerIdentity(t.Context(), identity)
			})
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
	f := newSpriteNativeFixture(t, Config{})
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("fixture authority=%d %v", status, err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
	writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
		_, err := w.(postgres.SpriteWriter).PutSpritePool(t.Context(), postgres.SpritePool{OrganizationID: scope.organization, ConfiguredBy: credential.ID, IsolationTier: "native-trusted", PlacementMode: "blended", IdleSeconds: 300}, 0)
		return err
	})
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
