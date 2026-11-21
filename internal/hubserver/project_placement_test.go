package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func placementNativeFixture(t *testing.T, cfg Config) nativeFixture {
	t.Helper()
	if cfg.Clock == nil {
		now := time.Now().UTC().Truncate(time.Microsecond)
		cfg.Clock = func() time.Time { return now }
	}
	f := newDefaultNativeFixture(t, cfg)
	placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.Core.RequireDependencies = true
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	})
	f.project.RequireDependencies = true
	return f
}

func placementFixtureScope(t *testing.T, f nativeFixture) hubscope.NativeScope {
	t.Helper()
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("placement administration status%d: %v", status, err)
	}
	return hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
}

func placementFixtureWrite(t *testing.T, f nativeFixture, write func(tenantstore.Writer) error) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, write); err != nil {
		t.Fatal(err)
	}
}

func setPlacementFixture(t *testing.T, f nativeFixture, placement policy.Placement, ceiling int) hubscope.NativeScope {
	t.Helper()
	scope := placementFixtureScope(t, f)
	found := false
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.(postgres.SpriteReader).SpritePool(t.Context())
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var bootstrap attachment.ArchiveReference
	if !found {
		ref, err := prepareChangeContent(f.service.contentContext(t.Context()), string(scope.Organization), []byte("true"))
		if err != nil {
			t.Fatal(err)
		}
		bootstrap = ref
	}
	bootstrapKey := bootstrap.Key
	placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
		owner := w.(postgres.SpriteWriter)
		row, err := owner.SpritePool(t.Context())
		created := errors.Is(err, corestore.ErrNotFound)
		if created {
			if bootstrapKey == "" {
				return corestore.ErrRevisionConflict
			}
			row = postgres.SpritePool{OrganizationID: scope.Organization, IdleSeconds: 300, IsolationTier: isolation.NativeTrusted, ConfiguredBy: scope.Credential.ID, BootstrapObjectKey: new(bootstrapKey)}
		} else if err != nil {
			return err
		}
		expected := row.Revision
		row.MaxRunners, row.PlacementMode, row.TodoThreshold, row.OverflowSlots, row.MaxSpriteSlots = int32(ceiling), placement.Mode, int32(placement.TodoThreshold), int32(placement.OverflowSlots), int32(placement.MaxSpriteSlots)
		if created {
			if err := w.LockArchiveAccounting(t.Context()); err != nil {
				return err
			}
		}
		if _, err := owner.PutSpritePool(t.Context(), row, expected); err != nil {
			return err
		}
		if !created {
			return nil
		}
		object, err := w.ArchiveObjectByReference(t.Context(), bootstrap)
		if err != nil {
			return err
		}
		return w.AddArchiveOwner(t.Context(), artifactstore.ArchiveOwner{OrganizationID: scope.Organization, ArchiveID: object.ID, Kind: "sprite-pool", ID: string(scope.Organization), CreatedAt: f.service.config.Clock()})
	})
	return scope
}

func setPlacementAllocationFixture(t *testing.T, f nativeFixture, desired int, revision tracker.Revision) projectSpriteAllocation {
	t.Helper()
	scope := placementFixtureScope(t, f)
	placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
		_, err := w.(postgres.SpriteWriter).PutSpriteAllocation(t.Context(), postgres.SpriteAllocation{OrganizationID: scope.Organization, ProjectID: scope.Project, DesiredCount: int32(desired), RequestedAt: f.service.config.Clock(), ConfiguredBy: scope.Credential.ID}, int64(revision))
		return err
	})
	result, err := f.service.readProjectSpriteAllocation(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func placementFixtureRouting(t *testing.T, f nativeFixture, runner string, update func(*runnerstore.Identity)) {
	t.Helper()
	placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
		row, err := w.RunnerIdentity(t.Context(), runner)
		if err != nil {
			return err
		}
		expected := row.Revision
		update(&row)
		return w.UpdateRunnerRouting(t.Context(), expected, row)
	})
}

func placementFixtureHeartbeat(t *testing.T, f nativeFixture, runner string, at time.Time) {
	t.Helper()
	placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
		row, err := w.RunnerIdentity(t.Context(), runner)
		if err != nil {
			return err
		}
		row.LastHeartbeatAt = at
		return w.UpdateRunnerIdentity(t.Context(), row)
	})
}

func placementFixtureMember(t *testing.T, f nativeFixture, r runnerFixture, name, state string) {
	t.Helper()
	now := f.service.config.Clock()
	bootstrapLog := ""
	if state == "deleted" {
		bootstrapLog = "bootstrap failed"
	}
	placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
		return w.CreateRunnerSpriteMembership(t.Context(), runnerstore.SpriteMembership{OrganizationID: f.project.OrganizationID, AllocationProjectID: f.project.ID, Name: name, ProviderOrganization: "example-org", EnrollmentID: r.enrollment.ID, State: state, BootstrapLog: bootstrapLog, IdleSince: now, CreatedAt: now})
	})
}

func placementFixtureDependency(t *testing.T, f nativeFixture, dependent, blocker tracker.NativeIssue) {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(dependent.WorkItemID)+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("dependency")}, ExpectedRevision: dependent.Revision, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"})
	requireNativeStatus(t, response, http.StatusOK)
}

func placementFixtureState(t *testing.T, f nativeFixture, item tracker.NativeIssue, state string) tracker.NativeIssue {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("workflow")}, ExpectedRevision: item.Revision, State: state, Reason: "user_requested"})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &item)
	return item
}

func placementFixtureRunners(t *testing.T, f nativeFixture, scope hubscope.NativeScope) ([]hubrecords.PlacementRunner, error) {
	t.Helper()
	var rows []hubrecords.PlacementRunner
	err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = readPlacementRunners(t.Context(), r, scope, f.service.config.Clock())
		return err
	})
	return rows, err
}

func placementFixtureRunner(t *testing.T, f nativeFixture, name, account string, sprite bool, capacity int, models ...string) runnerFixture {
	t.Helper()
	return placementFixtureRunnerWithScope(t, f, name, account, sprite, capacity, "projects", models...)
}

func placementFixtureRunnerWithScope(t *testing.T, f nativeFixture, name, account string, sprite bool, capacity int, scope string, models ...string) runnerFixture {
	t.Helper()
	r := prepareRunnerForScope(t, f, scope, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	r.redemption.Hostname, r.redemption.Capacity = name, capacity
	if sprite {
		r.redemption.SpriteName = name
	}
	r.enroll(t)
	report := capacityReport(f.service.config.Clock())
	report.Models, report.MaxConcurrent, report.SharedAccountAlias = models, capacity, account
	publishCapacity(t, f, r, report)
	placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
		return w.UpdateRunnerCapacityConfig(t.Context(), r.binding.RunnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: capacity, ClientLimit: capacity, RuntimeLimit: capacity, Manageable: true, ObservedAt: f.service.config.Clock()})
	})
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
			f := placementNativeFixture(t, Config{})
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
					view, err := f.service.readRunnerRouting(t.Context(), placementFixtureScope(t, f), local.binding.RunnerID)
					if err != nil || len(view.Problems) != len(problems) || view.Health != "needs_attention" {
						t.Fatalf("reported problems lost: %+v, %v", view, err)
					}
				case "full":
					busy := f.create(t, "already running")
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", local.redemption.Credential, providerClaim(local, busy, "busy")), http.StatusOK)
				case "offline":
					placementFixtureHeartbeat(t, f, local.binding.RunnerID, f.service.config.Clock().Add(-3*time.Minute))
				case "draining":
					placementFixtureRouting(t, f, local.binding.RunnerID, func(row *runnerstore.Identity) { row.State = "draining" })
				}
			}
			placement := policy.Placement{Mode: test.mode}
			if test.mode == "local_first" {
				placement.TodoThreshold, placement.OverflowSlots = 3, 2
			}
			scope := setPlacementFixture(t, f, placement, 4)
			if test.desired >= 0 {
				scope = setPlacementFixture(t, f, placement, 0)
				setPlacementAllocationFixture(t, f, test.desired, 0)
				placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
					owner := w.(postgres.SpriteWriter)
					row, err := owner.SpriteAllocation(t.Context(), f.project.ID)
					if err != nil {
						return err
					}
					row, err = owner.PutSpriteAllocation(t.Context(), row, row.Revision)
					if err != nil {
						return err
					}
					for range 3 {
						if err := owner.RecordSpriteProvisionFailure(t.Context(), f.project.ID, row.Revision); err != nil {
							return err
						}
					}
					return nil
				})
				if test.claimant == "sprite" {
					for i := range 2 {
						historical := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim)
						placementFixtureMember(t, f, historical, fmt.Sprintf("historical-%d", i), "deleted")
					}
				}
				allocation, err := f.service.readProjectSpriteAllocation(t.Context(), scope)
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
				issue = placementFixtureState(t, f, issue, "In Progress")
			}
			if test.claimant == "local" {
				r = local
			}
			claim := providerClaim(r, issue, "placement")
			switch test.local {
			case "disabled":
				placementFixtureRouting(t, f, r.binding.RunnerID, func(row *runnerstore.Identity) { row.State = "disabled" })
			case "execution":
				placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
					return w.ReplaceRunnerBackendIsolation(t.Context(), r.binding.RunnerID, isolation.Report{})
				})
			case "runner_full":
				placementFixtureRouting(t, f, r.binding.RunnerID, func(row *runnerstore.Identity) { row.CapacityLimit = 1 })
			case "host_full":
				view, err := f.service.readRunnerRouting(t.Context(), placementFixtureScope(t, f), r.binding.RunnerID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.updateRunnerHostCommand(t.Context(), placementFixtureScope(t, f), string(r.binding.MachineID), runnerauth.HostChange{ExpectedRevision: view.HostRevision, DisplayName: view.Hostname, Capacity: 1}); err != nil {
					t.Fatal(err)
				}
			case "provider_full":
				report := capacityReport(f.service.config.Clock())
				report.Availability = "exhausted"
				publishCapacity(t, f, r, report)
			case "policy":
				result, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "DELETE FROM project_policies WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID)
				if err != nil {
					t.Fatal(err)
				}
				if count, err := result.RowsAffected(); err != nil || count != 1 {
					t.Fatalf("missing-policy fixture rows%d: %v", count, err)
				}
			case "grant":
				placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
					row, err := w.RunnerIdentity(t.Context(), r.binding.RunnerID)
					if err != nil {
						return err
					}
					return w.DeleteAccessTokenGrant(t.Context(), row.TokenID, f.project.ID)
				})
			case "dependency":
				blocker := f.create(t, "unfinished dependency")
				placementFixtureDependency(t, f, issue, blocker)
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
					err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
						_, err := reader.RunnerSpriteMembershipByEnrollment(t.Context(), sprite.enrollment.ID)
						if errors.Is(err, corestore.ErrNotFound) {
							return nil
						}
						if err == nil {
							adopted = 1
						}
						return err
					})
					if err != nil || adopted != 0 {
						t.Fatalf("manual runner adopted=%d err=%v", adopted, err)
					}
					allocation, err := f.service.readProjectSpriteAllocation(t.Context(), scope)
					if err != nil || allocation.DesiredCount != test.desired || allocation.AllocatedCount != 0 || allocation.ProvisionFailures != 3 {
						t.Fatalf("allocation changed: %+v err=%v", allocation, err)
					}
				}
				runners, err := placementFixtureRunners(t, f, scope)
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
					err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
						row, err := reader.Lease(t.Context(), string(lease.ID))
						active = row.ReleasedAt == nil && row.FencingToken == int64(lease.FencingToken)
						return err
					})
					if err != nil || !active {
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
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := placementNativeFixture(t, Config{})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			setPlacementFixture(t, f, policy.Placement{Mode: "local_first", TodoThreshold: 3, OverflowSlots: 2}, 5)
			runnerScope := "projects"
			if test.organization {
				runnerScope = "organization"
			}
			sprite := placementFixtureRunnerWithScope(t, f, "sprite-runner", "sprite", true, 1, runnerScope, "gpt-6.1-sol", "gpt-6-astra", "test-model")
			for i := range test.locals {
				account := fmt.Sprintf("local%d", i)
				if test.shared {
					account = "local"
				}
				r := placementFixtureRunnerWithScope(t, f, account, account, false, 2, runnerScope, "gpt-6.1-sol", "test-model")
				report := capacityReport(f.service.config.Clock())
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
			case "in_progress":
				third = placementFixtureState(t, f, third, "In Progress")
			case "backlog":
				placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
					project, err := w.ProjectRecord(t.Context(), f.project.ID)
					if err != nil {
						return err
					}
					found := false
					for index := range project.States {
						if project.States[index].Name == "Backlog" {
							project.States[index].Dispatchable = true
							found = true
						}
						if project.States[index].Name == "Todo" {
							project.States[index].Transitions = append(project.States[index].Transitions, "Backlog")
						}
					}
					if !found {
						project.States = append(project.States, tracker.NativeState{Name: "Backlog", Dispatchable: true, Transitions: []string{"Todo"}})
					}
					_, err = w.UpdateProjectRecord(t.Context(), project)
					return err
				})
				third = placementFixtureState(t, f, third, "Backlog")
			case "dependency":
				placementFixtureDependency(t, f, third, first)
			case "leased":
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", sprite.redemption.Credential, providerClaim(sprite, third, "leased")), http.StatusOK)
			}
			if test.model != "" {
				body := third.Body + "\n```detent-agent\nschema: 1\nmodel: " + test.model + "\n```"
				response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(third.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("model")}, ExpectedRevision: third.Revision, Body: &body})
				requireNativeStatus(t, response, http.StatusOK)
			}
			if test.pending {
				placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
					now := f.service.config.Clock()
					return w.CreateRunnerSpriteMembership(t.Context(), runnerstore.SpriteMembership{OrganizationID: f.project.OrganizationID, Name: sprite.redemption.Hostname, ProviderOrganization: "example-org", EnrollmentID: sprite.enrollment.ID, State: "bootstrapping", IdleSince: now, CreatedAt: now})
				})
				placementFixtureHeartbeat(t, f, sprite.binding.RunnerID, f.service.config.Clock().Add(-time.Minute))
				placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
					return w.UpdateMachineSpriteWake(t.Context(), leasestore.SpriteWake{MachineID: string(sprite.binding.MachineID), RunnerID: sprite.binding.RunnerID, Name: sprite.redemption.Hostname, WokenAt: new(f.service.config.Clock()), ObservedAt: f.service.config.Clock()})
				})
			}
			scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}
			if test.organization {
				later := newNativeFixture(t, f.service, f.project.OrganizationID, "later")
				approveHubTestPolicy(t, f.service, later.base+"/policy", hubTestPolicy())
				for i := range 3 {
					later.create(t, fmt.Sprintf("later %d", i))
				}

			}
			var usage int
			err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				usage, err = readPlacementSpriteUsage(t.Context(), r, f.service.config.Clock())
				return err
			})
			wantUsage := 0
			if test.excluded == "leased" {
				wantUsage = 1
			}
			if err != nil || usage != wantUsage {
				t.Fatalf("Sprite usage=%d err=%v, want %d", usage, err, wantUsage)
			}

			decision, err := f.service.readPlacementSnapshot(t.Context(), scope, f.service.config.Clock(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.organization {
				decision, err = f.service.readOrganizationPlacementSnapshot(t.Context(), scope, f.service.config.Clock())
				if err != nil {
					t.Fatal(err)
				}
			}
			if decision.ReadyTodo != test.wantDemand || decision.LocalFree != test.wantLocal || decision.Pending != test.wantPending {
				t.Fatalf("decision=%+v; want demand=%d local=%d pending=%d", decision.PlacementDecision, test.wantDemand, test.wantLocal, test.wantPending)
			}
			if test.model == "gpt-6-astra" && (decision.SpriteTarget != 1 || decision.SpriteFree != 1) {
				t.Fatalf("heterogeneous demand decision=%+v; want one Sprite slot", decision.PlacementDecision)
			}
		})
	}
}

func TestPlacementConcurrentClaims(t *testing.T) {
	t.Parallel()
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
			f := placementNativeFixture(t, Config{})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			placement := policy.Placement{Mode: test.mode, OverflowSlots: test.overflow, MaxSpriteSlots: test.limit}
			if test.mode == "local_first" {
				placement.TodoThreshold = 1
			}
			setPlacementFixture(t, f, placement, test.ceiling)
			runnerScope := "projects"
			if test.organization {
				runnerScope = "organization"
			}
			sprite := placementFixtureRunnerWithScope(t, f, "sprite-runner", "sprite", true, 8, runnerScope, "gpt-6.1-sol", "test-model")
			if test.allocation != "" {
				desired := 1
				if test.allocation == "zero" {
					desired = 0
				}
				setPlacementAllocationFixture(t, f, desired, 0)
				if test.allocation == "owned" {
					placementFixtureMember(t, f, sprite, sprite.redemption.Hostname, "enrolled")
					placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
						return w.ReplaceRunnerLocalChecks(t.Context(), sprite.binding.RunnerID, f.project.ID, runnerauth.LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"codex"}, ObservedAt: f.service.config.Clock()})
					})
				}
			}
			var items []tracker.NativeIssue
			for i := range 6 {
				items = append(items, f.create(t, fmt.Sprintf("queued %d", i)))
			}
			if test.organization && test.allocation == "" {
				later := newNativeFixture(t, f.service, f.project.OrganizationID, "later")
				approveHubTestPolicy(t, f.service, later.base+"/policy", hubTestPolicy())

				for i := range 3 {
					items[i] = later.create(t, fmt.Sprintf("later %d", i))
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
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				count, err := r.Usage(t.Context(), string(sprite.binding.MachineID))
				if err != nil {
					return err
				}
				running = int(count)
				page, err := r.ScanWorkItems(t.Context(), f.project.ID, corestore.ScanCursor{}, corestore.MaxPageSize)
				if err != nil {
					return err
				}
				if page.Next.After < page.Next.Through {
					return corestore.ErrInvalidPage
				}
				for _, item := range page.Items {
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
			if placement.SpriteLimit() > 0 {
				decision, err := f.service.readPlacementSnapshot(t.Context(), hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}, f.service.config.Clock(), nil)
				if err != nil || decision.Reason != "Configured maximum Sprite concurrency is full" || decision.SpriteTarget != 0 {
					t.Fatalf("concurrency diagnostic=%+v err=%v", decision.PlacementDecision, err)
				}
			}
		})
	}
}

func TestPlacementClaimTenantBoundaries(t *testing.T) {
	t.Parallel()
	f := placementNativeFixture(t, Config{})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	second := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
	approveHubTestPolicy(t, f.service, second.base+"/policy", hubTestPolicy())
	seedPostgresHubAdmin(t, f.service.config.TenantRepository, f.service, nativeOrganization{ID: "org_placement_other", Name: "Other placement organization", Slug: "other-placement"})
	other := newNativeFixture(t, f.service, "org_placement_other", "other")
	approveHubTestPolicy(t, f.service, other.base+"/policy", hubTestPolicy())
	setPlacementFixture(t, f, policy.Placement{Mode: "sprites_only"}, 2)
	setPlacementAllocationFixture(t, f, 1, 0)
	sprite := placementFixtureRunnerWithScope(t, f, "sprite-runner", "sprite", true, 4, "organization", "gpt-6.1-sol", "test-model")
	placementFixtureMember(t, f, sprite, sprite.redemption.Hostname, "enrolled")
	placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
		return w.ReplaceRunnerLocalChecks(t.Context(), sprite.binding.RunnerID, f.project.ID, runnerauth.LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"codex"}, ObservedAt: f.service.config.Clock()})
	})
	local := placementFixtureRunnerWithScope(t, f, "local-runner", "local", false, 4, "organization", "gpt-6.1-sol", "test-model")
	otherLocal := placementFixtureRunnerWithScope(t, other, "other-local", "other", false, 2, "projects", "gpt-6.1-sol", "test-model")
	for _, runner := range []runnerFixture{sprite, local, otherLocal} {
		if len(runner.redemption.BackendIsolation) == 0 {
			t.Fatalf("runner %s enrolled without a backend isolation report", runner.binding.RunnerID)
		}
	}
	for _, test := range []struct {
		name    string
		project nativeFixture
		runner  runnerFixture
		status  int
	}{
		{"allocated Sprite claims its project", f, sprite, http.StatusOK},
		{"allocated Sprite is refused outside its allocation", second, sprite, http.StatusConflict},
		{"local runner is refused by Sprites-only placement", f, local, http.StatusConflict},
		{"local runner is refused in an unallocated project of the same organization", second, local, http.StatusConflict},
		{"local runner in another organization keeps its default placement", other, otherLocal, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := test.project.create(t, test.name)
			response := performHubAPIRequest(t, f.service, http.MethodPost, test.project.base+"/claims", test.runner.redemption.Credential, providerClaim(test.runner, issue, hubapi.NewID("session")))
			requireNativeStatus(t, response, test.status)
			if test.status == http.StatusOK {
				return
			}
			var refusal hubapi.NativeError
			decodeHubResponse(t, response, &refusal)
			if refusal.Code != "no_claimable_work" {
				t.Fatalf("refusal code=%q, want no_claimable_work", refusal.Code)
			}
		})
	}
}

func TestPlacementIgnoresExhaustedSubscriptionQuota(t *testing.T) {
	t.Parallel()
	for _, claimant := range []string{"local", "sprite"} {
		t.Run(claimant, func(t *testing.T) {
			f := placementNativeFixture(t, Config{})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			sprite := placementFixtureRunner(t, f, "sprite-runner", "sprite", true, 2, "gpt-6.1-sol", "test-model")
			local := placementFixtureRunner(t, f, "local-runner", "local", false, 1, "gpt-6.1-sol", "test-model")
			report := capacityReport(f.service.config.Clock())
			report.Models, report.SharedAccountAlias, report.Availability = []string{"gpt-6.1-sol", "test-model"}, "sprite", "exhausted"
			publishCapacity(t, f, sprite, report)
			scope := setPlacementFixture(t, f, policy.Placement{Mode: "blended"}, 3)
			issue := f.create(t, "first")
			f.create(t, "second")
			decision, err := f.service.readPlacementSnapshot(t.Context(), scope, f.service.config.Clock(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Provisionable != 0 || decision.LocalFree != 1 || decision.SpriteFree != 1 {
				t.Fatalf("exhausted Sprite account decision=%+v", decision.PlacementDecision)
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
	t.Parallel()
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
			f := placementNativeFixture(t, Config{SecretKeys: secretTestKeys(t, "1", "1"), SpritesHTTPClient: client})
			scope := placementFixtureScope(t, f)
			envelope, err := f.service.config.SecretKeys.Seal([]byte(spritesSecretSentinel), accessstore.SecretAAD(scope.Organization, scope.Project, flySpritesToken))
			if err != nil {
				t.Fatal(err)
			}
			placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
				return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: scope.Organization, ProjectID: scope.Project, Kind: flySpritesToken, OrganizationSlug: "example-org", Envelope: envelope, UpdatedAt: f.service.config.Clock()}, accessstore.SecretAudit{OrganizationID: scope.Organization, ProjectID: scope.Project, Actor: scope.Credential.ID, Kind: flySpritesToken, Event: "set", KeyVersion: envelope.Version, RecordedAt: f.service.config.Clock()})
			})
			keys := f.service.config.SecretKeys
			f.service.config.SecretKeys = nil
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			sprite := placementFixtureRunner(t, f, "sprite-runner", "sprite", true, 2, "gpt-6.1-sol")
			if test.exhausted {
				report := capacityReport(f.service.config.Clock())
				report.Models, report.SharedAccountAlias, report.Availability = []string{"gpt-6.1-sol"}, "sprite", "exhausted"
				publishCapacity(t, f, sprite, report)
			}
			placementFixtureHeartbeat(t, f, sprite.binding.RunnerID, f.service.config.Clock().Add(-time.Minute))
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
	t.Parallel()
	f := placementNativeFixture(t, Config{})
	scope := placementFixtureScope(t, f)
	placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
		_, err := w.(postgres.SpriteWriter).PutSpritePool(t.Context(), postgres.SpritePool{OrganizationID: scope.Organization, ConfiguredBy: scope.Credential.ID, IdleSeconds: 300, IsolationTier: isolation.NativeTrusted, PlacementMode: "blended"}, 0)
		return err
	})
	view, err := f.service.readSpritePool(t.Context(), scope)
	if err != nil || view.Placement == nil || view.Placement.Mode != "blended" {
		t.Fatalf("stored settings=%+v err=%v", view, err)
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

func TestClaimSourceAvailabilityFollowsCurrentSource(t *testing.T) {
	t.Parallel()
	f := placementNativeFixture(t, Config{})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	holder := placementFixtureRunner(t, f, "source-runner", "source", false, 2, "gpt-6.1-sol", "test-model")
	other := placementFixtureRunner(t, f, "other-runner", "other", false, 2, "gpt-6.1-sol", "test-model")
	issue := f.create(t, "source pinned to its runner")
	item := f.base + "/work-items/" + string(issue.WorkItemID)
	claim := func(r runnerFixture, session string, want int) tracker.NativeLease {
		t.Helper()
		var current tracker.NativeIssue
		decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, item, f.token, nil), &current)
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, current, session))
		var lease tracker.NativeLease
		if requireNativeStatus(t, response, want); want == http.StatusOK {
			decodeHubResponse(t, response, &lease)
		} else {
			requireNativeCode(t, response, want, "no_claimable_work")
		}
		return lease
	}
	publish := func(key string, input tracker.ChangeVersionInput, bundle []byte, expected, change string) tracker.ChangeVersion {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodPost, item+"/changes/"+change+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: key}, ExpectedVersionID: expected, ChangeVersionInput: input, SourceBundle: bundle})
		requireNativeStatus(t, response, http.StatusOK)
		var version tracker.ChangeVersion
		decodeHubResponse(t, response, &version)
		return version
	}
	release := func(r runnerFixture, lease tracker.NativeLease) {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
	}
	lease := claim(holder, "holder-first", http.StatusOK)
	event := nativeStartedEvent(lease)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, item+"/events", holder.redemption.Credential, event), http.StatusOK)
	event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Handoff = "run.checkpointed", "holder-local-checkpoint", 2, nativeTestCheckpoint()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, item+"/events", holder.redemption.Credential, event), http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodPost, item+"/changes", f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "source-change"}, Title: "Source pinned change"}), &change)
	unsourced := publish("unsourced-version", changeTestInput(), nil, "", change.ID)
	release(holder, lease)
	claim(other, "other-refused", http.StatusConflict)
	claim(other, "other-refused", http.StatusConflict)
	var evidence tracker.NativeRuntimeEvidence
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, item+"/runtime", f.token, nil), &evidence)
	want := fmt.Sprintf("Change %s version %s has no verified Hub source; recapture the exact head on source runner %s", change.ID, unsourced.ID, holder.binding.MachineID)
	if evidence.LatestDecision == nil || evidence.LatestDecision.Data.Decision == nil || *evidence.LatestDecision.Data.Decision != (tracker.NativeSchedulerDecision{RunnerID: other.binding.RunnerID, Source: "native_runner_routing", Outcome: "skipped", Reason: want, At: evidence.LatestDecision.Data.Decision.At, WorkItemRevision: evidence.LatestDecision.Data.Decision.WorkItemRevision}) {
		t.Fatalf("source refusal decision=%#v", evidence.LatestDecision)
	}
	for _, entry := range evidence.Admission {
		if refused := entry.Outcome == "skipped" && entry.Reason == want; refused != (entry.RunnerID == other.binding.RunnerID) {
			t.Fatalf("admission explanation disagrees with the claim source decision: %#v", entry)
		}
	}
	if len(evidence.Admission) != 2 {
		t.Fatalf("admission explanation entries=%#v", evidence.Admission)
	}
	release(holder, claim(holder, "holder-second", http.StatusOK))
	input := changeTestInput()
	publish("sourced-version", input, changeTestSource(t, &input), unsourced.ID, change.ID)
	claim(other, "other-after-source", http.StatusOK)
}

func TestClaimScanSkipsUnreadableSourceCheckpoint(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		bad    int
		damage func(*testing.T, *Service, *memoryRunDetails, leasestore.Attempt)
		status int
	}{
		{"missing across a page boundary", nativeClaimCandidatePageSize, func(_ *testing.T, _ *Service, objects *memoryRunDetails, attempt leasestore.Attempt) {
			delete(objects.data, *attempt.CheckpointObjectKey)
		}, http.StatusConflict},
		{"corrupt", 1, func(_ *testing.T, _ *Service, objects *memoryRunDetails, attempt leasestore.Attempt) {
			objects.data[*attempt.CheckpointObjectKey] = []byte(`{"resume":`)
		}, http.StatusConflict},
		{"run events missing", 1, func(t *testing.T, service *Service, _ *memoryRunDetails, attempt leasestore.Attempt) {
			tx, err := usagePostgresAdmin(t, service).BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(t.Context(), "SET LOCAL session_replication_role=replica"); err != nil {
				t.Fatal(err)
			}
			result, err := tx.ExecContext(t.Context(), `DELETE FROM collaboration_events WHERE organization_id=$1 AND attempt_id=$2`, attempt.OrganizationID, attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if count, err := result.RowsAffected(); err != nil || count == 0 {
				t.Fatalf("deleted run events=%d error=%v", count, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}, http.StatusConflict},
		{"storage outage", 1, func(_ *testing.T, _ *Service, objects *memoryRunDetails, _ leasestore.Attempt) {
			objects.beforeRead = func(context.Context) error { return errors.New("object store unavailable") }
		}, http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			objects := &memoryRunDetails{}
			f := placementNativeFixture(t, Config{RunDetails: objects})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			runner := prepareRunnerForScope(t, f, "projects", runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			runner.enroll(t)
			placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
				return w.UpdateRunnerCapacityConfig(t.Context(), runner.binding.RunnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: f.service.config.Clock()})
			})
			request := func(session string, item tracker.NativeWorkItemID) tracker.NativeClaim {
				return tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: item, MachineID: runner.binding.MachineID, SessionID: session, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
			}
			var items []tracker.NativeWorkItemID
			for index := range test.bad {
				issue := f.create(t, fmt.Sprintf("unreadable checkpoint %03d", index))
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, request(fmt.Sprintf("bad-%03d", index), issue.WorkItemID))
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				event := nativeStartedEvent(lease)
				path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, event), http.StatusOK)
				event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Handoff = "run.checkpointed", "bad-checkpoint", 2, nativeTestCheckpoint()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, event), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", runner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
				items = append(items, issue.WorkItemID)
			}
			healthy := f.create(t, "healthy after unreadable checkpoints").WorkItemID
			var attempts []leasestore.Attempt
			placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
				for index, id := range append(items, healthy) {
					entry, err := w.QueueEntry(t.Context(), f.project.ID, id, string(f.project.ID))
					if err != nil {
						return err
					}
					entry.Priority, entry.Rank = new(int64(0)), fmt.Sprintf("%05d", index)
					if err := w.PutQueueEntry(t.Context(), entry); err != nil {
						return err
					}
					if id == healthy {
						continue
					}
					item, err := w.WorkItem(t.Context(), f.project.ID, id)
					if err != nil {
						return err
					}
					attempt, err := w.(leasestore.CheckpointAttemptReader).SourceCheckpointAttempt(t.Context(), item.ID)
					if err != nil {
						return err
					}
					attempts = append(attempts, attempt)
				}
				return nil
			})
			objects.mu.Lock()
			for _, attempt := range attempts {
				test.damage(t, f.service, objects, attempt)
			}
			objects.mu.Unlock()
			if test.status == http.StatusConflict {
				requireNativeCode(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, request("explicit-damaged", items[0])), http.StatusConflict, "no_claimable_work")
			}
			if test.bad > 1 {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, request("scan-first-page", "")), test.status)
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, request("scan", ""))
			if test.status != http.StatusConflict {
				requireNativeStatus(t, response, test.status)
				return
			}
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			if lease.WorkItemID != healthy {
				t.Fatalf("scan claimed %s, want healthy %s", lease.WorkItemID, healthy)
			}
			var decision tracker.CollaborationEvent
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				rows, err := r.(eventstore.SchedulerDecisionReader).SchedulerDecisions(t.Context(), f.project.ID, items[0])
				if err != nil || rows.Latest == nil {
					return errors.Join(err, corestore.ErrNotFound)
				}
				decision, err = eventstore.NewContent(f.service.runDetailStore()).Event(t.Context(), *rows.Latest)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if decision.Data.Decision == nil || decision.Data.Decision.Source != "native_runner_routing" || decision.Data.Decision.Outcome != "skipped" || !strings.Contains(decision.Data.Decision.Reason, "is unreadable; restore and verify source") {
				t.Fatalf("unreadable checkpoint diagnostic=%#v", decision.Data.Decision)
			}
		})
	}
}
