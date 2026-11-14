//go:build detent_hostedpreview

package hubserver

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/billing"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedBrowserPreview(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedOrganizationFixture(t, true, "org_browser_preview", browserPreviewConfig)
	f.server.Config.Handler = f.sessionRefreshHandler(t, f.server.Config.Handler)
	if os.Getenv("DETENT_HOSTED_BROWSER_SPRITES") != "" {
		f.service.config.SecretKeys = secretTestKeys(t, "1", "1")
		f.service.config.SpritesHTTPClient = &http.Client{Transport: spritesTestTransport(func(request *http.Request) (*http.Response, error) {
			if request.Header.Get("Authorization") != "Bearer "+spritesSecretSentinel {
				return spritesTestResponse("", http.StatusUnauthorized), nil
			}
			return spritesTestResponse(`{"sprites":[]}`, http.StatusOK), nil
		})}
	}
	f.seedPreview(t)
	if os.Getenv("DETENT_HOSTED_BROWSER_BOARD_DEFAULTS") != "" {
		f.seedBoardDefaults(t)
	}
	if os.Getenv("DETENT_HOSTED_BROWSER_GITHUB_TRIAGE") != "" {
		f.seedGitHubTriage(t)
	}
	if os.Getenv("DETENT_HOSTED_BROWSER_WORKFLOW_REVISIONS") != "" {
		f.seedWorkflowRevisions(t, f.project)
		f.seedWorkflowRevisions(t, f.privateProject)
	}
	if os.Getenv("DETENT_HOSTED_BROWSER_ISSUE_ASK") != "" {
		f.seedIssueAsk(t)
	}
	if os.Getenv("DETENT_HOSTED_BROWSER_CHAT_ACTIONS") != "" {
		f.seedCoordinatorActions(t)
	}
	if os.Getenv("DETENT_HOSTED_BROWSER_CHAT_WORKFLOW") != "" {
		f.seedCoordinatorWorkflow(t)
	}
	if os.Getenv("DETENT_HOSTED_BROWSER_CAPACITY") != "" {
		plans := hostedplans.CapacityPlans()
		if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&hostedplans.HostedConfig{Plans: &plans}))); err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
			assignment, err := w.BillingAssignment(t.Context())
			if err != nil {
				return err
			}
			assignment.Base = billingstore.PlanReference{ID: "starter", Version: 1}
			return w.SetBillingAssignment(t.Context(), assignment, assignment.Revision)
		}); err != nil {
			t.Fatal(err)
		}
		provider := &hostedBillingProvider{snapshot: billing.Snapshot{Status: "free"}}
		cfg := f.service.config.Hosted
		cfg.Billing = &hostedplans.HostedBillingConfig{Mode: billing.ModeTest, AccountID: "acct_fixture", CustomerID: "cus_fixture", PortalConfigurationID: "bpc_fixture", WebhookSecret: []byte("whsec_fixture_browser_capacity"), GraceSeconds: 3600, ReconcileSeconds: 60, Provider: provider}
		for _, id := range []string{"starter", "growth", "scale"} {
			cfg.Billing.Prices = append(cfg.Billing.Prices, hostedplans.HostedBillingPrice{PriceID: "price_" + id + "_test", Label: id, Plan: hostedplans.PlanReference{ID: id, Version: 1}})
		}
		if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigureBilling(t.Context(), hostedplans.PlansInput(cfg))); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		close(done)
		f.service.billing = &hostedBillingWorker{service: f.service, cancel: func() {}, done: done}
	}
	if os.Getenv("DETENT_HOSTED_BROWSER_AI_CREDITS") != "" {
		plans := hostedTestPlans(t, f.service, map[string]int64{"projects": 2})
		base := &hostedBillingProvider{snapshot: billing.Snapshot{Status: "free"}}
		cfg := f.service.config.Hosted
		cfg.Plans = &plans
		cfg.Billing = &hostedplans.HostedBillingConfig{AccountID: "acct_fixture", CustomerID: "cus_fixture", PortalConfigurationID: "bpc_fixture", WebhookSecret: []byte("whsec_fixture_credits_browser"), GraceSeconds: 3600, ReconcileSeconds: 60, Provider: base, Prices: []hostedplans.HostedBillingPrice{{PriceID: "price_fixture", Label: "Extended pilot", Plan: plans.Plans[1].PlanReference}}}
		configureTestCredits(t, f, base, billingstore.CreditAccount{PaymentMethod: "pm_credit", Failure: "automatic payment failed; update the saved payment method"})
		postAICreditFixture(t, f.service.database, billingstore.CreditTransaction{Mode: billing.ModeTest, Source: "fixture-purchase", AmountMicros: 1250000, Kind: "purchase", RecordedAt: f.service.config.now()})
		done := make(chan struct{})
		close(done)
		f.service.billing = &hostedBillingWorker{service: f.service, cancel: func() {}, done: done}
	}

	if os.Getenv("DETENT_HOSTED_BROWSER_CHAT_USAGE") != "" {
		err := f.service.database.RecordConversationUsage(t.Context(), ConversationUsage{OrganizationID: "org_browser_preview", ProjectID: tracker.ProjectID(f.project), ConversationID: f.conversation, TurnID: "preview-chat-usage", Provider: "openai", Model: "gpt-6-luna", Tokens: runner.AgentTokenCounts{InputTokens: 1_000_000, CachedInputTokens: 400_000, OutputTokens: 100_000, ReasoningOutputTokens: 30_000}})
		if err != nil {
			t.Fatal(err)
		}
	}
	var problemRunner runnerauth.Binding
	var problemCredential string
	type capacityRunner struct {
		runnerauth.Binding
		Credential string `json:"credential"`
	}
	var capacityRunners []capacityRunner
	if os.Getenv("DETENT_HOSTED_BROWSER_RUNNER") != "" {
		base := browserHostedOrganizationBase
		names := []string{"Settings runner"}
		if os.Getenv("DETENT_HOSTED_BROWSER_RUNNER_CAPACITY") != "" {
			names = append(names, "Second settings runner")
		}
		if os.Getenv("DETENT_HOSTED_BROWSER_RUNNER_PROBLEMS") != "" {
			names = append(names, "Healthy runner")
		}
		for _, name := range names {
			binding := runnerauth.NewBinding()
			request := runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{tracker.ProjectID(f.project), tracker.ProjectID(f.privateProject)}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat}, TTLSeconds: 900}
			response := f.api(t, "owner", http.MethodPost, base+"/runner-enrollments", request, http.StatusCreated)
			var enrollment runnerauth.Enrollment
			decodeHubResponse(t, response, &enrollment)
			credential, err := apikey.GenerateToken()
			if err != nil {
				t.Fatal(err)
			}
			redemption := runnerauth.Redemption{BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: "test-host", DisplayName: name, Capacity: 2, Version: "test", OS: "linux", Architecture: "arm64"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/runner-enrollments/redeem", enrollment.Token, redemption), http.StatusCreated)
			if os.Getenv("DETENT_HOSTED_BROWSER_RUNNER_CAPACITY") != "" {
				capacityRunners = append(capacityRunners, capacityRunner{Binding: binding, Credential: credential})
				stored := f.api(t, "owner", http.MethodGet, base+"/runners/"+binding.RunnerID+"/routing", nil, http.StatusOK)
				var routing runnerauth.Runner
				decodeHubResponse(t, stored, &routing)
				f.api(t, "owner", http.MethodPut, base+"/machines/"+string(binding.MachineID)+"/routing", runnerauth.HostChange{ExpectedRevision: routing.HostRevision, DisplayName: name, Capacity: 8}, http.StatusOK)
				provider := capacityReport(f.service.config.now())
				provider.MaxConcurrent = 8
				heartbeat := map[string]any{"display_name": name, "capacity": 2, "version": "test", "backend_isolation": redemption.BackendIsolation, "provider_reports": []any{provider}, "capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: f.service.config.now()}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/projects/"+f.project+"/machines/"+string(binding.MachineID)+"/heartbeat", credential, heartbeat), http.StatusOK)
			}
			if name == "Settings runner" && os.Getenv("DETENT_HOSTED_BROWSER_RUNNER_PROBLEMS") != "" {
				problemRunner, problemCredential = binding, credential
				problem := runnerauth.NewProblem("backend_missing")
				problem.Subject = "codex"
				problem.Message = "not signed in"
				problem.FixHint = "Sign in on this runner, then wait for its next heartbeat."
				problem.Check = "codex login status"
				problem.ErrorOutput = "exit status 1: Not logged in"
				problem.FixCommand = "codex login --device-auth"
				heartbeat := map[string]any{"display_name": name, "capacity": 2, "version": "test", "backend_isolation": redemption.BackendIsolation, "problems": []runnerauth.Problem{problem}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/projects/"+f.project+"/machines/"+string(binding.MachineID)+"/heartbeat", credential, heartbeat), http.StatusOK)
			}
		}
	}
	var restartURL string
	if os.Getenv("DETENT_HOSTED_BROWSER_RESTART") != "" {
		controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if r.URL.Path == "/block" {
				backend := f.service.conversations.Config.Backend.(*fakeCoordinatorBackend)
				backend.setRun(blockingRun(make(chan struct{}), "Working before restart"))
				w.WriteHeader(http.StatusNoContent)
				return
			}
			cfg := f.service.config
			handler := f.server.Config.Handler
			address := f.server.Listener.Addr().String()
			ctx := testenv.WaitContext(t, r.Context())
			if err := f.shutdownForRestart(ctx); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			cfg.Conversation.Backend = newFakeCoordinatorBackend()
			cfg.TenantRepository, cfg.AccessRepository, cfg.BillingRepository = nil, nil, nil
			if owner, ok := cfg.NativeRunEvents.(tenantNativeRunEventParticipant); ok && owner.service == f.service {
				cfg.NativeRunEvents = nil
			}
			if owner, ok := cfg.ConversationDispatch.(tenantNativeRunEventParticipant); ok && owner.service == f.service {
				cfg.ConversationDispatch = nil
			}
			if owner, ok := cfg.ProjectParticipants.(tenantProjectParticipants); ok && owner.service == f.service {
				cfg.ProjectParticipants = nil
			}
			service, err := Open(t.Context(), cfg)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			f.service = service
			var lc net.ListenConfig
			listener, err := lc.Listen(r.Context(), "tcp", address)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			server := httptest.NewUnstartedServer(handler)
			_ = server.Listener.Close()
			server.Listener = listener
			f.server = server
			server.Start()
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(controller.Close)
		restartURL = controller.URL
	}
	accounts := make(map[string]string, len(f.cookies))
	for account := range f.cookies {
		accounts[account] = f.server.URL + "/__preview/account/" + account
	}
	fixture := struct {
		URL               string             `json:"url"`
		Login             string             `json:"login"`
		Organization      string             `json:"organization"`
		Project           string             `json:"project"`
		PrivateProject    string             `json:"private_project"`
		Chat              string             `json:"chat"`
		ProjectID         string             `json:"project_id"`
		Conversation      string             `json:"conversation"`
		WorkItem          string             `json:"work_item"`
		WorkerChat        string             `json:"worker_conversation"`
		WorkerItem        string             `json:"worker_work_item"`
		OwnerEmail        string             `json:"owner_email"`
		Accounts          map[string]string  `json:"accounts"`
		Stop              string             `json:"stop"`
		Restart           string             `json:"restart,omitempty"`
		Expires           time.Time          `json:"expires"`
		ProblemRunner     runnerauth.Binding `json:"problem_runner"`
		ProblemCredential string             `json:"problem_credential,omitempty"`
		CapacityRunners   []capacityRunner   `json:"capacity_runners,omitempty"`
		PolicyID          string             `json:"policy_id"`
	}{
		URL: f.server.URL, Login: f.server.URL + "/login", Organization: f.server.URL + "/organization",
		Project: f.server.URL + "/projects/" + f.project, PrivateProject: f.server.URL + "/projects/" + f.privateProject,
		Chat: f.server.URL + "/chat", ProjectID: f.project, Conversation: f.conversation, WorkItem: f.workItem,
		WorkerChat: f.workerChat, WorkerItem: f.workerItem,
		OwnerEmail: browserHostedOwnerEmail, Accounts: accounts, Stop: f.server.URL + "/__preview/stop", Expires: time.Now().Add(browserHostedPreviewLifetime),
		ProblemRunner: problemRunner, ProblemCredential: problemCredential, Restart: restartURL,
		CapacityRunners: capacityRunners, PolicyID: hubTestPolicy().ID,
	}
	encoded, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hosted-browser-preview.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("Hosted browser fixture: %s", path)
	t.Logf("Hosted browser URL: %s", f.server.URL)
	timer := time.NewTimer(browserHostedPreviewLifetime)
	defer timer.Stop()
	select {
	case <-f.stop:
	case <-timer.C:
	case <-t.Context().Done():
	}
}

func (f *browserHostedFixture) seedBoardDefaults(t *testing.T) {
	t.Helper()
	now := f.service.config.now()
	states := []tracker.NativeState{
		{Name: "Backlog", Transitions: []string{"Todo"}},
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}},
		{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress"}},
		{Name: "Merging", Transitions: []string{"Done"}},
		{Name: "Blocked", Transitions: []string{"Todo"}},
		{Name: "Human Review", Transitions: []string{"Todo"}},
		{Name: "Triage", Transitions: []string{"Todo"}},
		{Name: "Done", Terminal: true},
		{Name: "Cancelled", Terminal: true},
	}
	for projectIndex, project := range []string{f.project, f.privateProject} {
		scope := hubscope.NativeScope{Organization: tracker.OrganizationID(f.service.config.Hosted.OrganizationID), Project: tracker.ProjectID(project)}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			project, err := w.ProjectRecord(t.Context(), scope.Project)
			if err != nil {
				return err
			}
			project.States = hubrecords.NativeTriageStates(states)
			_, err = w.UpdateProjectRecord(t.Context(), project)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		backlog := seedArchiveIssues(t, f.service, scope, 130, "Backlog")
		var number int64
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			number, err = r.NextWorkItemNumber(t.Context(), scope.Project)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var issues []tracker.NativeIssue
		var histories [][]tracker.CollaborationEvent
		for index, seed := range []struct {
			state string
			age   time.Duration
		}{
			{"Done", 30 * time.Minute}, {"Done", 5 * time.Hour}, {"Done", 60 * time.Hour}, {"Cancelled", 30 * time.Minute},
		} {
			at := now.Add(-seed.age)
			issue := backlog[0]
			issue.WorkItemID, issue.Number = tracker.NativeWorkItemID(hubapi.NewID("wi")), int(number)+index
			issue.State, issue.Terminal, issue.ClosedAt = seed.state, true, &at
			issue.CreatedAt, issue.UpdatedAt, issue.LastActivityAt = now.Add(-72*time.Hour), now, now
			issues = append(issues, issue)
			histories = append(histories, []tracker.CollaborationEvent{{Type: "workflow.transitioned", RecordedAt: at, Data: tracker.CollaborationData{Revision: issue.Revision, FromState: "Todo", ToState: seed.state}}})
		}
		importNativeWorkPageIssues(t, nativeFixture{service: f.service, project: tracker.NativeProject{OrganizationID: scope.Organization, ID: scope.Project}}, issues, histories, int64(2000+1000*projectIndex))
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			for _, issue := range issues {
				row, err := r.WorkItem(t.Context(), scope.Project, issue.WorkItemID)
				if err != nil {
					return err
				}
				if row.TerminalEnteredAt == nil || !row.TerminalEnteredAt.Equal(*issue.ClosedAt) {
					t.Fatalf("historical browser closure=%v want=%v", row.TerminalEnteredAt, issue.ClosedAt)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	drainTestNativeBoard(t, f.service)
}

func (f *browserHostedFixture) seedGitHubTriage(t *testing.T) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), tracker.ProjectID(f.project))
		if err != nil {
			return err
		}
		project.Settings.CheckoutRepository = "acme/orders"
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := linkedSnapshot()
	snapshot.Title = "Public GitHub report awaiting triage"
	scope := hubscope.NativeScope{Organization: tracker.OrganizationID(f.service.config.Hosted.OrganizationID), Project: tracker.ProjectID(f.project)}
	applyLinkedWebhook(t, nativeFixture{service: f.service, project: tracker.NativeProject{ID: scope.Project, OrganizationID: scope.Organization}}, "opened", "issues", snapshot, nil)
	drainTestNativeBoard(t, f.service)
}

func (f *browserHostedFixture) seedWorkflowRevisions(t *testing.T, project string) {
	t.Helper()
	base := browserHostedOrganizationBase + "/projects/" + project
	coordinatorFixtureProject(t, f.service, tracker.OrganizationID(strings.TrimPrefix(browserHostedOrganizationBase, "/api/v2/organizations/")), tracker.ProjectID(project), func(row *projectstore.Record) {
		row.Settings.CheckoutRepository = "example-org/example-project"
	})
	binding := runnerauth.NewBinding()
	request := runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{tracker.ProjectID(project)}, Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, TTLSeconds: 900}
	var enrollment runnerauth.Enrollment
	decodeHubResponse(t, f.api(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/runner-enrollments", request, http.StatusCreated), &enrollment)
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	redemption := runnerauth.Redemption{BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: "test-host", DisplayName: "Workflow runner", Capacity: 2, Version: "test", OS: "linux", Architecture: "arm64"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, browserHostedOrganizationBase+"/runner-enrollments/redeem", enrollment.Token, redemption), http.StatusCreated)
	original := resolvedWorkflowPolicy(t, append(nativeFixtureStates(), policy.State{Name: "Archive", Terminal: true}), strings.Repeat("a", 40), false)
	clearNativeFixturePolicy(t, nativeFixture{
		service: f.service,
		project: tracker.NativeProject{
			OrganizationID: tracker.OrganizationID(strings.TrimPrefix(browserHostedOrganizationBase, "/api/v2/organizations/")),
			ID:             tracker.ProjectID(project),
		},
	})
	f.api(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{Policy: original}, http.StatusOK)
	applied := resolvedWorkflowPolicy(t, []policy.State{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Review"}},
		{Name: "Review", Transitions: []string{"Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Review"}},
		{Name: "Done", Terminal: true},
	}, strings.Repeat("b", 40), true)
	source := &policy.RepositorySource{Repository: "example-org/example-project", Commit: applied.Workflow.Revision, DefaultBranch: "develop", DefaultBranchHead: original.Workflow.Revision, DefaultBranchReachable: false}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/policy/observed", credential, policy.Observation{Descriptor: applied, Source: source}), http.StatusNoContent)
	f.api(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{ExpectedID: original.ID, Policy: applied}, http.StatusOK)
	pending := resolvedWorkflowPolicy(t, []policy.State{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Review"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}},
		{Name: "Review", Dispatchable: true, Transitions: []string{"Done"}},
		{Name: "Done", Terminal: true},
	}, strings.Repeat("c", 40), false)
	pendingSource := *source
	pendingSource.DefaultBranchHead = applied.Workflow.Revision
	pendingSource.Commit, pendingSource.DefaultBranchReachable = pending.Workflow.Revision, project == f.privateProject
	client, err := hubclient.New(hubclient.Config{URL: "https://workflow-browser.example.test", TokenSource: func() string { return credential }, HTTPClient: &http.Client{Transport: policyAPITransport{service: f.service}}})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := hubclient.NewScheduler(client, hubclient.SchedulerConfig{
		OrganizationID: tracker.OrganizationID(strings.TrimPrefix(browserHostedOrganizationBase, "/api/v2/organizations/")), NativeProjects: map[string]tracker.ProjectID{"workflow": tracker.ProjectID(project)},
		Machine: hubclient.Machine{ID: binding.MachineID, Hostname: "workflow-host", Capacity: 2, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	local, err := workflowconfig.ApplyNativePolicy(workflowconfig.Workflow{Config: workflowconfig.Default()}, pending)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := scheduler.ResolveProjectWorkflow(t.Context(), "workflow", local, &pendingSource)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := workflowconfig.ResolvePolicy(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.CheckProjectPolicyWithSource(t.Context(), "workflow", "example-org/example-project", descriptor, &pendingSource); err != nil {
		t.Fatal(err)
	}
}
