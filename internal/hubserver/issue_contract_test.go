package hubserver

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeContractTestBody = "## Acceptance criteria\nThe change works.\n## Must not break\nExisting behavior.\n## How we know it worked\nThe focused tests pass."

func TestNativeIssueContract(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	t.Parallel()
	complete := "## Acceptance criteria\nFix the endpoint.\n## Must not break\nExisting requests.\n## How we know it worked\nRun the endpoint tests."
	for _, test := range []struct {
		name      string
		body      string
		machine   bool
		claimable bool
		custom    bool
		refused   bool
	}{
		{name: "missing", refused: true},
		{name: "partial", body: "## Acceptance criteria\nFix endpoint.", refused: true},
		{name: "machine missing", machine: true},
		{name: "human", body: complete, claimable: true},
		{name: "machine", body: complete, machine: true},
		{name: "custom authored contract", body: "## Result\nThe endpoint works.", claimable: true, custom: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "contract-"+test.name)
			descriptor := hubTestPolicy()
			states := nativeFixtureStates()
			states[0].Transitions = append(states[0].Transitions, "Blocked")
			states = append(states, tracker.NativeState{Name: "Blocked", Transitions: []string{"Todo"}})
			descriptor.Workflow = &policy.Workflow{Source: "detent.yaml", States: states}
			descriptor = descriptor.WithID()
			if test.custom {
				workflow, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{Workflow: []byte("## Issue Contract\n- Result\n"), Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: acme/orders\n  lanes:\n    - {name: Todo, role: active}\n    - {name: In Progress, role: active}\n    - {name: Done, role: terminal}\n    - {name: Blocked, role: holding}\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [In Progress, Done, Blocked]\n      In Progress: [Todo, Done]\n      Blocked: [Todo]\n      Done: [Todo]\n"), HasConfig: true, ConfigPath: "detent.yaml"})
				if err != nil {
					t.Fatal(err)
				}
				workflow.Definition.Revision = strings.Repeat("a", 40)
				descriptor, err = config.ResolvePolicy(workflow)
				if err != nil {
					t.Fatal(err)
				}
				descriptor.Configuration = nil
				descriptor = descriptor.WithID()
			}
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			body := test.body
			if test.machine {
				body = issueorigin.Stamp(body, issueorigin.Origin{Kind: "worker", Fingerprint: "endpoint"})
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create"}, Title: "Fix endpoint", Body: body, State: "Todo"})
			if test.refused {
				requireNativeStatus(t, response, http.StatusUnprocessableEntity)
				return
			}
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			issue = tracker.NativeIssue{}
			decodeHubResponse(t, response, &issue)
			worker := f.worker(t, "contract-worker")
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": "contract-machine", "hostname": "runner", "capacity": 1, "version": "test"}), http.StatusOK)
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			check := func(want bool) {
				t.Helper()
				_, id, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(issue.WorkItemID))
				if err != nil {
					t.Fatal(err)
				}
				got, err := nativeIssueContractClaimable(t.Context(), f.service.database.db, scope, id)
				if err != nil || got != want {
					contract, contractErr := nativeIssueContract(t.Context(), f.service.database.db, scope)
					t.Fatalf("claimable = %t, %v; want %t; contract %+v (%v), criteria %+v, evaluation %+v", got, err, want, contract, contractErr, issue.IssueContract, contract.Evaluate(nativeContractIssue(issue)))
				}
				expected := http.StatusConflict
				if want {
					expected = http.StatusOK
				}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: "contract-machine", SessionID: "contract-session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
				requireNativeStatus(t, response, expected)
			}
			check(test.claimable)
			if test.claimable {
				return
			}
			contract, err := config.ResolveIssueContract("")
			if err != nil {
				t.Fatal(err)
			}
			action := contract.HumanAction(nativeContractIssue(issue), contract.Evaluate(nativeContractIssue(issue)))
			hold := fmt.Sprintf("## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: %q\nfields:\n  issue_contract_return_state: Todo\n```", action)
			path := f.base + "/work-items/" + string(issue.WorkItemID)
			response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", worker, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "hold"}, ExpectedRevision: issue.Revision, State: "Blocked", Reason: "worker_progress", ReasonDetail: hold})
			requireNativeStatus(t, response, http.StatusOK)
			issue = tracker.NativeIssue{}
			decodeHubResponse(t, response, &issue)
			if issue.State != "Blocked" || issue.IssueContract.HumanAction != action || issue.IssueContract.ReturnState != "Todo" {
				t.Fatalf("missing persisted human action: %+v", issue)
			}
			recovery := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "premature"}, ExpectedRevision: issue.Revision, State: "Todo", Reason: "worker_progress", ReasonDetail: "recorded blocker recovery"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", worker, recovery), http.StatusUnprocessableEntity)

			fixed := complete
			if test.machine {
				fixed = issueorigin.Stamp(complete, issueorigin.Origin{Kind: "worker", Fingerprint: "endpoint"})
			}
			response = performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "confirm"}, ExpectedRevision: issue.Revision, Body: &fixed})
			requireNativeStatus(t, response, http.StatusOK)
			issue = tracker.NativeIssue{}
			decodeHubResponse(t, response, &issue)
			if issue.State != "Blocked" || issue.IssueContract.HumanAction != "" {
				t.Fatalf("human edit did not clear the contract action: %+v", issue.IssueContract)
			}
			recovery.ExpectedRevision, recovery.IdempotencyKey = issue.Revision, "recover"
			response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", worker, recovery)
			requireNativeStatus(t, response, http.StatusOK)
			issue = tracker.NativeIssue{}
			decodeHubResponse(t, response, &issue)
			check(true)
			if strings.TrimSpace(issue.IssueContract.ReturnState) != "" {
				t.Fatal("recovered issue retained its contract hold")
			}
		})
	}
}

func TestIssueContractRolloutMigration(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, ddl := range []string{
		"CREATE TABLE issues (native_id TEXT, organization_id TEXT, project_id TEXT, workflow_state_id INTEGER, body TEXT, actor_json TEXT)",
		"CREATE TABLE workflow_states (id INTEGER, detent_state TEXT)",
		"CREATE TABLE collaboration_events (sequence INTEGER, organization_id TEXT, project_id TEXT, work_item_id TEXT, type TEXT, actor_json TEXT, data_json TEXT)",
		"INSERT INTO workflow_states VALUES (1, 'Backlog'), (2, 'Todo'), (3, 'In Progress'), (4, 'Human Review'), (5, 'Blocked')",
	} {
		if _, err := db.ExecContext(t.Context(), ddl); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		id    string
		state int
		actor string
	}{
		{"human backlog", 1, "human"}, {"machine backlog", 1, "runner"}, {"existing todo", 2, "human"}, {"in flight", 3, "runner"}, {"review", 4, "runner"}, {"human hold", 5, "human"},
	} {
		if _, err := db.ExecContext(t.Context(), "INSERT INTO issues VALUES (?, 'org', 'project', ?, 'Criteria', ?)", row.id, row.state, fmt.Sprintf(`{"kind":%q}`, row.actor)); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := migrationFiles.ReadFile("migrations/20261007193000_issue_contract.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(raw), "-- +goose Down")
	if _, err := db.ExecContext(t.Context(), up); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id        string
		exempt    bool
		confirmed string
	}{
		{"human backlog", false, "Criteria"}, {"machine backlog", false, ""}, {"existing todo", true, ""}, {"in flight", true, ""}, {"review", true, ""}, {"human hold", true, ""},
	} {
		var exempt bool
		var confirmed string
		if err := db.QueryRowContext(t.Context(), "SELECT COALESCE(json_extract(issue_contract_json, '$.exempt'), 0), COALESCE(json_extract(issue_contract_json, '$.confirmed_body'), '') FROM issues WHERE native_id = ?", test.id).Scan(&exempt, &confirmed); err != nil {
			t.Fatal(err)
		}
		if exempt != test.exempt || confirmed != test.confirmed {
			t.Fatalf("%s: exempt %t, confirmed %q", test.id, exempt, confirmed)
		}
	}
}
