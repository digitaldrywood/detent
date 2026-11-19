package conversation_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type projectBudgetReply struct {
	Revision tracker.Revision     `json:"revision,string"`
	Policy   budget.MonthlyPolicy `json:"policy"`
}

func TestCoordinatorProjectBudgetPostgres(t *testing.T) {
	t.Parallel()
	backend := scriptedToolBackend{calls: make(chan scriptedToolCall)}
	first := hubfixture.NewHostedSecurity(t, func(cfg *hubfixture.Config) {
		cfg.Content = conversationTestConfig().Content
		cfg.Conversation = &conversations.ConversationConfig{Enabled: true, Backend: backend, Workspace: t.TempDir()}
	})
	second := hubfixture.OpenConversationHostedPeer(t, first, "org_budget_peer")
	second.Project = "prj_budget_peer"
	second.Base = hubfixture.ProjectBase(second.Primary, second.Project)
	second.SeedProject(t, second.Project, "Budget peer")
	for _, hub := range []*hubfixture.Hosted{first, second} {
		seedProjectBudget(t, hub, "", 9000)
	}
	owner := budgetActor(t, first, "owner", false)
	t.Run("absent project budget does not inherit organization", func(t *testing.T) {
		result := readBudgetTool(t, first, owner, backend)
		if result.Revision != 0 || result.Policy.Enabled || result.Policy.TotalMicros != nil {
			t.Fatalf("absent project budget inherited a policy: %+v", result)
		}
	})
	seedProjectBudget(t, first, first.Project, 1000)
	seedProjectBudget(t, second, second.Project, 2000)
	t.Run("administrator reads project policy", func(t *testing.T) {
		user := budgetActor(t, first, "admin", false)
		requireProjectBudget(t, readBudgetTool(t, first, user, backend), 1000)
	})
	t.Run("other organization reads its own policy", func(t *testing.T) {
		user := budgetActor(t, second, "owner", false)
		requireProjectBudget(t, readBudgetTool(t, second, user, backend), 2000)
		requireProjectBudget(t, readBudgetTool(t, first, owner, backend), 1000)
	})
	member := budgetActor(t, first, "member", true)
	for _, args := range []struct {
		key   string
		value any
	}{
		{"project_id", string(second.Project)}, {"organization_id", string(second.Primary)}, {"scope", "organization"}, {"unknown", true},
	} {
		t.Run("refuses "+args.key, func(t *testing.T) {
			startBudgetTurn(t, first, member)
			result := backend.run(t, "get_project_budget", map[string]any{args.key: args.value}, nil)
			if result.err != nil || result.value.Success {
				t.Fatalf("invalid scope argument %s reached the budget: %+v", args.key, result)
			}
		})
	}
	t.Run("revoked project grant cannot read budget", func(t *testing.T) {
		startBudgetTurn(t, first, member)
		result := backend.run(t, "get_project_budget", map[string]any{}, func() {
			if err := first.Repository.WriteTenant(t.Context(), first.Primary, corestore.Serving, func(writer tenantstore.Writer) error {
				return writer.DeleteAccessProjectGrant(t.Context(), member.Identity.Subject, first.Project)
			}); err != nil {
				t.Fatal(err)
			}
		})
		if result.err != nil || result.value.Success {
			t.Fatalf("revoked project authority received a policy: %+v", result)
		}
	})
}

func startBudgetTurn(t *testing.T, hub *hubfixture.Hosted, user hubfixture.HostedUser) {
	t.Helper()
	response := hub.Request(t, user, http.MethodPost, hub.Base+"/conversations", map[string]any{
		"key": hubapi.NewID("budget"), "first_message": map[string]any{"key": hubapi.NewID("message"), "text": "Read the monthly budget for this project."},
	})
	hubfixture.RequireStatus(t, response, http.StatusCreated)
}

func readBudgetTool(t *testing.T, hub *hubfixture.Hosted, user hubfixture.HostedUser, backend scriptedToolBackend) projectBudgetReply {
	t.Helper()
	startBudgetTurn(t, hub, user)
	result := backend.run(t, "get_project_budget", map[string]any{}, nil)
	if result.err != nil || !result.value.Success {
		t.Fatalf("project budget could not be read: %s (%v)", result.value.Content, result.err)
	}
	var output projectBudgetReply
	if err := json.Unmarshal([]byte(result.value.Content), &output); err != nil {
		t.Fatal(err)
	}
	return output
}

func requireProjectBudget(t *testing.T, value projectBudgetReply, cap int64) {
	t.Helper()
	if value.Revision != 1 || !value.Policy.Enabled || value.Policy.Mode != budget.HardStop || value.Policy.TotalMicros == nil || *value.Policy.TotalMicros != cap {
		t.Fatalf("project budget differs from its stored policy of %d: %+v", cap, value)
	}
}

func budgetActor(t *testing.T, hub *hubfixture.Hosted, role string, write bool) hubfixture.HostedUser {
	t.Helper()
	user := hub.User(t, hubapi.NewID("budget-user"), role, "budget@example.test", "", "")
	hub.Grant(t, user, hub.Project, write, false)
	return user
}

func seedProjectBudget(t *testing.T, hub *hubfixture.Hosted, project tracker.ProjectID, cap int64) {
	t.Helper()
	policy := budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, TotalMicros: &cap}
	if err := hub.Repository.WriteTenant(t.Context(), hub.Primary, corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetBillingMonthlyPolicy(t.Context(), billingstore.MonthlyPolicy{OrganizationID: hub.Primary, ProjectID: project, Policy: policy, Actor: "example-operator", UpdatedAt: hub.Now()}, 0)
	}); err != nil {
		t.Fatal(err)
	}
}
