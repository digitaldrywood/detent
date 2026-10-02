package hubserver

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerHomeClaims(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		mode      string
		minutes   int
		elapsed   time.Duration
		homeState string
		selector  bool
		want      int
		wantDry   bool
	}{
		{"never", "never", 0, 10 * time.Minute, "", false, http.StatusConflict, true},
		{"before threshold", "after", 5, 5*time.Minute - time.Nanosecond, "", false, http.StatusConflict, true},
		{"at threshold", "after", 5, 5 * time.Minute, "", false, http.StatusOK, true},
		{"immediate", "after", 0, 0, "", false, http.StatusOK, true},
		{"home todo", "after", 0, 0, "todo", false, http.StatusConflict, false},
		{"another project order does not hide home todo", "never", 0, 0, "todo", false, http.StatusConflict, false},
		{"leased home does not count", "after", 0, 0, "leased", false, http.StatusOK, true},
		{"blocked home does not count", "after", 0, 0, "blocked", false, http.StatusOK, true},
		{"home rework", "after", 0, 0, "rework", false, http.StatusConflict, false},
		{"planning does not count", "after", 0, 0, "planning", false, http.StatusOK, true},
		{"selector mismatch does not count", "after", 0, 0, "todo", true, http.StatusOK, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
			home := newNativeFixture(t, service, "", "home")
			general := newNativeFixture(t, service, home.project.OrganizationID, "general")
			r := prepareRunner(t, home, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			routing := map[string]any{"expected_revision": 1, "display_name": "Home runner", "state": "active", "capacity_limit": 2,
				"project_ids": []tracker.ProjectID{home.project.ID, general.project.ID}, "home_project_ids": []tracker.ProjectID{home.project.ID},
				"spillover": runnerauth.Spillover{Mode: test.mode, AfterMinutes: test.minutes}}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing), http.StatusOK)
			descriptor := hubTestPolicy()
			approveHubTestPolicy(t, service, general.base+"/policy", descriptor)
			homePolicy := descriptor
			if test.selector {
				homePolicy.Requirements = policy.Requirements{RequiredTags: []string{"gpu"}}
				homePolicy = homePolicy.WithID()
			}
			approveHubTestPolicy(t, service, home.base+"/policy", homePolicy)
			if test.homeState != "" {
				homeIssue := home.create(t, "home work")
				if test.homeState == "leased" {
					claim := tracker.NativeClaim{PolicyID: homePolicy.ID, MachineID: r.binding.MachineID, SessionID: "held-home", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, home.base+"/claims", r.redemption.Credential, claim), http.StatusOK)
				}
				if test.homeState == "blocked" {
					blocker := home.create(t, "home blocker")
					if _, err := service.database.db.ExecContext(t.Context(), "UPDATE workflow_states SET dispatchable = 0 WHERE project_id = ? AND detent_state = 'In Progress'", home.project.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id = (SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = 'In Progress') WHERE native_id = ?", home.project.ID, blocker.WorkItemID); err != nil {
						t.Fatal(err)
					}
					if _, err := service.database.db.ExecContext(t.Context(), "INSERT INTO issue_dependencies (dependent_issue_id, blocker_issue_id, provenance, created_at, updated_at) SELECT d.id, b.id, 'native', '2026-09-05T12:00:00Z', '2026-09-05T12:00:00Z' FROM issues d, issues b WHERE d.native_id = ? AND b.native_id = ?", homeIssue.WorkItemID, blocker.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
				if test.homeState == "planning" || test.homeState == "rework" {
					if _, err := service.database.db.ExecContext(t.Context(), "UPDATE workflow_states SET detent_state = ?, dispatchable = ? WHERE project_id = ? AND lower(detent_state) = 'todo'", test.homeState, test.homeState != "planning", home.project.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			claim := tracker.NativeClaim{PolicyID: descriptor.ID, MachineID: r.binding.MachineID, SessionID: "first", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			if test.name == "another project order does not hide home todo" {
				claim.DispatchPriorityByState = []string{"Merging"}
				claim.WorkflowStates = []string{"Merging"}
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, general.base+"/claims", r.redemption.Credential, claim), http.StatusConflict)
			now = now.Add(test.elapsed)
			if _, err := service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at = ? WHERE id = ?", formatHubTime(now), r.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			general.create(t, "general work")
			claim.SessionID = "second"
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, general.base+"/claims", r.redemption.Credential, claim), test.want)
			var dry sql.NullString
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT home_dry_since FROM runner_identities WHERE id = ?", r.binding.RunnerID).Scan(&dry); err != nil {
				t.Fatal(err)
			}
			if dry.Valid != test.wantDry {
				t.Fatalf("home_dry_since = %v, want valid %v", dry, test.wantDry)
			}
			if dry.Valid && dry.String != "2026-09-05T12:00:00Z" {
				t.Fatalf("idle timer restarted: %q", dry.String)
			}
		})
	}
}

func TestRunnerHomeReturnAndOrdering(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	config := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}
	service := openTestService(t, config)
	a := newNativeFixture(t, service, "", "first-home")
	b := newNativeFixture(t, service, a.project.OrganizationID, "second-home")
	general := newNativeFixture(t, service, a.project.OrganizationID, "general")
	r := prepareRunner(t, a, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	routing := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Home runner", State: "active", CapacityLimit: 2,
		ProjectIDs: []tracker.ProjectID{a.project.ID, b.project.ID, general.project.ID}, HomeProjectIDs: []tracker.ProjectID{a.project.ID, b.project.ID},
		Spillover: runnerauth.Spillover{Mode: "after", AfterMinutes: 1}}}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing), http.StatusOK)
	descriptor := hubTestPolicy()
	for _, f := range []nativeFixture{a, b, general} {
		approveHubTestPolicy(t, service, f.base+"/policy", descriptor)
	}
	claim := tracker.NativeClaim{PolicyID: descriptor.ID, MachineID: r.binding.MachineID, SessionID: "idle", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, general.base+"/claims", r.redemption.Credential, claim), http.StatusConflict)
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	service = openTestService(t, config)
	now = now.Add(time.Minute)
	general.service, a.service, b.service = service, service, service
	general.create(t, "general job")
	claim.SessionID = "general"
	response := performHubAPIRequest(t, service, http.MethodPost, general.base+"/claims", r.redemption.Credential, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var generalLease tracker.NativeLease
	decodeHubResponse(t, response, &generalLease)
	first := a.create(t, "older home")
	second := b.create(t, "priority home")
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override = 0 WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)", second.WorkItemID); err != nil {
		t.Fatal(err)
	}
	claim.SessionID = "home"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, a.base+"/claims", r.redemption.Credential, claim), http.StatusConflict)
	response = performHubAPIRequest(t, service, http.MethodPost, b.base+"/claims", r.redemption.Credential, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var homeLease tracker.NativeLease
	decodeHubResponse(t, response, &homeLease)
	if homeLease.WorkItemID != second.WorkItemID {
		t.Fatalf("picked %s, want priority home %s", homeLease.WorkItemID, second.WorkItemID)
	}
	runner, err := readRunner(t.Context(), service.database.db, a.project.OrganizationID, r.binding.RunnerID, now)
	if err != nil {
		t.Fatal(err)
	}
	if runner.HomeDrySince != nil || runner.HomeStatus != "Preferring home work" || runner.Used != 2 {
		t.Fatalf("home return = %#v", runner)
	}
	for _, lease := range []tracker.NativeLease{generalLease, homeLease} {
		if err := service.database.Release(t.Context(), tracker.ReleaseRequest{LeaseID: lease.ID, FencingToken: lease.FencingToken, Reason: "completed"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE workflow_states SET dispatchable = 0 WHERE project_id IN (?, ?)", a.project.ID, b.project.ID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	claim.SessionID = "dry-again"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, general.base+"/claims", r.redemption.Credential, claim), http.StatusConflict)
	runner, err = readRunner(t.Context(), service.database.db, a.project.OrganizationID, r.binding.RunnerID, now)
	if err != nil {
		t.Fatal(err)
	}
	if runner.HomeDrySince == nil || !runner.HomeDrySince.Equal(now) {
		t.Fatalf("timer did not restart after home work: %#v", runner.HomeDrySince)
	}
	if first.WorkItemID == homeLease.WorkItemID {
		t.Fatal("home project list order overrode queue priority")
	}
}

func TestRunnerHomeRoutingCompatibility(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Home runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}, HomeProjectIDs: []tracker.ProjectID{f.project.ID}}}
	response := performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change)
	requireNativeStatus(t, response, http.StatusOK)
	var stored runnerauth.Runner
	decodeHubResponse(t, response, &stored)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET home_dry_since = ? WHERE id = ?", formatHubTime(f.service.config.now()), r.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{"expected_revision": stored.Revision, "display_name": "Renamed", "state": "active", "capacity_limit": 2, "project_ids": stored.ProjectIDs}
	response = performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, legacy)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &stored)
	if len(stored.HomeProjectIDs) != 1 || stored.HomeDrySince == nil {
		t.Fatalf("legacy update lost home settings: %#v", stored)
	}
	snapshot, err := readRunnerRoutingSnapshot(t.Context(), f.service.database.db, f.project.OrganizationID, r.binding.RunnerID, f.service.config.now())
	if err != nil || len(snapshot.Routing.HomeProjectIDs) != 1 {
		t.Fatalf("heartbeat snapshot = %#v, %v", snapshot, err)
	}
	legacy["expected_revision"] = stored.Revision
	legacy["home_project_ids"] = []tracker.ProjectID{"prj_unauthorized"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, legacy), http.StatusUnprocessableEntity)
	legacy["home_project_ids"] = []tracker.ProjectID{}
	response = performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, legacy)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &stored)
	if len(stored.HomeProjectIDs) != 0 || stored.HomeDrySince != nil {
		t.Fatalf("explicit clear retained home state: %#v", stored)
	}
}

func TestRunnerHomeProviderEligibility(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	home := newNativeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}), "", "home-provider")
	general := newNativeFixture(t, home.service, home.project.OrganizationID, "general-provider")
	r := prepareRunner(t, home, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	routing := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Provider runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{home.project.ID, general.project.ID}, HomeProjectIDs: []tracker.ProjectID{home.project.ID}, Spillover: runnerauth.Spillover{Mode: "after", AfterMinutes: 0}}}
	requireNativeStatus(t, performHubAPIRequest(t, home.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing), http.StatusOK)
	for _, f := range []nativeFixture{home, general} {
		approveHubTestPolicy(t, home.service, f.base+"/policy", hubTestPolicy())
	}
	home.create(t, "unavailable model")
	generalIssue := general.create(t, "available model")
	publishCapacity(t, home, r, capacityReport(now))
	claim := providerClaim(r, generalIssue, "general-provider")
	response := performHubAPIRequest(t, home.service, http.MethodPost, general.base+"/claims", r.redemption.Credential, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if lease.WorkItemID != generalIssue.WorkItemID || lease.ProviderReservation == nil {
		t.Fatalf("provider spillover = %#v", lease)
	}
}

func TestRunnerHomeFleetVisibility(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	runner := runnerauth.Runner{Routing: runnerauth.Routing{HomeProjectIDs: []tracker.ProjectID{"prj_visible", "prj_hidden"}}, HomeDrySince: &since, HomeStatus: "Spilled over"}
	view := hostedFleetRunnerView(runner, "test", map[tracker.ProjectID]bool{"prj_visible": true}, since)
	if len(view.HomeProjectIDs) != 1 || view.HomeStatus != "" || view.HomeDrySince != nil {
		t.Fatalf("hidden home activity leaked: %#v", view)
	}
	view = hostedFleetRunnerView(runner, "test", map[tracker.ProjectID]bool{"prj_visible": true, "prj_hidden": true}, since)
	if len(view.HomeProjectIDs) != 2 || view.HomeStatus != "Spilled over" || view.HomeDrySince == nil {
		t.Fatalf("visible home state lost: %#v", view)
	}
}

func TestRunnerHomeWorkflowFilter(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	routing := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Home runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}, HomeProjectIDs: []tracker.ProjectID{f.project.ID}}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing), http.StatusOK)
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	f.create(t, "earlier todo")
	rework := f.create(t, "eligible rework")
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE workflow_states SET detent_state = 'Rework' WHERE project_id = ? AND detent_state = 'In Progress'", f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id = (SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = 'Rework') WHERE native_id = ?", f.project.ID, rework.WorkItemID); err != nil {
		t.Fatal(err)
	}
	claim := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, MachineID: r.binding.MachineID, SessionID: "rework-only", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}, WorkflowStates: []string{" Rework "}}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if lease.WorkItemID != rework.WorkItemID {
		t.Fatalf("rework claim = %#v", lease)
	}
}
