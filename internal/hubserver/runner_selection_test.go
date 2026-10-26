package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerAllowedProviderEligibility(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name         string
		unavailable  int
		ready        bool
		contract     string
		landing      string
		providerPage bool
	}{
		{"unavailable project yields to ready project", 1, false, "", "", false},
		{"ready work beyond first candidate page wins", 100, true, "", "", false},
		{"invalid contract yields to ready project", 1, false, "missing", "", false},
		{"invalid contract yields to next candidate", 1, true, "missing", "", false},
		{"provider page beyond project quota", 50, true, "missing", "", true},
		{"provider page hydration exhaustion", 99, true, "hydration", "", true},
		{"rejected prefix beyond budget in one project", 125, true, "missing", "", false},
		{"rejected prefix beyond budget across projects", 125, false, "missing", "", false},
		{"hydration exhaustion advances to next candidate", 125, true, "hydration", "", false},
		{"policy changes revisit rejected candidates", 125, true, "policy", "", false},
		{"legacy checkpoint hydration cannot exhaust a candidate", 125, true, "checkpoints", "", false},
		{"dispatch marker yields to next candidate", 1, true, "marker", "", false},
		{"unknown legacy landing ceiling cannot strand another project", 0, false, "", "legacy owner", false},
		{"positive owner pin cannot prove a foreign incoming ceiling", 0, false, "", "pinned owner", false},
		{"native-only request preserves unknown foreign capacity", 0, false, "", "native-only request", false},
		{"all legacy cross-project claims preserve unbounded behavior", 0, false, "", "legacy request", false},
		{"unoccupied foreign landing retains project ranking", 0, false, "", "unoccupied", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			preferred := newNativeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}), "", "preferred-provider")
			var landing landingCapacityFixture
			if scenario.landing != "" {
				landing = newLandingCapacityFixture(t, preferred.service, "preferred-landings", true)
				preferred = landing.nativeFixture
			}
			general := newNativeFixture(t, preferred.service, preferred.project.OrganizationID, "general-provider")
			if scenario.unavailable > claimPreparationLimit && preferred.project.ID > general.project.ID {
				preferred, general = general, preferred
			}

			r := prepareRunner(t, preferred, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			routing := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Provider runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{preferred.project.ID, general.project.ID}}}
			requireNativeStatus(t, performHubAPIRequest(t, preferred.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing), http.StatusOK)
			if scenario.landing != "" {
				approveHubTestPolicy(t, preferred.service, general.base+"/policy", hubTestPolicy())
				owner := prepareRunner(t, preferred, runnerauth.Read, runnerauth.Claim)
				owner.enroll(t)
				first, next := landing.ready(t, "legacy owner"), landing.ready(t, "next landing")
				if scenario.landing != "unoccupied" {
					ownerCapacity := 0
					if scenario.landing == "pinned owner" || scenario.landing == "native-only request" {
						ownerCapacity = 3
					}
					requireNativeStatus(t, landing.claim(t, owner, first, "legacy-landing", ownerCapacity), http.StatusOK)
					requireNativeStatus(t, landing.claim(t, r, next, "bounded-landing", 1), http.StatusConflict)
				}
				if _, err := preferred.service.database.db.ExecContext(t.Context(), "UPDATE projects SET scheduling_rank=1 WHERE id=?", general.project.ID); err != nil {
					t.Fatal(err)
				}
				issue := general.create(t, "ready source work")
				claim := tracker.NativeClaim{LandingCapacity: 1, PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "general-source", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
				switch scenario.landing {
				case "native-only request":
					claim.LandingCapacity = 0
					claim.Capabilities = append(claim.Capabilities, tracker.NativeLandingCapacityCapability)
				case "legacy request":
					claim.LandingCapacity = 0
				}
				response := performHubAPIRequest(t, preferred.service, http.MethodPost, general.base+"/claims", r.redemption.Credential, claim)
				if scenario.landing == "legacy request" || scenario.landing == "unoccupied" {
					requireNativeStatus(t, response, http.StatusConflict)
					capacity := 1
					if scenario.landing == "legacy request" {
						capacity = 0
					}
					requireNativeStatus(t, landing.claim(t, r, next, "preferred-landing", capacity), http.StatusOK)
					return
				}
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				if lease.WorkItemID != issue.WorkItemID {
					t.Fatalf("unproven competing landing stranded source work: %#v", lease)
				}
				response = performHubAPIRequest(t, preferred.service, http.MethodGet, preferred.base+"/work-items/"+string(next.WorkItemID)+"/runtime", preferred.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var evidence tracker.NativeRuntimeEvidence
				decodeHubResponse(t, response, &evidence)
				if evidence.LatestDecision == nil || evidence.LatestDecision.Data.Decision == nil || evidence.LatestDecision.Data.Decision.Reason != "Project landing capacity cannot be determined without the incoming ceiling" {
					t.Fatalf("foreign capacity refusal blamed reviewed source: %#v", evidence.LatestDecision)
				}
				return
			}
			if _, err := preferred.service.database.db.ExecContext(t.Context(), "UPDATE projects SET scheduling_rank=CASE WHEN id=? THEN 1 ELSE 2 END WHERE organization_id=?", preferred.project.ID, preferred.project.OrganizationID); err != nil {
				t.Fatal(err)
			}
			for _, f := range []nativeFixture{preferred, general} {
				approveHubTestPolicy(t, preferred.service, f.base+"/policy", hubTestPolicy())
			}
			var providerCandidates []tracker.NativeCapacityCandidate
			var rejected tracker.NativeIssue
			rejectedIDs := make(map[tracker.NativeWorkItemID]bool)
			for i := range scenario.unavailable {
				issue := preferred.create(t, fmt.Sprintf("unavailable candidate %d", i))
				if scenario.contract == "" {
					body := issue.Body + "\n```detent-agent\nschema: 1\nmodel: unavailable-model\n```\n"
					if _, err := preferred.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body=? WHERE native_id=?", body, issue.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
				rejectedIDs[issue.WorkItemID] = true
				if scenario.providerPage {
					providerCandidates = append(providerCandidates, providerClaim(r, issue, "").ProviderCandidates[0])
				}
				if i == 0 {
					rejected = issue
				}
				if scenario.contract == "hydration" {
					tx, err := preferred.service.database.db.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					attempt := seedHealthRateAttempt(t, tx, nativeScope{organization: preferred.project.OrganizationID, project: preferred.project.ID}, issue, string(r.binding.MachineID), "succeeded", tracker.NativeRunData{AttemptID: string(issue.WorkItemID)}, now, -1)
					if _, err := tx.ExecContext(t.Context(), `UPDATE native_attempts SET work_item_revision=?,dispatch_generation=(SELECT dispatch_generation FROM issues WHERE native_id=?) WHERE id=?`, issue.Revision, issue.WorkItemID, attempt); err != nil {
						t.Fatal(err)
					}
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
				}
				if scenario.contract != "" {
					body := ""
					if scenario.contract == "marker" {
						body = issue.Body + "\n<!-- detent:no-dispatch -->"
					}
					if _, err := preferred.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body=? WHERE native_id=?", body, issue.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
			}
			var preferredIssue tracker.NativeIssue
			if scenario.ready {
				preferredIssue = preferred.create(t, "ready after unavailable candidates")
				if scenario.contract == "checkpoints" {
					tx, err := preferred.service.database.db.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					for index := range 99 {
						attempt := seedHealthRateAttempt(t, tx, nativeScope{organization: preferred.project.OrganizationID, project: preferred.project.ID}, preferredIssue, string(r.binding.MachineID), "failed", tracker.NativeRunData{}, now.Add(time.Duration(index)*time.Second), -1)
						if _, err := tx.ExecContext(t.Context(), `UPDATE native_attempts SET checkpoint_json='{"resume":"fresh_checkout","worktree_state":"clean"}',checkpoint_preserves_source=NULL WHERE id=?`, attempt); err != nil {
							t.Fatal(err)
						}
					}
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
				}

				if _, err := preferred.service.database.db.ExecContext(t.Context(), "UPDATE issues SET native_created_at = ? WHERE native_id = ?", formatHubTime(now.Add(time.Second)), preferredIssue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			var generalIssue tracker.NativeIssue
			if !scenario.providerPage {
				generalIssue = general.create(t, "available model")
			}
			if scenario.contract == "" || scenario.providerPage {
				report := capacityReport(now)
				report.Models = append(report.Models, "gpt-6.1-sol")
				publishCapacity(t, preferred, r, report)
			}
			claim := providerClaim(r, generalIssue, "general-provider")
			if scenario.ready {
				claim.ProviderCandidates = append(claim.ProviderCandidates, providerClaim(r, preferredIssue, "").ProviderCandidates[0])
			}
			if scenario.contract != "" {
				claim.ProviderCandidates = nil
				claim.WorkItemID = ""
			}
			if scenario.providerPage {
				claim.ProviderCandidates = append(providerCandidates, providerClaim(r, preferredIssue, "").ProviderCandidates[0])
			}
			if scenario.unavailable > claimPreparationLimit || scenario.providerPage {
				target, want := general, generalIssue
				if scenario.ready {
					target, want = preferred, preferredIssue
					if !scenario.providerPage || scenario.contract == "hydration" {
						routing.ExpectedRevision = 2
						routing.ProjectIDs = []tracker.ProjectID{preferred.project.ID}
						requireNativeStatus(t, performHubAPIRequest(t, preferred.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing), http.StatusOK)
					}
				}
				claimed := false
				for attempt := range 5 {
					claim.SessionID = fmt.Sprintf("bounded-%d", attempt)
					response := performHubAPIRequest(t, preferred.service, http.MethodPost, target.base+"/claims", r.redemption.Credential, claim)
					if response.Code == http.StatusConflict {
						continue
					}
					requireNativeStatus(t, response, http.StatusOK)
					var lease tracker.NativeLease
					decodeHubResponse(t, response, &lease)
					if lease.WorkItemID != want.WorkItemID {
						t.Fatalf("claimed %s, want %s", lease.WorkItemID, want.WorkItemID)
					}
					if scenario.ready && attempt == 0 {
						t.Fatal("claim exceeded the request budget")
					}
					claimed = true
					break
				}
				if !claimed {
					t.Fatal("eligible candidate starved beyond the budget")
				}
				if scenario.providerPage {
					return
				}
				if scenario.contract == "policy" {
					workflow := config.Workflow{Config: config.Default()}
					workflow.Config.Tracker.Kind = config.TrackerHubNative
					workflow.Config.BacklogAdmission.TargetState = "In Progress"
					descriptor, err := config.ResolvePolicy(workflow)
					if err != nil {
						t.Fatal(err)
					}
					approveHubTestPolicy(t, preferred.service, preferred.base+"/policy", descriptor)
					claim.PolicyID = descriptor.ID
				} else if _, err := preferred.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body=?,revision=revision+1 WHERE native_id=?", rejected.Body, rejected.WorkItemID); err != nil {
					t.Fatal(err)
				}
				for attempt := range 6 {
					claim.SessionID = fmt.Sprintf("revised-%d", attempt)
					response := performHubAPIRequest(t, preferred.service, http.MethodPost, preferred.base+"/claims", r.redemption.Credential, claim)
					if response.Code == http.StatusConflict {
						continue
					}
					requireNativeStatus(t, response, http.StatusOK)
					var lease tracker.NativeLease
					decodeHubResponse(t, response, &lease)
					if scenario.contract == "policy" && !rejectedIDs[lease.WorkItemID] || scenario.contract != "policy" && lease.WorkItemID != rejected.WorkItemID {
						t.Fatalf("revised claim=%s, want %s", lease.WorkItemID, rejected.WorkItemID)
					}
					return
				}
				t.Fatal("historical refusal vetoed revised work")
			}
			response := performHubAPIRequest(t, preferred.service, http.MethodPost, general.base+"/claims", r.redemption.Credential, claim)
			if scenario.ready {
				requireNativeStatus(t, response, http.StatusConflict)
				claim = providerClaim(r, preferredIssue, "preferred-provider")
				if scenario.contract != "" {
					claim.ProviderCandidates = nil
					claim.WorkItemID = ""
				}
				response = performHubAPIRequest(t, preferred.service, http.MethodPost, preferred.base+"/claims", r.redemption.Credential, claim)
				generalIssue = preferredIssue
			}
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			if lease.WorkItemID != generalIssue.WorkItemID || (scenario.contract == "" && lease.ProviderReservation == nil) {
				t.Fatalf("provider selection = %#v", lease)
			}
		})
	}
}

func TestRunnerAllowedWorkflowFilter(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	routing := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}}}
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

func TestRunnerAllowedOrdering(t *testing.T) {
	for _, scenario := range []struct {
		name                          string
		firstPriority, secondPriority int
		firstRank, secondRank         int
		firstAge, secondAge           time.Duration
		allowed                       string
		wantFirst                     bool
	}{
		{"allowlist filters higher priority", 1, 4, 0, 1, -time.Hour, time.Hour, "second", false},
		{"empty allowlist runs nothing", 1, 4, 0, 1, -time.Hour, time.Hour, "empty", false},
		{"project rank beats issue priority", 2, 1, 0, 1, -time.Hour, time.Hour, "both", true},
		{"foreign provider candidate competes", 2, 1, 1, 2, -time.Hour, time.Hour, "provider", true},
		{"provider claim honors runner rank override", 1, 2, 1, 2, -time.Hour, time.Hour, "provider-override", false},
		{"project rank breaks priority ties", 2, 2, 0, 1, time.Hour, -time.Hour, "both", true},
		{"organization runner follows organization rank", 2, 1, 0, 1, -time.Hour, time.Hour, "organization", true},
		{"runner rank override wins", 1, 2, 0, 1, -time.Hour, time.Hour, "override", false},
		{"issue priority breaks equal ranks", 2, 1, 0, 0, -time.Hour, time.Hour, "both", false},
		{"age breaks remaining ties", 2, 2, 0, 0, time.Hour, -time.Hour, "both", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			first := newDefaultNativeFixture(t, Config{})
			second := newNativeFixture(t, first.service, first.project.OrganizationID, "second")
			r := prepareRunner(t, first, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			allowed := []tracker.ProjectID{first.project.ID, second.project.ID}
			if scenario.allowed == "second" {
				allowed = []tracker.ProjectID{second.project.ID}
			}
			if scenario.allowed == "empty" {
				allowed = []tracker.ProjectID{}
			}
			routing := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: allowed}}
			if scenario.allowed == "organization" || scenario.allowed == "override" || scenario.allowed == "provider-override" {
				routing.Scope = "organization"
				routing.ProjectIDs = nil
			}
			if scenario.allowed == "override" || scenario.allowed == "provider-override" {
				routing.ProjectRanks = map[tracker.ProjectID]int{first.project.ID: 2, second.project.ID: 0}
			}
			requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing), http.StatusOK)
			descriptor := hubTestPolicy()
			now := first.service.config.now()
			issues := make(map[tracker.ProjectID]tracker.NativeIssue)
			for _, project := range []struct {
				fixture        nativeFixture
				priority, rank int
				age            time.Duration
			}{
				{first, scenario.firstPriority, scenario.firstRank, scenario.firstAge},
				{second, scenario.secondPriority, scenario.secondRank, scenario.secondAge},
			} {
				approveHubTestPolicy(t, first.service, project.fixture.base+"/policy", descriptor)
				issue := project.fixture.create(t, "ready")
				issues[project.fixture.project.ID] = issue
				for _, statement := range []struct {
					sql  string
					args []any
				}{
					{"UPDATE projects SET scheduling_rank = ? WHERE id = ?", []any{project.rank, project.fixture.project.ID}},
					{"UPDATE queue_entries SET priority_override = ? WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)", []any{project.priority - 1, issue.WorkItemID}},
					{"UPDATE issues SET native_created_at = ? WHERE native_id = ?", []any{formatHubTime(now.Add(project.age)), issue.WorkItemID}},
				} {
					if _, err := first.service.database.db.ExecContext(t.Context(), statement.sql, statement.args...); err != nil {
						t.Fatal(err)
					}
				}
			}
			claim := tracker.NativeClaim{PolicyID: descriptor.ID, MachineID: r.binding.MachineID, SessionID: "ordering", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			loser, winner := first, second
			if scenario.wantFirst {
				loser, winner = second, first
			}
			if scenario.allowed == "provider" || scenario.allowed == "provider-override" {
				report := capacityReport(now)
				report.Models = append(report.Models, "gpt-6.1-sol")
				publishCapacity(t, first, r, report)
				claim = providerClaim(r, issues[loser.project.ID], "lower-provider")
			}
			response := performHubAPIRequest(t, first.service, http.MethodPost, loser.base+"/claims", r.redemption.Credential, claim)
			if response.Code == http.StatusOK {
				t.Fatalf("lower-ranked or unauthorized project acquired work: %s", response.Body.String())
			}
			if scenario.allowed == "provider" || scenario.allowed == "provider-override" {
				claim = providerClaim(r, issues[winner.project.ID], "higher-provider")
			}
			response = performHubAPIRequest(t, first.service, http.MethodPost, winner.base+"/claims", r.redemption.Credential, claim)
			if scenario.allowed == "empty" {
				if response.Code == http.StatusOK {
					t.Fatal("empty allowlist acquired work")
				}
			} else {
				requireNativeStatus(t, response, http.StatusOK)
			}
		})
	}
}

func TestRunnerFutureProjectAdmission(t *testing.T) {
	for _, scenario := range []struct {
		name, scope       string
		approved, deleted bool
		want              int
	}{
		{"organization admits future project", "organization", true, false, http.StatusOK},
		{"project scope stays restricted", "projects", true, false, http.StatusNotFound},
		{"organization requires reported configuration", "organization", false, false, http.StatusUnprocessableEntity},
		{"organization omits deleted project", "organization", true, true, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunnerForScope(t, f, scenario.scope, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			later := newNativeFixture(t, f.service, f.project.OrganizationID, "future")

			descriptor := hubTestPolicy()
			if scenario.approved {
				approveHubTestPolicy(t, f.service, later.base+"/policy", descriptor)
			}
			item := later.create(t, "future work")
			if !scenario.approved {
				clearNativeFixturePolicy(t, later)
			}
			if scenario.deleted {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET deleted_at=? WHERE id=?", formatHubTime(f.service.config.now()), later.project.ID); err != nil {
					t.Fatal(err)
				}
				projects, err := readRunnerProjects(t.Context(), f.service.database.db, r.binding.RunnerID)
				if err != nil {
					t.Fatal(err)
				}
				ranks, err := readRunnerProjectRanks(t.Context(), f.service.database.db, r.binding.RunnerID, f.project.OrganizationID)
				if err != nil {
					t.Fatal(err)
				}
				if _, ranked := ranks[later.project.ID]; ranked || !slices.Equal(projects, []tracker.ProjectID{f.project.ID}) {
					t.Fatalf("projects=%v ranks=%v advertise deleted %s", projects, ranks, later.project.ID)
				}
				return
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, later.base+"/claims", r.redemption.Credential, tracker.NativeClaim{PolicyID: descriptor.ID, MachineID: r.binding.MachineID, SessionID: "future", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
			requireNativeStatus(t, response, scenario.want)
			if scenario.want == http.StatusOK {
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				if lease.WorkItemID != item.WorkItemID {
					t.Fatalf("claimed %s, want %s", lease.WorkItemID, item.WorkItemID)
				}
			}
		})
	}
}

func TestRunnerAllowedCursorCASStaleRetry(t *testing.T) {
	for _, initialRevision := range []int64{0, 1} {
		t.Run(fmt.Sprintf("revision-%d", initialRevision), func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			r := placementFixtureRunner(t, f, "cursor-worker", "local", false, 1, "gpt-6.1-sol", "test-model")
			credential, _, err := f.service.authenticateAPIToken(t.Context(), r.redemption.Credential, "", "")
			if err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
			db := f.service.database
			if initialRevision != 0 {
				if _, err := db.db.ExecContext(t.Context(), `INSERT INTO runner_selection_cursors(runner_id,project_id,issue_id,priority,aging,landing,created,identifier,revision) VALUES(?,?,10,0,0,0,'','',1)`, r.binding.RunnerID, f.project.ID); err != nil {
					t.Fatal(err)
				}
			}
			query := claimCandidateQuery{NativeScope: &scope, Scope: string(scope.project)}
			prepare := func() (context.Context, *runnerSelectionProgress) {
				t.Helper()
				ctx := context.WithValue(t.Context(), claimPreparationKey{}, &claimPreparation{})
				progress, err := readRunnerSelectionProgress(ctx, db.reader, scope, query)
				if err != nil {
					t.Fatal(err)
				}
				return ctx, progress
			}
			winnerCtx, winner := prepare()
			loserCtx, loser := prepare()
			write := func(ctx context.Context, progress *runnerSelectionProgress, id tracker.WorkItemID, session string) {
				t.Helper()
				progress.cursor.ID, progress.dirty = id, true
				request := tracker.ClaimRequest{MachineID: r.binding.MachineID, SessionID: session, TTL: time.Minute}
				_, err := db.claimNextOn(ctx, db.db, nil, claimMode{hint: &claimSelected{id: 999999, revision: 1}}, request, query, time.Hour)
				if !errors.Is(err, errClaimHintStale) {
					t.Fatalf("stale candidate pass: %v", err)
				}
			}
			write(winnerCtx, winner, 40, "winner")
			write(loserCtx, loser, 20, "loser")
			write(loserCtx, loser, 25, "loser")
			var id tracker.WorkItemID
			var revision int64
			if err := db.reader.QueryRowContext(t.Context(), `SELECT issue_id,revision FROM runner_selection_cursors WHERE runner_id=? AND project_id=?`, r.binding.RunnerID, f.project.ID).Scan(&id, &revision); err != nil {
				t.Fatal(err)
			}
			if id != 40 || revision != initialRevision+1 {
				t.Fatalf("CAS loser replaced winner progress: cursor=%d revision=%d", id, revision)
			}
			write(winnerCtx, winner, 50, "winner")
			if err := db.reader.QueryRowContext(t.Context(), `SELECT issue_id,revision FROM runner_selection_cursors WHERE runner_id=? AND project_id=?`, r.binding.RunnerID, f.project.ID).Scan(&id, &revision); err != nil {
				t.Fatal(err)
			}
			if id != 50 || revision != initialRevision+2 {
				t.Fatalf("winner could not continue: cursor=%d revision=%d", id, revision)
			}
		})
	}
}
