package hubserver

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoordinatorRunnerTools(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	for _, test := range []struct {
		name, role, tool, tier string
		preview, execute       bool
		status                 int
	}{
		{"read unavailable tier", "member", "get_runners", "", true, false, 0},
		{"organization runner read", "member", "get_runners", "", true, false, 0},
		{"organization runner admin full access", "admin", "set_runner_tier", isolation.NativeTrusted, true, true, http.StatusOK},
		{"admin converts another owner's runner", "admin", "convert_runner_scope", "", true, true, http.StatusOK},
		{"unapproved conversion", "owner", "convert_runner_scope", "", true, false, 0},
		{"member cannot convert", "member", "convert_runner_scope", "", false, false, 0},
		{"admin full access", "admin", "set_runner_tier", isolation.NativeTrusted, true, true, http.StatusOK},
		{"owner sandbox", "owner", "set_runner_tier", isolation.Sandbox, true, true, http.StatusOK},
		{"unapproved preview", "owner", "set_runner_tier", isolation.NativeTrusted, true, false, 0},
		{"member cannot change", "member", "set_runner_tier", isolation.NativeTrusted, false, false, 0},
		{"invalid tier", "owner", "set_runner_tier", "user", false, false, 0},
		{"foreign runner", "owner", "set_runner_tier", isolation.NativeTrusted, false, false, 0},
		{"foreign read", "member", "get_runners", "", false, false, 0},
		{"stale preview", "owner", "set_runner_tier", isolation.NativeTrusted, true, true, http.StatusConflict},
		{"role revoked after preview", "owner", "set_runner_tier", isolation.NativeTrusted, true, true, http.StatusForbidden},
		{"project grant revoked", "owner", "set_runner_tier", isolation.NativeTrusted, true, true, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedSecurityFixture(t, func(cfg *Config) {
				cfg.Conversation = &ConversationConfig{Enabled: true, Backend: newFakeCoordinatorBackend(), Workspace: t.TempDir()}
			})
			owner := f.user(t, "runner-owner", "owner", "owner@example.test", "write", "")
			f.grant(t, owner, true, true)
			enrollmentRequest := runnerauth.EnrollmentRequest{ProjectIDs: []tracker.ProjectID{f.project}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat}, TTLSeconds: 900}
			if strings.HasPrefix(test.name, "organization runner") {
				enrollmentRequest.Scope, enrollmentRequest.ProjectIDs = "organization", nil
			}
			response := f.request(t, owner, http.MethodPost, "/api/v2/organizations/org_security/runner-enrollments", enrollmentRequest)
			requireNativeStatus(t, response, http.StatusCreated)
			var enrollment runnerauth.Enrollment
			decodeHubResponse(t, response, &enrollment)
			credential, err := apikey.GenerateToken()
			if err != nil {
				t.Fatal(err)
			}
			binding := runnerauth.NewBinding()
			r := runnerFixture{nativeFixture: nativeFixture{service: f.service, project: tracker.NativeProject{ID: f.project, OrganizationID: "org_security"}}, base: "/api/v2/organizations/org_security", enrollment: enrollment, binding: binding, redemption: runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "customer-host", DisplayName: "Runner", Capacity: 2, Version: "test"}}
			r.redemption.BackendIsolation = isolation.Report{"codex": {isolation.NativeTrusted}}
			if test.tier == isolation.Sandbox {
				r.redemption.IsolationTier = isolation.NativeTrusted
			}
			r.enroll(t)
			heartbeat := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": r.redemption.BackendIsolation, "problems": []runnerauth.Problem{}}
			if test.tier != isolation.Sandbox {
				heartbeat["problems"] = []runnerauth.Problem{runnerauth.NewProblem("tier_unavailable")}
			}
			postHeartbeat := func() {
				t.Helper()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, heartbeat), http.StatusOK)
			}
			postHeartbeat()
			before, err := readRunnerWithClock(t.Context(), f.service.database.db, "org_security", r.binding.RunnerID, f.service.config.now)
			if err != nil {
				t.Fatal(err)
			}
			user := owner
			if test.role != "owner" {
				user = f.user(t, "runner-caller", test.role, "caller@example.test", "write", "")
				f.grant(t, user, true, true)
			}
			response = f.request(t, user, http.MethodPost, f.base+"/conversations", map[string]any{"key": "tier-chat", "first_message": map[string]any{"key": "tier-message", "text": "why isn't my runner working"}})
			requireNativeStatus(t, response, http.StatusCreated)
			var created struct {
				Conversation struct {
					ID string `json:"id"`
				} `json:"conversation"`
			}
			decodeHubResponse(t, response, &created)
			c := f.service.conversations.coordinator.(*conversationTurnCoordinator)
			messages, err := c.service.store.listMessages(t.Context(), f.service.database.db, created.Conversation.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			var message conversationMessageRecord
			for _, m := range messages {
				if m.Role == conversation.RoleUser {
					message = m
				}
			}
			tools := newCoordinatorToolset(c, &coordinatorTurnState{coordinator: c, conversationID: created.Conversation.ID, users: []conversationMessageRecord{message}})
			args := map[string]any{}
			if test.tool == "convert_runner_scope" {
				args["runner_id"] = r.binding.RunnerID
			}
			if test.tool == "set_runner_tier" {
				args["runner_id"], args["isolation_tier"] = r.binding.RunnerID, test.tier
			}
			if test.name == "foreign runner" || test.name == "foreign read" {
				args["runner_id"] = "runner_foreign"
			}
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: test.tool, Arguments: raw})
			if err != nil || result.Success != test.preview {
				t.Fatalf("tool=%+v err=%v", result, err)
			}
			if test.tool == "get_runners" && test.preview {
				var view struct {
					Runners []runnerauth.Runner `json:"runners"`
				}
				decodeToolResult(t, result, &view)
				if len(view.Runners) != 1 || view.Runners[0].IsolationTier != isolation.Sandbox || !slices.ContainsFunc(view.Runners[0].Problems, func(p runnerauth.Problem) bool { return p.Code == "tier_unavailable" }) || !reflect.DeepEqual(view.Runners[0].BackendIsolation, map[string][]string{"codex": {isolation.NativeTrusted}}) {
					t.Fatalf("runner diagnostic=%s", result.Content)
				}
			}
			if test.execute {
				messages, err = c.service.store.listMessages(t.Context(), f.service.database.db, created.Conversation.ID, 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				var action chat.Action
				for _, m := range messages {
					var data struct {
						Proposal struct {
							Action chat.Action `json:"action"`
						} `json:"operator_action"`
					}
					if json.Unmarshal(m.Data, &data) == nil && string(data.Proposal.Action.Kind) == test.tool {
						action = data.Proposal.Action
					}
				}
				if action.ID == "" {
					t.Fatal("missing preview")
				}
				switch test.name {
				case "stale preview":
					operatorSQL(t, f, "UPDATE runner_identities SET revision=revision+1 WHERE id=?", r.binding.RunnerID)
				case "role revoked after preview":
					operatorSQL(t, f, "UPDATE hosted_members SET role='member' WHERE user_id=?", user.identity.Subject)
				case "project grant revoked":
					f.grant(t, user, false, false)
				}
				response = f.request(t, user, http.MethodPost, f.base+"/conversations/"+created.Conversation.ID+"/actions", action)
				requireNativeStatus(t, response, test.status)
			}
			if test.execute && test.status == http.StatusOK {
				heartbeat["problems"] = []runnerauth.Problem{}
				postHeartbeat()
			}
			after, err := readRunnerWithClock(t.Context(), f.service.database.db, "org_security", r.binding.RunnerID, f.service.config.now)
			if err != nil {
				t.Fatal(err)
			}
			want := before.IsolationTier
			if test.execute && test.status == http.StatusOK && test.tool == "set_runner_tier" {
				want = test.tier
			}
			if after.IsolationTier != want {
				t.Fatalf("tier=%q want=%q", after.IsolationTier, want)
			}
			expected := before.Routing
			expected.IsolationTier = want
			if test.execute && test.status == http.StatusOK && test.tool == "convert_runner_scope" {
				expected.Scope = "organization"
				expected.ProjectIDs = []tracker.ProjectID{}
			}
			if !reflect.DeepEqual(after.Routing, expected) {
				t.Fatalf("routing changed beyond tier: before=%+v after=%+v", expected, after.Routing)
			}
			if after.Binding != before.Binding {
				t.Fatal("tier change re-enrolled runner")
			}
			if test.execute && test.status == http.StatusOK && test.tier == isolation.NativeTrusted && slices.ContainsFunc(after.Problems, func(p runnerauth.Problem) bool { return p.Code == "tier_unavailable" }) {
				t.Fatal("tier problem still present after supported switch")
			}
		})
	}
}
