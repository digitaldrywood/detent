package hubserver

import (
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// The hosted account surface half of the shared contract fixtures: the
// extended bootstrap and the organization, project, fleet, plan and billing
// payloads of docs/conversation/decisions.md section 12. Each case binds the
// Go projection to the fixture the client validates against, so a renamed or
// dropped field fails here rather than in the browser.

var (
	accountFixtureWindowEnd = time.Date(2026, 9, 10, 13, 0, 0, 0, time.UTC)
	accountFixtureStamp     = time.Date(2026, 9, 10, 12, 4, 31, 0, time.UTC)
)

// accountFixtureStates builds one state per transition count so the fixture
// comparison, which requires equal array lengths, lines up.
func accountFixtureStates(transitions ...int) []tracker.NativeState {
	states := make([]tracker.NativeState, 0, len(transitions))
	for _, count := range transitions {
		state := tracker.NativeState{Name: "Todo", Dispatchable: true, Transitions: []string{}}
		for range count {
			state.Transitions = append(state.Transitions, "Done")
		}
		states = append(states, state)
	}
	return states
}

func accountFixtureSteps() []onboarding.Step {
	steps := make([]onboarding.Step, 0, 4)
	for range 4 {
		steps = append(steps, onboarding.Step{Name: "Repository configuration", State: "ready", Detail: "Approve the resolved policy descriptor."})
	}
	return steps
}

func accountFixtureProject(id string, transitions ...int) appBootstrapProject {
	return appBootstrapProject{ID: id, Name: id, Profile: "native", CanWrite: true, CanManageRunners: true, States: accountFixtureStates(transitions...)}
}

func accountFixtureBootstrap() appBootstrap {
	return appBootstrap{
		Organization: appBootstrapOrganization{ID: "org_threefold", Name: "Threefold Solutions", PublicURL: "https://threefold.detent.cloud", Current: true},
		Organizations: []appBootstrapOrganization{
			{ID: "org_threefold", Name: "Threefold Solutions", PublicURL: "https://threefold.detent.cloud", Current: true},
			{ID: "org_parable", Name: "Parable", PublicURL: "https://parable.detent.cloud"},
		},
		Actor:        appBootstrapActor{PrincipalID: "tok_01J9", Subject: "user_michael", Email: "michael@threefold.solutions", Role: "owner", CanManage: true, CanManageRunners: true},
		Projects:     []appBootstrapProject{accountFixtureProject("proj_parable", 1, 1, 0), accountFixtureProject("proj_marketing", 1, 0)},
		CSRFToken:    "csrf_01J9QX",
		Capabilities: appBootstrapCapabilities{Coordinator: true},
		Feature:      appBootstrapFeature{Conversation: true},
		Plan:         &appBootstrapPlan{ID: "pilot_free", Name: "pilot_free · version 1", Source: "base", WindowEndsAt: accountFixtureWindowEnd.Format(time.RFC3339)},
		APIBase:      "/api/v2/organizations/org_threefold",
		Version:      "v0.108.0",
	}
}

func accountFixtureMember(id, user, role string, grants int) hostedMemberView {
	view := hostedMemberView{ID: id, UserID: user, Email: user + "@example.test", Role: role, Status: "active", Grants: []hostedMemberGrant{}}
	for range grants {
		view.Grants = append(view.Grants, hostedMemberGrant{ProjectID: "proj_parable", Write: true, Runner: true})
	}
	return view
}

func accountFixtureInvitation() hostedInvitationView {
	return hostedInvitationView{ID: "inv_01J9", Email: "rae@example.test", Role: "member", CreatedAt: "2026-09-09T14:02:11Z", ExpiresAt: "2026-09-10T14:02:11Z"}
}

func accountFixtureEntitlement(names []string, features, grants int) HostedEntitlement {
	entitlement := HostedEntitlement{
		OrganizationID: "org_threefold", Base: PlanReference{ID: "pilot_free", Version: 1}, EffectiveBase: PlanReference{ID: "pilot_free", Version: 1},
		Source: "base", Revision: 4, Features: []string{}, Allowances: map[string]int64{}, Usage: map[string]int64{},
		Grants: []HostedGrant{}, WindowEndsAt: accountFixtureWindowEnd,
	}
	for i := range features {
		entitlement.Features = append(entitlement.Features, hostedFeatureNames()[i])
	}
	for _, name := range names {
		entitlement.Allowances[name] = 10
		entitlement.Usage[name] = 1
	}
	expires := accountFixtureWindowEnd
	for range grants {
		entitlement.Grants = append(entitlement.Grants, HostedGrant{ID: "grant_pilot", Plan: PlanReference{ID: "pilot_plus", Version: 2}, Scope: []string{"hosted_artifacts"}, StartsAt: accountFixtureStamp, ExpiresAt: &expires})
	}
	return entitlement
}

func accountFixtureFleetRunner(providers, leases int) hostedFleetRunner {
	runner := hostedFleetRunner{
		ID: "rnr_macbook", DisplayName: "Michael's MacBook Pro", Hostname: "michaels-macbook-pro.local", Health: "healthy",
		State: "enabled", OS: "darwin", Architecture: "arm64", Version: "v0.9.0", HostCapacity: 2, HostUsed: 1, CapacityLimit: 2, ReportedCapacity: 2,
		ProviderCapacity: []providercapacity.View{}, LastHeartbeatAt: accountFixtureStamp, Leases: []hostedFleetLease{},
	}
	reset := accountFixtureStamp
	for i := range providers {
		view := providercapacity.View{
			Report: providercapacity.Report{
				Provider: "codex", Backend: "codex", AccountAlias: "michael@threefold.solutions",
				MaxConcurrent: 2, Availability: "available", ObservedAt: accountFixtureStamp, Models: []string{"gpt-6-astra", "gpt-5.6-sol"},
			},
			Used: 1, State: "ready",
		}
		if i == 1 {
			view.ResetAt = reset
			view.Reason = "Five hour window is spent"
		}
		runner.ProviderCapacity = append(runner.ProviderCapacity, view)
	}
	for range leases {
		runner.Leases = append(runner.Leases, hostedFleetLease{LeaseID: "lease_7c1", WorkItemID: "wi_2093", Title: "connections", ProjectID: "proj_parable", ExpiresAt: accountFixtureStamp})
	}
	return runner
}

func accountFixtureUsage(names ...string) hostedFleetUsage {
	usage := hostedFleetUsage{WindowEndsAt: accountFixtureWindowEnd.Format(time.RFC3339), Allowances: map[string]hostedFleetAllowance{}}
	for _, name := range names {
		usage.Allowances[name] = hostedFleetAllowance{Used: 1, Limit: 10}
	}
	return usage
}

func accountFixtureSpend(projects, points int) *hostedSpend {
	spend := &hostedSpend{Today: 12.4, Window: 118.12, Currency: "USD", ByProject: []hostedProjectSpend{}, Series: []hostedSpendPoint{}}
	for range projects {
		spend.ByProject = append(spend.ByProject, hostedProjectSpend{ProjectID: "proj_parable", Amount: 58.31})
	}
	for range points {
		spend.Series = append(spend.Series, hostedSpendPoint{At: accountFixtureStamp.Format(time.RFC3339), Amount: 4.2})
	}
	return spend
}

func accountFixtureOnboarding() onboarding.Project {
	runner := runnerauth.Runner{
		Binding:  runnerauth.Binding{RunnerID: "rnr_macbook", MachineID: "mac_9df1"},
		Routing:  runnerauth.Routing{DisplayName: "Michael's MacBook Pro", Tags: []string{"detent:macbook"}, State: "enabled", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{"proj_parable"}},
		Revision: 4, Hostname: "michaels-macbook-pro.local", Health: "healthy", OS: "darwin", Architecture: "arm64",
		LastHeartbeatAt: accountFixtureStamp, Operations: []string{runnerauth.Claim},
	}
	approval := accountFixturePolicy()
	return onboarding.Project{
		LatestRun: "succeeded",
		Progress:  onboarding.Progress{Revision: 4, Repository: "existing", Doctor: true, Provider: true, Artifacts: "local", UpdatedAt: accountFixtureStamp.Format(time.RFC3339)},
		Policy:    &approval,
		Runners: []runnerauth.Eligibility{
			{Runner: runner, Exclusions: []runnerauth.Exclusion{}},
			{Runner: runner, Exclusions: []runnerauth.Exclusion{{Code: "runner_offline", Message: "No recent heartbeat"}}},
		},
		Artifacts: []artifact.Binding{},
		Steps:     accountFixtureSteps(),
		Ready:     true,
	}
}

// accountFixtureAuthority is the authority map readProjectIntegration fills.
func accountFixtureAuthority() map[string]string {
	authority := map[string]string{}
	for _, field := range []string{
		"title", "body", "discussion", "dependencies", "authors", "workflow", "labels", "assignees", "priority",
		"source_timestamps", "scheduling", "progress", "repository_policy", "github_merge", "native_approval",
	} {
		authority[field] = "detent"
	}
	return authority
}

func accountFixturePolicy() policy.Approval {
	return policy.Approval{
		Policy: policy.Descriptor{
			Schema: 1, ID: "pol_9f2c4a", SourceRevision: "3f8f74e0", SourceDigest: "sha256:6ac1f0b7d2", ConfigDigest: "sha256:1d90ae44c7",
			Profile:      "native",
			Requirements: policy.Requirements{RequiredTags: []string{"detent:mac-mini-1"}},
			Gates: policy.Gates{
				Kind: "pull_request", AutomatedReview: "required", RequiredChecks: 2, Validator: true, MergeMethod: "squash",
			},
		},
		ApprovedBy: "michael@threefold.solutions", ApprovedAt: accountFixtureStamp.Format(time.RFC3339),
	}
}

// hostedAccountFixtureCases binds the account fixtures to their Go payloads.
func hostedAccountFixtureCases() map[string]conversationFixtureCase {
	projects := []hostedProjectView{}
	for _, transitions := range [][]int{{1, 1, 0}, {1, 0}} {
		view := hostedProjectView{appBootstrapProject: accountFixtureProject("proj_parable", transitions...)}
		view.Onboarding.Ready, view.Onboarding.Steps = true, accountFixtureSteps()
		projects = append(projects, view)
	}
	support := appBootstrapSupport{Actor: "support@detent.dev", Reason: "Investigating a stuck lease", ExpiresAt: accountFixtureWindowEnd.Format(time.RFC3339)}
	supportBootstrap := accountFixtureBootstrap()
	supportBootstrap.Support, supportBootstrap.Plan, supportBootstrap.Version = &support, nil, ""
	supportBootstrap.Projects = []appBootstrapProject{}
	supportBootstrap.Organizations = supportBootstrap.Organizations[:1]
	return map[string]conversationFixtureCase{
		"account-bootstrap.json": {
			value: accountFixtureBootstrap(),
			extra: append([]string{"organization"}, conversationSection14Fields("")...),
			// The conversation client's own per-project fields are not part
			// of the hosted bootstrap.
			optional: []string{"projects[].labels", "projects[].priorities", "projects[].coordinator"},
		},
		"account-bootstrap-support.json": {
			value:    supportBootstrap,
			extra:    append([]string{"organization"}, conversationSection14Fields("")...),
			optional: []string{"projects[].labels", "projects[].priorities", "projects[].coordinator", "version"},
		},
		"account-members.json": {value: hostedMembersResponse{
			Members: []hostedMemberView{
				accountFixtureMember("mem_owner", "user_michael", "owner", 1),
				accountFixtureMember("mem_admin", "user_dana", "admin", 2),
				accountFixtureMember("mem_viewer", "user_sam", "viewer", 1),
			},
			Invitations: []hostedInvitationView{accountFixtureInvitation()},
		}},
		"account-member.json":     {value: accountFixtureMember("mem_admin", "user_dana", "member", 1)},
		"account-invitation.json": {value: accountFixtureInvitation()},
		"account-next.json": {value: struct {
			Next string `json:"next"`
		}{"https://parable.detent.cloud/auth/oidc/start"}},
		"account-support.json": {value: struct {
			Support appBootstrapSupport `json:"support"`
		}{support}},
		"account-organization-created.json": {
			value: struct {
				Organization appBootstrapOrganization `json:"organization"`
				Next         string                   `json:"next"`
			}{appBootstrapOrganization{ID: "org_newco", Name: "Newco", PublicURL: "https://newco.detent.cloud", Current: true}, "/auth/oidc/start"},
			extra: []string{"organization"},
		},
		"account-projects.json": {value: projects},
		"account-fleet.json": {value: hostedFleetResponse{
			Runners: []hostedFleetRunner{accountFixtureFleetRunner(2, 1), accountFixtureFleetRunner(0, 0)},
			Usage:   accountFixtureUsage("members", "projects", "connected_runners", "concurrent_work", "api_mutations", "ingested_events"),
			Spend:   accountFixtureSpend(2, 7),
			Current: "v0.9.1",
		}},
		"account-fleet-empty.json": {value: hostedFleetResponse{Runners: []hostedFleetRunner{}, Usage: accountFixtureUsage("members", "projects", "connected_runners"), Current: "v0.9.1"}},
		// GET /app/updates: what the sidebar footer's pill polls.
		"account-updates.json": {value: appUpdates{
			Current: "v0.9.1",
			Source:  "hub",
			Runners: []appUpdateRunner{
				{RunnerID: "rnr_macbook", DisplayName: "Michael's MacBook Pro", Version: "v0.9.0", Online: true, Behind: true},
				{RunnerID: "rnr_studio", DisplayName: "Studio", Version: "v0.9.1", Online: true, Behind: false},
			},
			BehindCount: 1,
			Client:      appClientBuild{Version: "v0.9.1", Build: "2f1c9e6a4b8d0f3c5a7e9b1d3f5a7c9e1b3d5f7a9c1e3b5d7f9a1c3e5b7d9f01", ServedAt: "2026-09-12T09:00:00Z"},
		}},
		"account-plan.json": {value: accountFixtureEntitlement(hostedAllowanceNames()[:10], 3, 1)},
		"account-billing.json": {
			value: hostedBillingReport{
				OrganizationID: "org_threefold",
				State:          hostedBillingState{Status: "active", Plan: PlanReference{ID: "pilot_team", Version: 2}},
				Entitlement:    accountFixtureEntitlement([]string{"members", "projects", "concurrent_work"}, 3, 0),
				ReconciledAt:   "2026-09-10T11:58:00Z",
				CanCheckout:    true,
				Prices:         []hostedBillingPriceView{{ID: "price_team_monthly", Label: "Team"}, {ID: "price_team_yearly", Label: "Team"}},
				Audit:          []HostedBillingAudit{{Actor: "michael@threefold.solutions", Action: "checkout_started", Summary: "active", At: "2026-09-01T00:00:00Z"}},
			},
			// The hub also reports the operator-facing status line the
			// removed Templ page rendered; the client renders its own.
			extra: []string{"", "state", "state.subscription"},
		},
		"account-checkout.json": {value: hostedBillingLocation{URL: "https://checkout.stripe.com/c/pay/cs_test"}},
		"account-integration.json": {value: ProjectIntegration{
			Profile: "native", Revision: 7, Intake: "manual", Projection: "summary", RepositoryEnabled: true,
			Repository: "getparable/parable", Authority: accountFixtureAuthority(),
		}},
		"account-policy.json": {
			value: accountFixturePolicy(),
			// policy.Requirements omits the two selectors it has no value for.
			optional: []string{"policy.requirements.runner_id", "policy.requirements.machine_id"},
		},
		"account-onboarding.json": {
			value:    accountFixtureOnboarding(),
			extra:    []string{"runners[].runner"},
			optional: []string{"policy.policy.requirements.runner_id", "policy.policy.requirements.machine_id"},
		},
		"account-onboarding-progress.json": {value: onboarding.Progress{Revision: 4, Repository: "existing", Doctor: true, Provider: true, Artifacts: "local", UpdatedAt: accountFixtureStamp.Format(time.RFC3339)}},
		"account-runner-enrollment.json":   {value: runnerauth.Enrollment{ID: "enr_01J9QX", Token: "det_enroll", ExpiresAt: accountFixtureStamp}},
	}
}
