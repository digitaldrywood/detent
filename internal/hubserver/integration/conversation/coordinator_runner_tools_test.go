package conversation_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type scriptedToolResult struct {
	value runner.AgentToolResult
	err   error
}

type scriptedToolCall struct {
	call   runner.AgentToolCall
	result chan scriptedToolResult
}

type scriptedToolBackend struct {
	calls chan scriptedToolCall
}

func (b scriptedToolBackend) RunTurn(context.Context, runner.AgentTurnRequest, runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	return runner.AgentTurnResult{}, errors.New("the test expects a turn with tools")
}

func (b scriptedToolBackend) RunTurnWithTools(ctx context.Context, _ runner.AgentTurnRequest, _ []runner.AgentTool, handle runner.AgentToolHandler, _ runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	select {
	case call := <-b.calls:
		value, err := handle(ctx, call.call)
		call.result <- scriptedToolResult{value: value, err: err}
		return runner.AgentTurnResult{}, nil
	case <-ctx.Done():
		return runner.AgentTurnResult{}, ctx.Err()
	}
}

func (b scriptedToolBackend) run(t *testing.T, name string, args map[string]any) scriptedToolResult {
	t.Helper()
	arguments, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	call := scriptedToolCall{call: runner.AgentToolCall{Name: name, Arguments: arguments}, result: make(chan scriptedToolResult, 1)}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	select {
	case b.calls <- call:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case result := <-call.result:
		return result
	case <-ctx.Done():
		t.Fatalf("coordinator did not run tool %s: %v", name, ctx.Err())
		return scriptedToolResult{}
	}
}

type runnerToolCase struct {
	name, role, tool, tier string
	preview, execute       bool
	status                 int
}

func TestCoordinatorRunnerTools(t *testing.T) {
	t.Parallel()
	backend := scriptedToolBackend{calls: make(chan scriptedToolCall)}
	hub := hubfixture.NewHostedSecurity(t, func(cfg *hubfixture.Config) {
		cfg.Content = conversationTestConfig().Content
		cfg.Conversation = &conversations.ConversationConfig{Enabled: true, Backend: backend, Workspace: t.TempDir()}
		for _, plan := range cfg.Hosted.Plans.Plans {
			for _, metric := range []string{"registered_runners", "connected_runners", "members", "projects"} {
				plan.Allowances[metric] = 100
			}
		}
	})
	cases := []runnerToolCase{
		{"read unavailable tier", "member", "get_runners", "", true, false, 0},
		{"organization runner read", "member", "get_runners", "", true, false, 0},
		{"organization runner admin full access", "admin", "set_runner_tier", isolation.NativeTrusted, true, true, http.StatusOK},
		{"admin converts another owner's runner", "admin", "convert_runner_scope", "", true, true, http.StatusOK},
		{"unapproved conversion", "owner", "convert_runner_scope", "", true, false, 0},
		{"member cannot convert", "member", "convert_runner_scope", "", false, false, 0},
		{"admin full access", "admin", "set_runner_tier", isolation.NativeTrusted, true, true, http.StatusOK},
		{"owner sandbox", "owner", "set_runner_tier", isolation.Sandbox, true, true, http.StatusOK},
		{"sprite sandbox requests a fresh probe", "owner", "set_runner_tier", isolation.Sandbox, true, true, http.StatusOK},
		{"sprite sandbox after passing probe", "owner", "set_runner_tier", isolation.Sandbox, true, true, http.StatusOK},
		{"sprite full access", "owner", "set_runner_tier", isolation.NativeTrusted, true, true, http.StatusOK},
		{"sprite detected after preview", "owner", "set_runner_tier", isolation.Sandbox, true, true, http.StatusOK},
		{"unapproved preview", "owner", "set_runner_tier", isolation.NativeTrusted, true, false, 0},
		{"member cannot change", "member", "set_runner_tier", isolation.NativeTrusted, false, false, 0},
		{"invalid tier", "owner", "set_runner_tier", "user", false, false, 0},
		{"foreign runner", "owner", "set_runner_tier", isolation.NativeTrusted, false, false, 0},
		{"foreign read", "member", "get_runners", "", false, false, 0},
		{"stale preview", "owner", "set_runner_tier", isolation.NativeTrusted, true, true, http.StatusConflict},
		{"role revoked after preview", "owner", "set_runner_tier", isolation.NativeTrusted, true, true, http.StatusNotFound},
		{"project grant revoked", "owner", "set_runner_tier", isolation.NativeTrusted, true, true, http.StatusNotFound},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			project := tracker.ProjectID(hubapi.NewID("prj"))
			hub.Project, hub.Base = project, hubfixture.ProjectBase(hub.Primary, project)
			seededProject := hub.SeedProject(t, project, test.name)
			owner := hub.User(t, hubapi.NewID("owner"), "owner", "owner@example.test", "", "")
			hub.Grant(t, owner, project, true, true)
			enrolled := enrollCoordinatorRunner(t, hub, owner, seededProject, test)
			heartbeat := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": enrolled.Redemption.BackendIsolation, "problems": []runnerauth.Problem{}}
			if test.tier != isolation.Sandbox {
				heartbeat["problems"] = []runnerauth.Problem{runnerauth.NewProblem("tier_unavailable")}
			}
			publishHeartbeat := func() {
				t.Helper()
				hubfixture.RequireStatus(t, hub.Hub.Request(t, http.MethodPost, hub.Base+"/machines/"+string(enrolled.Binding.MachineID)+"/heartbeat", enrolled.Redemption.Credential, heartbeat), http.StatusOK)
			}
			publishHeartbeat()
			before := coordinatorRunnerRecord(t, hub, enrolled.Binding.RunnerID)
			user := owner
			if test.role != "owner" {
				user = hub.User(t, hubapi.NewID("caller"), test.role, "caller@example.test", "", "")
				hub.Grant(t, user, project, true, true)
			}
			created := hub.Request(t, user, http.MethodPost, hub.Base+"/conversations", map[string]any{"key": "tier-chat", "first_message": map[string]any{"key": "tier-message", "text": "Why is my runner unavailable?"}})
			hubfixture.RequireStatus(t, created, http.StatusCreated)
			var conversation conversations.ConversationCreatedResponse
			hubfixture.Decode(t, created, &conversation)
			args := map[string]any{}
			if test.tool != "get_runners" {
				args["runner_id"] = enrolled.Binding.RunnerID
			}
			if test.tool == "set_runner_tier" {
				args["isolation_tier"] = test.tier
			}
			if strings.HasPrefix(test.name, "foreign ") {
				args["runner_id"] = "runner_foreign"
			}
			x := backend.run(t, test.tool, args)
			if x.err != nil || x.value.Success != test.preview {
				t.Fatalf("tool %s: %s (success=%t, error=%v)", test.tool, x.value.Content, x.value.Success, x.err)
			}
			if test.tool == "get_runners" && test.preview {
				requireCoordinatorRunnerDiagnostic(t, x.value, enrolled.Binding.RunnerID)
			}
			if test.preview && test.tool != "get_runners" {
				path := hub.Base + "/conversations/" + conversation.Conversation.ID
				action := coordinatorRunnerProposal(t, hub, user, path, test.tool)
				if test.execute {
					changeRunnerAuthority(t, hub, user, enrolled, test.name)
					hubfixture.RequireStatus(t, hub.Request(t, user, http.MethodPost, path+"/actions", action), test.status)
				}
			}
			if test.execute && test.status == http.StatusOK {
				heartbeat["problems"] = []runnerauth.Problem{}
				publishHeartbeat()
			}
			after := coordinatorRunnerRecord(t, hub, enrolled.Binding.RunnerID)
			assertCoordinatorRunnerOutcome(t, before, after, test)
		})
	}
}

func enrollCoordinatorRunner(t *testing.T, hub *hubfixture.Hosted, owner hubfixture.HostedUser, project tracker.NativeProject, test runnerToolCase) hubfixture.Runner {
	t.Helper()
	request := runnerauth.EnrollmentRequest{ProjectIDs: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat}, TTLSeconds: 900}
	if strings.HasPrefix(test.name, "organization runner") {
		request.Scope, request.ProjectIDs = "organization", nil
	}
	orgBase := hubfixture.OrganizationBase(hub.Primary)
	response := hub.Request(t, owner, http.MethodPost, orgBase+"/runner-enrollments", request)
	hubfixture.RequireStatus(t, response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	hubfixture.Decode(t, response, &enrollment)
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	binding := runnerauth.NewBinding()
	redemption := runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "customer-host", DisplayName: "Runner", Capacity: 2, Version: "test", BackendIsolation: isolation.Report{"codex": {isolation.NativeTrusted}}}
	if test.name == "sprite sandbox after passing probe" {
		redemption.BackendIsolation["codex"] = []string{isolation.NativeTrusted, isolation.Sandbox}
	}
	if test.tier == isolation.Sandbox {
		redemption.IsolationTier = isolation.NativeTrusted
	}
	if strings.HasPrefix(test.name, "sprite ") && test.name != "sprite detected after preview" {
		redemption.SpriteName = redemption.Hostname
	}
	r := hubfixture.Runner{Native: hubfixture.Native{Hub: hub.Hub, Project: project, Base: hub.Base}, Binding: binding, Enrollment: enrollment, Redemption: redemption, OrgBase: orgBase}
	r.Enroll(t)
	return r
}

func coordinatorRunnerRecord(t *testing.T, hub *hubfixture.Hosted, id string) runnerstore.FleetRecord {
	t.Helper()
	var record runnerstore.RoutingRecord
	if err := hub.Repository.ReadTenant(t.Context(), hub.Primary, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		record, err = r.(runnerstore.RoutingReader).RunnerRoutingRecord(t.Context(), id, hub.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return record.Record
}

func requireCoordinatorRunnerDiagnostic(t *testing.T, result runner.AgentToolResult, id string) {
	t.Helper()
	var view struct {
		Runners []runnerauth.Runner `json:"runners"`
	}
	if err := json.Unmarshal([]byte(result.Content), &view); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(view.Runners, func(r runnerauth.Runner) bool {
		return r.RunnerID == id && r.IsolationTier == isolation.Sandbox && slices.Contains(r.BackendIsolation["codex"], isolation.NativeTrusted) && slices.ContainsFunc(r.Problems, tierUnavailable)
	}) {
		t.Fatalf("runner %s is missing its unavailable-tier diagnostic: %s", id, result.Content)
	}
}

func tierUnavailable(problem runnerauth.Problem) bool { return problem.Code == "tier_unavailable" }

func coordinatorRunnerProposal(t *testing.T, hub *hubfixture.Hosted, user hubfixture.HostedUser, path, kind string) chat.Action {
	t.Helper()
	response := hub.Request(t, user, http.MethodGet, path, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var snapshot struct {
		Messages []struct {
			Data struct {
				Proposal struct {
					Action chat.Action `json:"action"`
				} `json:"operator_action"`
			} `json:"data"`
		} `json:"messages"`
	}
	hubfixture.Decode(t, response, &snapshot)
	for _, message := range snapshot.Messages {
		action := message.Data.Proposal.Action
		if string(action.Kind) == kind && action.ID != "" {
			return action
		}
	}
	t.Fatalf("conversation did not publish a %s proposal", kind)
	return chat.Action{}
}

func changeRunnerAuthority(t *testing.T, hub *hubfixture.Hosted, user hubfixture.HostedUser, enrolled hubfixture.Runner, scenario string) {
	t.Helper()
	var change func(tenantstore.Writer) error
	ctx := t.Context()
	switch scenario {
	case "sprite detected after preview":
		change = func(w tenantstore.Writer) error {
			machine, err := w.Machine(ctx, string(enrolled.Binding.MachineID))
			if err != nil {
				return err
			}
			machine.SpriteName = machine.Hostname
			return w.RegisterMachine(ctx, machine)
		}
	case "stale preview":
		change = func(w tenantstore.Writer) error {
			row, err := w.RunnerIdentity(ctx, enrolled.Binding.RunnerID)
			if err != nil {
				return err
			}
			return w.UpdateRunnerRouting(ctx, row.Revision, row)
		}
	case "role revoked after preview":
		change = func(w tenantstore.Writer) error {
			member, err := w.AccessMember(ctx, user.Identity.Subject)
			if err != nil {
				return err
			}
			member.Role = "member"
			return w.UpdateAccessMember(ctx, member)
		}
	case "project grant revoked":
		hub.Grant(t, user, hub.Project, false, false)
	}
	if change != nil {
		if err := hub.Repository.WriteTenant(ctx, hub.Primary, corestore.Serving, change); err != nil {
			t.Fatal(err)
		}
	}
}

func assertCoordinatorRunnerOutcome(t *testing.T, before, after runnerstore.FleetRecord, test runnerToolCase) {
	t.Helper()
	wantedTier, wantedScope, wantedProjects := before.Identity.IsolationTier, before.Identity.Scope, before.Identity.ProjectIDs
	if test.execute && test.status == http.StatusOK {
		switch test.tool {
		case "set_runner_tier":
			wantedTier = test.tier
		case "convert_runner_scope":
			wantedScope, wantedProjects = "organization", nil
		}
	}
	identity := after.Identity
	if identity.IsolationTier != wantedTier || identity.Scope != wantedScope || !slices.Equal(identity.ProjectIDs, wantedProjects) {
		t.Fatalf("runner routing became %s %s %v, want %s %s %v", identity.IsolationTier, identity.Scope, identity.ProjectIDs, wantedTier, wantedScope, wantedProjects)
	}
	if identity.RunnerID != before.Identity.RunnerID || identity.MachineID != before.Identity.MachineID || identity.TokenID != before.Identity.TokenID {
		t.Fatal("runner action unexpectedly re-enrolled the runner")
	}
	if test.execute && test.status == http.StatusOK && test.tier == isolation.NativeTrusted && slices.ContainsFunc(after.Problems, tierUnavailable) {
		t.Fatal("runner still reports an unavailable tier after switching to a supported tier")
	}
}
