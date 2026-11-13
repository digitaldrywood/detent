package hubserver

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
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
		{"equal rank quota waits for better priority", claimPreparationLimit/2 + 1, true, "priority quota", "", false},
		{"higher project pending scan beats lower project in progress", claimPreparationLimit/2 + 1, true, "rank quota", "", false},
		{"lower project pending scan yields to higher project todo", claimPreparationLimit/2 + 1, false, "lower rank quota", "", false},
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
			preferred := newNativeFixture(t, newClockedPostgresNativeService(t, Config{now: func() time.Time { return now }}), "", "preferred-provider")
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
			r.reportCapacity(t)
			routing := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Provider runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{preferred.project.ID, general.project.ID}}}
			requireNativeStatus(t, performHubAPIRequest(t, preferred.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing), http.StatusOK)
			if scenario.landing != "" {
				approveHubTestPolicy(t, preferred.service, general.base+"/policy", hubTestPolicy())
				owner := prepareRunner(t, preferred, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
				owner.enroll(t)
				owner.reportCapacity(t)
				first, next := landing.ready(t, "legacy owner"), landing.ready(t, "next landing")
				if scenario.landing != "unoccupied" {
					ownerCapacity := 0
					if scenario.landing == "pinned owner" || scenario.landing == "native-only request" {
						ownerCapacity = 3
					}
					requireNativeStatus(t, landing.claim(t, owner, first, "legacy-landing", ownerCapacity), http.StatusOK)
					requireNativeStatus(t, landing.claim(t, r, next, "bounded-landing", 1), http.StatusConflict)
				}
				if _, err := usagePostgresAdmin(t, preferred.service).ExecContext(t.Context(), "UPDATE projects SET scheduling_rank=1 WHERE id=$1", general.project.ID); err != nil {
					t.Fatal(err)
				}
				issue := general.create(t, "ready source work")
				claim := tracker.NativeClaim{LandingCapacity: 1, PolicyID: hubTestPolicy().ID, MachineID: r.binding.MachineID, SessionID: "general-source", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
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
			if _, err := usagePostgresAdmin(t, preferred.service).ExecContext(t.Context(), "UPDATE projects SET scheduling_rank=CASE WHEN id=$1 THEN 1 ELSE 2 END WHERE organization_id=$2", preferred.project.ID, preferred.project.OrganizationID); err != nil {
				t.Fatal(err)
			}
			if scenario.contract == "priority quota" {
				if _, err := usagePostgresAdmin(t, preferred.service).ExecContext(t.Context(), "UPDATE projects SET scheduling_rank=1 WHERE organization_id=$1", preferred.project.OrganizationID); err != nil {
					t.Fatal(err)
				}
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
					issue = updateSelectionFixtureBody(t, preferred, issue, body)
				}
				rejectedIDs[issue.WorkItemID] = true
				if i == 0 {
					rejected = issue
				}
				if scenario.contract == "hydration" {
					recordSelectionFixtureAttempt(t, preferred, r, issue, "succeeded", nil)
				}
				if scenario.contract != "" {
					body := ""
					if scenario.contract == "marker" {
						body = issue.Body + "\n<!-- detent:no-dispatch -->"
					}
					issue = updateSelectionFixtureBody(t, preferred, issue, body)
				}
				if scenario.providerPage {
					providerCandidates = append(providerCandidates, providerClaim(r, issue, "").ProviderCandidates[0])
				}
			}
			var preferredIssue tracker.NativeIssue
			if scenario.ready {
				preferredIssue = preferred.create(t, "ready after unavailable candidates")
				if scenario.contract == "checkpoints" {
					attemptIDs := make([]string, 0, 99)
					for range 99 {
						attemptIDs = append(attemptIDs, recordSelectionFixtureAttempt(t, preferred, r, preferredIssue, "failed", &tracker.NativeCheckpoint{Resume: "fresh_checkout", WorktreeState: "clean", Storage: "local_only", Availability: "available", ExternalEffect: "none", EffectState: "none"}))
					}
					placeholders := make([]string, len(attemptIDs))
					args := []any{preferred.project.OrganizationID, preferred.project.ID, preferredIssue.WorkItemID}
					for index, id := range attemptIDs {
						placeholders[index] = fmt.Sprintf("$%d", index+4)
						args = append(args, id)
					}
					result, err := usagePostgresAdmin(t, preferred.service).ExecContext(t.Context(), "UPDATE native_attempts SET checkpoint_preserves_source=NULL WHERE organization_id=$1 AND project_id=$2 AND issue_id=(SELECT id FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3) AND checkpoint_object_key IS NOT NULL AND id IN ("+strings.Join(placeholders, ",")+")", args...)
					if err != nil {
						t.Fatal(err)
					}
					if count, err := result.RowsAffected(); err != nil || count != int64(len(attemptIDs)) {
						t.Fatalf("unknown checkpoint classifications = %d, want %d: %v", count, len(attemptIDs), err)
					}
				}

				if _, err := usagePostgresAdmin(t, preferred.service).ExecContext(t.Context(), "UPDATE issues SET native_created_at = $1 WHERE native_id = $2", now.Add(time.Second), preferredIssue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			var generalIssue tracker.NativeIssue
			if !scenario.providerPage {
				generalIssue = general.create(t, "available model")
			}
			if scenario.contract == "rank quota" {
				setSelectionFixtureState(t, general, generalIssue.WorkItemID, "In Progress")
			}
			if scenario.contract == "lower rank quota" {
				if _, err := usagePostgresAdmin(t, preferred.service).ExecContext(t.Context(), "UPDATE projects SET scheduling_rank=CASE WHEN id=$1 THEN 3 ELSE 1 END WHERE organization_id=$2", preferred.project.ID, preferred.project.OrganizationID); err != nil {
					t.Fatal(err)
				}
				for item := range rejectedIDs {
					setSelectionFixtureState(t, preferred, item, "In Progress")
				}
				if preferredIssue.WorkItemID != "" {
					setSelectionFixtureState(t, preferred, preferredIssue.WorkItemID, "In Progress")
				}
			}
			if scenario.contract == "priority quota" {
				if _, err := usagePostgresAdmin(t, preferred.service).ExecContext(t.Context(), "UPDATE queue_entries SET priority_override=CASE WHEN project_id=$1 THEN 0 ELSE 3 END", preferred.project.ID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.contract == "" || scenario.providerPage {
				report := capacityReport(now)
				report.Models = append(report.Models, "gpt-6.1-sol")
				publishCapacity(t, preferred, r, report)
			}
			claim := providerClaim(r, generalIssue, "general-provider")
			if scenario.ready {
				claim.WorkItemID = ""
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
				} else {
					response := performHubAPIRequest(t, preferred.service, http.MethodGet, preferred.base+"/work-items/"+string(rejected.WorkItemID), preferred.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var current tracker.NativeIssue
					decodeHubResponse(t, response, &current)
					updateSelectionFixtureBody(t, preferred, current, rejected.Body)
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
	r.reportCapacity(t)
	routing := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing), http.StatusOK)
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	f.create(t, "earlier todo")
	rework := f.create(t, "eligible rework")

	setSelectionFixtureState(t, f, rework.WorkItemID, "Rework")
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
		firstLanding, secondLanding   string
		localWork                     bool
		firstRework, secondRework     bool
	}{
		{"allowlist filters higher priority", 1, 4, 0, 1, -time.Hour, time.Hour, "second", false, "", "", false, false, false},
		{"empty allowlist runs nothing", 1, 4, 0, 1, -time.Hour, time.Hour, "empty", false, "", "", false, false, false},
		{"project capacity yields to another project", 1, 4, 0, 1, -time.Hour, time.Hour, "project-full", false, "", "", false, false, false},
		{"project rank beats issue priority", 2, 1, 0, 1, -time.Hour, time.Hour, "both", true, "", "", false, false, false},
		{"targeted work item honors project rank", 2, 1, 1, 3, -time.Hour, time.Hour, "targeted", true, "", "", false, false, false},
		{"targeted lower project in progress yields", 3, 1, 3, 1, time.Hour, -time.Hour, "targeted-in-progress", false, "", "", false, false, false},
		{"targeted lower project rework yields", 3, 1, 3, 1, time.Hour, -time.Hour, "targeted", false, "", "", false, true, false},
		{"targeted lower project landing yields", 3, 1, 3, 1, time.Hour, -time.Hour, "targeted", false, "reviewed", "", false, false, false},
		{"single provider candidate honors project rank", 2, 1, 1, 3, -time.Hour, time.Hour, "single-provider", true, "", "", false, false, false},
		{"single provider lower project in progress yields", 3, 1, 3, 1, time.Hour, -time.Hour, "single-provider-in-progress", false, "", "", false, false, false},
		{"single provider lower project rework yields", 3, 1, 3, 1, time.Hour, -time.Hour, "single-provider", false, "", "", false, true, false},
		{"single provider lower project landing yields", 3, 1, 3, 1, time.Hour, -time.Hour, "single-provider", false, "reviewed", "", false, false, false},
		{"foreign provider candidate competes", 2, 1, 1, 2, -time.Hour, time.Hour, "provider", true, "", "", false, false, false},
		{"provider claim honors runner rank override", 1, 2, 1, 2, -time.Hour, time.Hour, "provider-override", false, "", "", false, false, false},
		{"project rank breaks priority ties", 2, 2, 0, 1, time.Hour, -time.Hour, "both", true, "", "", false, false, false},
		{"organization runner follows organization rank", 2, 1, 0, 1, -time.Hour, time.Hour, "organization", true, "", "", false, false, false},
		{"runner rank override wins", 1, 2, 0, 1, -time.Hour, time.Hour, "override", false, "", "", false, false, false},
		{"issue priority breaks equal ranks", 2, 1, 0, 0, -time.Hour, time.Hour, "both", false, "", "", false, false, false},
		{"age breaks remaining ties", 2, 2, 0, 0, time.Hour, -time.Hour, "both", false, "", "", false, false, false},
		{"reviewed landing then P2 rework then P0 todo", 3, 1, 0, 0, time.Hour, -time.Hour, "both", true, "reviewed", "", true, false, false},
		{"higher project todo beats lower project landing", 3, 1, 3, 1, -time.Hour, time.Hour, "both", false, "reviewed", "", false, false, false},
		{"higher project rework beats lower project landing", 3, 1, 3, 1, -time.Hour, time.Hour, "both", false, "reviewed", "", false, false, true},
		{"landings follow project rank", 1, 3, 1, 0, -time.Hour, time.Hour, "both", false, "reviewed", "reviewed", false, false, false},
		{"landing priority precedes creation", 3, 2, 0, 0, -time.Hour, time.Hour, "both", false, "reviewed", "reviewed", false, false, false},
		{"landing creation breaks priority ties", 3, 3, 0, 0, -time.Hour, time.Hour, "both", true, "reviewed", "reviewed", false, false, false},
		{"missing landing version yields to implementation", 1, 3, 0, 0, -time.Hour, time.Hour, "both", false, "missing version", "", false, false, false},
		{"unreviewed landing yields to implementation", 1, 3, 0, 0, -time.Hour, time.Hour, "both", false, "missing review", "", false, false, false},
		{"failed landing check yields to implementation", 1, 3, 0, 0, -time.Hour, time.Hour, "both", false, "failed check", "", false, false, false},
		{"P2 rework beats P0 todo at equal project ranks", 3, 1, 0, 0, time.Hour, -time.Hour, "both", true, "", "", false, true, false},
		{"higher project todo beats lower project rework", 3, 1, 3, 1, -time.Hour, time.Hour, "both", false, "", "", false, true, false},
		{"rework follows project rank", 1, 3, 1, 0, -time.Hour, time.Hour, "both", false, "", "", false, true, true},
		{"rework priority precedes creation", 3, 2, 0, 0, -time.Hour, time.Hour, "both", false, "", "", false, true, true},
		{"rework creation breaks priority ties", 3, 3, 0, 0, -time.Hour, time.Hour, "both", true, "", "", false, true, true},
		{"higher project todo beats lower project in progress", 3, 1, 3, 1, time.Hour, -time.Hour, "in-progress", false, "", "", false, false, false},
		{"higher project in progress beats lower project rework", 1, 3, 1, 3, -time.Hour, time.Hour, "in-progress", true, "", "", false, false, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			targetMode := strings.TrimSuffix(scenario.allowed, "-in-progress")
			first := newDefaultNativeFixture(t, Config{})
			second := newNativeFixture(t, first.service, first.project.OrganizationID, "second")
			if scenario.firstLanding != "" || scenario.secondLanding != "" {
				first = newLandingCapacityFixture(t, first.service, "first-landings", false).nativeFixture
				second = newLandingCapacityFixture(t, first.service, "second-landings", false).nativeFixture
			}
			r := prepareRunner(t, first, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			r.reportCapacity(t)
			allowed := []tracker.ProjectID{first.project.ID, second.project.ID}
			if scenario.allowed == "second" {
				allowed = []tracker.ProjectID{second.project.ID}
			}
			if scenario.allowed == "empty" {
				allowed = []tracker.ProjectID{}
			}
			routing := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 1, ProjectIDs: allowed}}
			if scenario.allowed == "project-full" {
				routing.ProjectCapacityLimits = map[tracker.ProjectID]int{first.project.ID: 0, second.project.ID: 1}
			}
			if scenario.allowed == "organization" || scenario.allowed == "override" || scenario.allowed == "provider-override" {
				routing.Scope = "organization"
				routing.ProjectIDs = nil
			}
			if scenario.allowed == "override" || scenario.allowed == "provider-override" {
				routing.ProjectRanks = map[tracker.ProjectID]int{first.project.ID: 2, second.project.ID: 0}
			}
			requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, runnerRoutingRequest{RoutingChange: routing, IdempotencyKey: "ordering"}), http.StatusOK)
			descriptor := hubTestPolicy()
			now := first.service.config.now()
			issues := make(map[tracker.ProjectID]tracker.NativeIssue)
			for _, project := range []struct {
				fixture        nativeFixture
				priority, rank int
				age            time.Duration
				landing        string
				rework         bool
			}{
				{first, scenario.firstPriority, scenario.firstRank, scenario.firstAge, scenario.firstLanding, scenario.firstRework},
				{second, scenario.secondPriority, scenario.secondRank, scenario.secondAge, scenario.secondLanding, scenario.secondRework},
			} {
				approveHubTestPolicy(t, first.service, project.fixture.base+"/policy", descriptor)
				if project.rework || (scenario.localWork && project.fixture.project.ID == first.project.ID) {
					states := append(HostedProjectStates(), tracker.NativeState{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress"}})
					if err := first.service.config.TenantRepository.WriteTenant(t.Context(), first.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
						record, err := w.ProjectRecord(t.Context(), project.fixture.project.ID)
						if err != nil {
							return err
						}
						record.States = states
						_, err = w.UpdateProjectRecord(t.Context(), record)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				issue := project.fixture.create(t, "ready")
				if (scenario.allowed == "in-progress" || strings.HasSuffix(scenario.allowed, "-in-progress")) && project.fixture.project.ID == first.project.ID {

					setSelectionFixtureState(t, first, issue.WorkItemID, "In Progress")
				}
				if project.rework {
					setSelectionFixtureState(t, project.fixture, issue.WorkItemID, "Rework")
				}
				if project.landing != "" && project.landing != "missing version" {
					f := project.fixture
					var principal string
					if err := usagePostgresAdmin(t, first.service).QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE name=$1", "operator-"+f.project.Name).Scan(&principal); err != nil {
						t.Fatal(err)
					}
					rules := tracker.ChangeReviewPolicy{PolicyID: descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{{Name: "test", PrincipalID: principal, WorkflowID: "ci.yml", WorkflowSHA256: policy.Digest([]byte("trusted CI")), Source: "independent", MaxAgeSeconds: 3600}}}
					requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "rules"}, Policy: rules}), http.StatusOK)
					path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
					response := performHubAPIRequest(t, first.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Ready change"})
					requireNativeStatus(t, response, http.StatusOK)
					var change tracker.ChangeRequest
					decodeHubResponse(t, response, &change)
					cf := changeFixture{nativeFixture: f, issue: issue, change: change, path: path + "/" + change.ID}
					version := cf.publish(t, "publish", "", true)
					if project.landing != "missing review" {
						requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPost, cf.path+"/versions/"+version.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve"}, Decision: "approved"}), http.StatusOK)
					}
					check := changeTestResult(version)
					if project.landing == "failed check" {
						check.Conclusion = "failure"
					}
					requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPost, cf.path+"/versions/"+version.ID+"/checks", f.token, check), http.StatusOK)
					if status := cf.detail(t).Summary.Status; (status == "reviewed") != (project.landing == "reviewed") {
						t.Fatalf("landing summary = %s for %s", status, project.landing)
					}
				}
				if project.landing != "" {
					setSelectionFixtureState(t, project.fixture, issue.WorkItemID, "Merging")
				}
				issues[project.fixture.project.ID] = issue
				for _, statement := range []struct {
					sql  string
					args []any
				}{
					{"UPDATE projects SET scheduling_rank = $1 WHERE id = $2", []any{project.rank, project.fixture.project.ID}},
					{"UPDATE queue_entries SET priority_override = $1 WHERE work_item_id = $2", []any{project.priority - 1, issue.WorkItemID}},
					{"UPDATE issues SET native_created_at = $1 WHERE native_id = $2", []any{now.Add(project.age), issue.WorkItemID}},
				} {
					if _, err := usagePostgresAdmin(t, first.service).ExecContext(t.Context(), statement.sql, statement.args...); err != nil {
						t.Fatal(err)
					}
				}
			}
			if scenario.localWork {
				for _, work := range []struct {
					state    string
					priority int
				}{{"Rework", 2}, {"Todo", 0}} {
					issue := first.create(t, work.state)
					setSelectionFixtureState(t, first, issue.WorkItemID, work.state)
					if _, err := usagePostgresAdmin(t, first.service).ExecContext(t.Context(), "UPDATE queue_entries SET priority_override=$1 WHERE work_item_id=$2", work.priority, issue.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
				for _, selection := range []struct {
					runner bool
					want   []string
				}{
					{true, []string{"ready", "Rework", "Todo"}},
					{false, []string{"Todo", "ready", "Rework"}},
				} {
					scope := hubscope.NativeScope{Organization: first.project.OrganizationID, Project: first.project.ID}
					query := claimstore.CandidateQuery{ProjectID: scope.Project, Scope: string(scope.Project), RunnerSelection: selection.runner, AvailableAt: now, Limit: 1}
					for _, want := range selection.want {
						var page claimstore.CandidatePage
						var title string
						if err := first.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
							var err error
							page, err = reader.CandidatePage(t.Context(), query)
							if err != nil {
								return err
							}
							if len(page.Items) != 1 {
								t.Fatalf("candidate page = %+v", page)
							}
							item, err := reader.WorkItem(t.Context(), scope.Project, page.Items[0].WorkItemID)
							title = item.Title
							return err
						}); err != nil {
							t.Fatal(err)
						}
						query.After = page.Next
						if title != want {
							t.Fatalf("candidate = %s, want %s", title, want)
						}
					}
				}
			}
			claim := tracker.NativeClaim{PolicyID: descriptor.ID, MachineID: r.binding.MachineID, SessionID: "ordering", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			loser, winner := first, second
			if scenario.wantFirst {
				loser, winner = second, first
			}
			if scenario.allowed == "provider" || scenario.allowed == "provider-override" || targetMode == "single-provider" {
				report := capacityReport(now)
				report.Models = append(report.Models, "gpt-6.1-sol")
				publishCapacity(t, first, r, report)
				claim = providerClaim(r, issues[loser.project.ID], "lower-provider")
				claim.WorkItemID = ""
				claim.ProviderCandidates = append(claim.ProviderCandidates, providerClaim(r, issues[winner.project.ID], "").ProviderCandidates[0])
			}
			requestClaim := func(project nativeFixture) tracker.NativeClaim {
				if targetMode != "targeted" && targetMode != "single-provider" {
					return claim
				}
				preview := claim
				preview.WorkItemID, preview.ProviderCandidates = "", nil
				response := performHubAPIRequest(t, first.service, http.MethodPost, project.base+"/claims/preview", r.redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: preview})
				requireNativeStatus(t, response, http.StatusOK)
				var page tracker.NativeCapacityPage
				decodeHubResponse(t, response, &page)
				for _, issue := range page.Items {
					if issue.WorkItemID != issues[project.project.ID].WorkItemID {
						continue
					}
					if targetMode == "single-provider" {
						target := providerClaim(r, issue, claim.SessionID)
						target.WorkItemID = ""
						return target
					}
					preview.WorkItemID = issue.WorkItemID
					return preview
				}
				t.Fatal("project preview omitted its claimable work")
				return tracker.NativeClaim{}
			}
			assertRunnerAdmissionFacts(t, first, r)
			assertRunnerAdmissionFacts(t, second, r)
			response := performHubAPIRequest(t, first.service, http.MethodPost, loser.base+"/claims", r.redemption.Credential, requestClaim(loser))
			if response.Code == http.StatusOK {
				t.Fatalf("lower-ranked or unauthorized project acquired work: %s", response.Body.String())
			}
			assertRunnerAdmissionFacts(t, first, r)
			if scenario.allowed == "provider" || scenario.allowed == "provider-override" {
				claim.SessionID = "higher-provider"
			}
			response = performHubAPIRequest(t, first.service, http.MethodPost, winner.base+"/claims", r.redemption.Credential, requestClaim(winner))
			if scenario.allowed == "empty" {
				if response.Code == http.StatusOK {
					t.Fatal("empty allowlist acquired work")
				}
			} else {
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				if lease.WorkItemID != issues[winner.project.ID].WorkItemID {
					t.Fatalf("claimed %s, want %s", lease.WorkItemID, issues[winner.project.ID].WorkItemID)
				}
				if min(scenario.firstRank, scenario.secondRank) == 1 && max(scenario.firstRank, scenario.secondRank) == 3 {
					for round := range 2 {
						requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPost, winner.base+"/leases/"+string(lease.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
						claim.SessionID = fmt.Sprintf("freed-slot-%d", round)
						response = performHubAPIRequest(t, first.service, http.MethodPost, loser.base+"/claims", r.redemption.Credential, requestClaim(loser))
						requireNativeStatus(t, response, http.StatusConflict)
						response = performHubAPIRequest(t, first.service, http.MethodPost, winner.base+"/claims", r.redemption.Credential, requestClaim(winner))
						requireNativeStatus(t, response, http.StatusOK)
						decodeHubResponse(t, response, &lease)
						if lease.WorkItemID != issues[winner.project.ID].WorkItemID {
							t.Fatalf("freed slot claimed %s, want %s", lease.WorkItemID, issues[winner.project.ID].WorkItemID)
						}
					}
				}
				if targetMode == "targeted" || targetMode == "single-provider" {
					requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPost, winner.base+"/leases/"+string(lease.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
					setSelectionFixtureState(t, winner, issues[winner.project.ID].WorkItemID, "Done")
					claim.SessionID = "lower-project-fallback"
					response = performHubAPIRequest(t, first.service, http.MethodPost, loser.base+"/claims", r.redemption.Credential, requestClaim(loser))
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &lease)
					if lease.WorkItemID != issues[loser.project.ID].WorkItemID || targetMode == "single-provider" && lease.ProviderReservation == nil {
						t.Fatalf("fallback claim = %+v", lease)
					}
					assertRunnerAdmissionFacts(t, first, r)
				}
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
			r.reportCapacity(t)
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
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE projects SET deleted_at=$1 WHERE id=$2", f.service.config.now(), later.project.ID); err != nil {
					t.Fatal(err)
				}
				var projects []tracker.ProjectID
				var ranks map[tracker.ProjectID]int
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					var err error
					projects, err = reader.RunnerProjects(t.Context(), r.binding.RunnerID, runnerstore.MaxRunnerProjectList)
					if err != nil {
						return err
					}
					ranks, err = reader.(runnerProjectRankReader).RunnerProjectSchedulingRanks(t.Context(), r.binding.RunnerID, runnerstore.MaxRunnerProjectList)
					return err
				}); err != nil {
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
			now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return now }})
			r := prepareRunnerForScope(t, f, "organization", runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.redemption.Capacity = 4
			r.enroll(t)
			r.reportCapacity(t)
			second := newNativeFixture(t, f.service, f.project.OrganizationID, "second cursor")
			credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(r.redemption.Credential), "", "")
			if err != nil {
				t.Fatalf("cursor worker status=%d error=%v", status, err)
			}
			for _, project := range []nativeFixture{f, second} {
				approveHubTestPolicy(t, f.service, project.base+"/policy", hubTestPolicy())
				for index := range 4 {
					project.create(t, fmt.Sprintf("cursor work %d", index))
				}
				scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: project.project.ID, Credential: credential}
				request := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, MachineID: r.binding.MachineID, TTLSeconds: 60, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
				readScan := func() nativeClaimCandidateScan {
					t.Helper()
					scan, err := f.service.readNativeClaimCandidates(t.Context(), scope, request, now)
					if err != nil || len(scan.plans) != 4 {
						t.Fatalf("cursor candidates=%d error=%v", len(scan.plans), err)
					}
					return scan
				}
				if initialRevision != 0 {
					scan := readScan()
					if err := f.service.persistNativeClaimCursor(t.Context(), scope, scan.cursor); err != nil {
						t.Fatal(err)
					}
				}
				winnerScan, loserScan := readScan(), readScan()
				winner, loser := winnerScan.cursor, loserScan.cursor
				write := func(cursor claimstore.SelectionCursor, candidate claimstore.Candidate, want bool) {
					t.Helper()
					cursor.Position = claimCandidateCursor(candidate)
					err := f.service.persistNativeClaimCursor(t.Context(), scope, cursor)
					if want && err != nil || !want && !errors.Is(err, ErrNoClaimableWork) {
						t.Fatalf("selection progress write error=%v want committed=%v", err, want)
					}
				}
				write(winner, winnerScan.plans[2].candidate, true)
				write(loser, loserScan.plans[0].candidate, false)
				write(loser, loserScan.plans[1].candidate, false)
				var stored claimstore.SelectionCursor
				read := func() {
					t.Helper()
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
						var err error
						stored, err = reader.SelectionCursor(t.Context(), scope.Credential.Runner.RunnerID, scope.Project)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				read()
				if stored.Position.WorkItemID != winnerScan.plans[2].candidate.WorkItemID || stored.Revision != initialRevision+1 {
					t.Fatalf("CAS loser replaced winner progress: cursor=%s revision=%d", stored.Position.WorkItemID, stored.Revision)
				}
				winner.Revision = stored.Revision
				write(winner, winnerScan.plans[3].candidate, true)
				read()
				if stored.Position.WorkItemID != winnerScan.plans[3].candidate.WorkItemID || stored.Revision != initialRevision+2 {
					t.Fatalf("winner could not continue: cursor=%s revision=%d", stored.Position.WorkItemID, stored.Revision)
				}
				fresh := project.create(t, "work after cursor contention")
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
					queue, err := w.QueueEntry(t.Context(), scope.Project, fresh.WorkItemID, string(scope.Project))
					if err != nil {
						return err
					}
					queue.Rank = stored.Position.Rank + "z"
					return w.PutQueueEntry(t.Context(), queue)
				}); err != nil {
					t.Fatal(err)
				}
				request.SessionID = "cursor-progress-" + string(scope.Project)
				response := performHubAPIRequest(t, f.service, http.MethodPost, project.base+"/claims", r.redemption.Credential, request)
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				if lease.WorkItemID != fresh.WorkItemID {
					t.Fatalf("selection retried saved prefix: claimed=%s want=%s", lease.WorkItemID, fresh.WorkItemID)
				}
			}
		})
	}
}

func recordSelectionFixtureAttempt(t *testing.T, f nativeFixture, r runnerFixture, item tracker.NativeIssue, outcome string, checkpoint *tracker.NativeCheckpoint) string {
	t.Helper()
	now := f.service.config.now()
	attemptID, runID := hubapi.NewID("attempt"), hubapi.NewID("run")
	var lease leasestore.Lease
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		stored, err := w.WorkItem(t.Context(), f.project.ID, item.WorkItemID)
		if err != nil {
			return err
		}
		lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: stored.ID, ExpectedRevision: stored.Revision, MachineID: string(r.binding.MachineID), SessionID: hubapi.NewID("selection-session"), LeaseID: hubapi.NewID("lease"), TTL: time.Hour})
		if err != nil {
			return err
		}
		if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: f.project.OrganizationID, LeaseID: lease.LeaseID, RunnerID: r.binding.RunnerID, AssignedAt: now}); err != nil {
			return err
		}
		return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: hubTestPolicy().ID, BoundAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	var checkpointRef attachment.ArchiveReference
	if checkpoint != nil {
		var err error
		checkpointRef, err = f.service.runDetailStore().StoreDetail(t.Context(), string(f.project.OrganizationID), historyTestJSON(t, checkpoint))
		if err != nil {
			t.Fatal(err)
		}
	}
	for index, kind := range []string{"run.started", "run.finished"} {
		run := tracker.NativeRunData{AttemptID: attemptID, RunID: runID, Sequence: int64(index + 1), LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: hubTestPolicy().ID, MachineID: r.binding.MachineID, RunnerID: r.binding.RunnerID, SessionID: lease.SessionID, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "example", Model: "example-model"}}
		status := "running"
		if index == 1 {
			run.Outcome, run.Handoff, status = outcome, checkpoint, outcome
		}
		ref, err := f.service.runDetailStore().StoreDetail(t.Context(), string(f.project.OrganizationID), historyTestJSON(t, run))
		if err != nil {
			t.Fatal(err)
		}
		event, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvent(t.Context(), f.project.OrganizationID, historyTestJSON(t, tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateID: item.WorkItemID, Type: kind, SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: r.binding.RunnerID}, RecordedAt: now, Data: tracker.CollaborationData{Run: &run}}))
		if err != nil {
			t.Fatal(err)
		}
		record := leasestore.AttemptEvent{ID: attemptID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: runID, Sequence: run.Sequence, Status: status, PayloadObjectKey: ref.Key, PayloadReference: ref}
		if index == 0 {
			record.StartedAt = now
		}
		if index == 1 && checkpoint != nil {
			record.CheckpointObjectKey, record.CheckpointReference, record.CheckpointPreservesSource = new(checkpointRef.Key), checkpointRef, new(false)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.RecordAttempt(t.Context(), record); err != nil {
				return err
			}
			if _, err := w.AppendEvent(t.Context(), event); err != nil {
				return err
			}
			if index == 1 {
				_, err := w.Release(t.Context(), record.Fence, "completed")
				return err
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return attemptID
}

func updateSelectionFixtureBody(t *testing.T, f nativeFixture, item tracker.NativeIssue, body string) tracker.NativeIssue {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(item.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("selection-body")}, ExpectedRevision: item.Revision, Body: &body})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &item)
	return item
}

func setSelectionFixtureState(t *testing.T, f nativeFixture, item tracker.NativeWorkItemID, state string) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(project.States, func(candidate tracker.NativeState) bool { return candidate.Name == state }) {
			project.States = append(project.States, tracker.NativeState{Name: state, Dispatchable: state != "Done", Terminal: state == "Done"})
			if _, err := w.UpdateProjectRecord(t.Context(), project); err != nil {
				return err
			}
		}
		row, err := w.WorkItem(t.Context(), f.project.ID, item)
		if err != nil {
			return err
		}
		row.WorkflowStateName = &state
		if _, err := w.UpdateWorkItem(t.Context(), row); err != nil {
			return err
		}
		queue, err := w.QueueEntry(t.Context(), f.project.ID, item, string(f.project.ID))
		if err != nil {
			return err
		}
		queue.State = state
		return w.PutQueueEntry(t.Context(), queue)
	}); err != nil {
		t.Fatal(err)
	}
}
