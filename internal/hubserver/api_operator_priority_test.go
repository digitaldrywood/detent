package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// The queue's priority is a field an operator can take off again, not only
// move between levels. "none" is the word that says so; every other word
// still names a level, and the mirrored `priority:` label follows either way.
func TestChangeWorkItemPriorityClears(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "priority authority")
	service := f.service
	organization := f.project.OrganizationID
	project := tracker.ProjectID(hubapi.NewID("prj"))
	if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		stored, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Compatible project", Profile: "github_compatible", RequireDependencies: true}, States: nativeFixtureStates()})
		if err != nil {
			return err
		}
		stored.Settings.Repository, stored.Settings.CheckoutRepository, stored.Settings.RepositoryEnabled = "example-org/example-project", "example-org/example-project", true
		_, err = w.UpdateProjectRecord(t.Context(), stored)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	source := newCoreNeutralFixture(t, organization, "issues", "workflow_states", "repositories", "queue_entries", "native_comments", "collaboration_versions", "collaboration_events", "issue_dependencies", "change_content_objects", "attachments")
	source.add("workflow_states", map[string]any{"id": 1, "project_id": project, "detent_state": "Todo"})
	source.add("repositories", map[string]any{"id": 1, "github_owner": "example-org", "github_name": "example-project"})
	const stamp = "2026-10-01T00:00:00Z"
	actor := tracker.Actor{Kind: "human", PrincipalID: "example-owner"}
	for index, id := range []tracker.NativeWorkItemID{"wi_priority_first", "wi_priority_second"} {
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: project, WorkItemID: id, Number: index + 1, Revision: 1, Profile: "github_compatible"}, Title: "Retained source", Body: "Retained GitHub content", State: "Todo", Actor: actor, CreatedAt: parseTestTime(t, stamp), UpdatedAt: parseTestTime(t, stamp), LastActivityAt: parseTestTime(t, stamp)}
		issue.ExternalReferences = []tracker.ExternalReference{tracker.GitHubIssueSourceReference(fmt.Sprintf("I_retained_%d", index+1), fmt.Sprintf("https://github.com/example-org/example-project/issues/%d", index+1))}
		source.add("issues", map[string]any{"id": 41 + index, "native_id": id, "project_id": project, "number": index + 1, "revision": 1, "event_sequence": 1, "title": issue.Title, "body": issue.Body, "issue_contract_json": nil, "actor_json": string(historyTestJSON(t, actor)), "archived": 0, "created_at": stamp, "updated_at": stamp, "native_created_at": stamp, "native_updated_at": stamp, "last_activity_at": stamp, "workflow_state_id": 1, "labels_json": "[]", "assignees_json": "[]", "author_login": "example-owner", "repository_id": 1})
		source.add("collaboration_versions", map[string]any{"project_id": project, "work_item_id": id, "record_id": id, "revision": 1, "encoding": "snapshot", "record_json": string(historyTestJSON(t, issue))})
		source.add("collaboration_events", map[string]any{"id": "event_" + string(id), "project_id": project, "work_item_id": id, "sequence": 1, "type": "issue.created", "schema_version": 1, "actor_json": string(historyTestJSON(t, actor)), "data_json": string(historyTestJSON(t, tracker.CollaborationData{Revision: 1})), "event_revision": 1, "recorded_at": stamp})
	}
	count, err := ImportCoreWorkItems(t.Context(), service.config.TenantRepository.(*postgres.Repository), CoreWorkItemImportSource{BundlePath: source.write(), OrganizationID: organization, ProjectID: project, Objects: service.runDetailStore()})
	if err != nil || count != 2 {
		t.Fatalf("retained compatible import=%d error=%v", count, err)
	}
	seedHubAPIToken(t, service, "priority-operator", "priority-operator-token", hubapi.APIScopeOperator)
	admin := nativeAttemptTestDatabase(t, f)
	path := "/api/v2/organizations/" + string(organization) + "/mcp"
	readPriority := func(t *testing.T, item tracker.NativeWorkItemID) (int64, bool) {
		t.Helper()
		var value int64
		var valid bool
		if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			entry, err := r.QueueEntry(t.Context(), project, item, "fleet")
			if err == nil && entry.Priority != nil {
				value, valid = *entry.Priority, true
			}
			return err
		}); err != nil {
			t.Fatalf("read queue entry: %v", err)
		}
		return value, valid
	}
	readDesired := func(t *testing.T, item tracker.NativeWorkItemID) WorkflowLabelDesired {
		t.Helper()
		var key string
		if err := admin.QueryRowContext(t.Context(), "SELECT command_key FROM github_outbound WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 ORDER BY id DESC LIMIT 1", organization, project, item).Scan(&key); err != nil {
			t.Fatalf("read outbox record: %v", err)
		}
		var outbound projectstore.GitHubOutbound
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			owner, ok := w.(projectstore.GitHubOutboxWriter)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			var err error
			outbound, err = owner.GitHubOutbound(t.Context(), key)
			return err
		}); err != nil {
			t.Fatalf("read outbox record %q: %v", key, err)
		}
		if outbound.WorkItemID != item || outbound.Repository != "example-org/example-project" || outbound.Kind != string(MutationWorkflowLabel) {
			t.Fatalf("outbox target=%+v", outbound)
		}
		encoded, err := eventstore.NewContent(service.runDetailStore()).Receipt(t.Context(), outbound.Payload)
		if err != nil {
			t.Fatalf("read outbox payload %q: %v", key, err)
		}
		var desired WorkflowLabelDesired
		if err := json.Unmarshal(encoded, &desired); err != nil {
			t.Fatalf("decode outbox record %q: %v", key, err)
		}
		return desired
	}
	for index, item := range []tracker.NativeWorkItemID{"wi_priority_first", "wi_priority_second"} {
		t.Run(string(item), func(t *testing.T) {
			outbounds := 0
			for _, test := range []struct {
				name, priority, key  string
				token, code, message string
				failed               bool
				want                 int64
				valid                bool
				label                string
				clear                bool
			}{
				{name: "set", priority: "high", key: "priority-set", want: tracker.QueuePriorityHigh, valid: true, label: "priority:high"},
				{name: "clear", priority: "none", key: "priority-clear", clear: true},
				{name: "clear twice", priority: "none", key: "priority-clear-again", clear: true},
				{name: "set again", priority: "urgent", key: "priority-reset", want: tracker.QueuePriorityUrgent, valid: true, label: "priority:urgent"},
				{name: "operator fleet denied", priority: "high", key: "priority-operator-denied", token: "priority-operator-token", failed: true, want: tracker.QueuePriorityUrgent, valid: true, message: operatortool.ErrAccessDenied.Error()},
				{name: "nonsense", priority: "highest", key: "priority-bad", failed: true, want: tracker.QueuePriorityUrgent, valid: true, code: "invalid_request"},
			} {
				t.Run(test.name, func(t *testing.T) {
					fields := map[string]any{"project_id": project, "identifier": item, "queue_scope": "fleet", "state": "In Progress", "queue_priority": test.priority, "request_id": string(item) + ":" + test.key}
					body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": operatortool.SetQueuePriority, "arguments": fields, "_meta": json.RawMessage(fleetProtocolMeta)}}
					token := testHubAdminToken
					if test.token != "" {
						token = test.token
					}
					var replay string
					for attempt := range 2 {
						response := performHubWorkCall(t, service, path, token, operatortool.SetQueuePriority, body)
						requireNativeStatus(t, response, http.StatusOK)
						var envelope struct {
							Result struct {
								Failed  bool            `json:"isError"`
								Content json.RawMessage `json:"structuredContent"`
								Text    []struct {
									Text string `json:"text"`
								} `json:"content"`
							} `json:"result"`
						}
						decodeHubResponse(t, response, &envelope)
						if envelope.Result.Failed != test.failed {
							t.Fatalf("priority refusal=%t want=%t body=%s", envelope.Result.Failed, test.failed, response.Body.String())
						}
						if test.code != "" {
							var refusal operatortool.RequestError
							if err := json.Unmarshal(envelope.Result.Content, &refusal); err != nil || refusal.Code != test.code {
								t.Fatalf("priority invalid-input refusal=%s want code=%s error=%v", envelope.Result.Content, test.code, err)
							}
						}
						if test.message != "" && (len(envelope.Result.Text) != 1 || envelope.Result.Text[0].Text != test.message) {
							t.Fatalf("priority authority refusal=%+v want=%q", envelope.Result.Text, test.message)
						}
						receipt := string(envelope.Result.Content)
						if !test.failed {
							var stored struct {
								ResourceID string          `json:"resource_id"`
								URL        string          `json:"url"`
								Data       json.RawMessage `json:"data"`
							}
							if err := json.Unmarshal(envelope.Result.Content, &stored); err != nil || len(stored.Data) == 0 {
								t.Fatalf("priority receipt=%s error=%v", envelope.Result.Content, err)
							}
							receipt = stored.ResourceID + "\n" + stored.URL + "\n" + string(stored.Data)
						}
						if attempt == 0 {
							replay = receipt
							if !test.failed {
								outbounds++
							}
						} else if receipt != replay {
							t.Fatalf("priority retry changed its receipt: first=%s retry=%s", replay, receipt)
						}
						value, valid := readPriority(t, item)
						if valid != test.valid || (valid && value != test.want) {
							t.Fatalf("priority_override = (%d, %v), want (%d, %v)", value, valid, test.want, test.valid)
						}
						var count int
						if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", organization, project, item).Scan(&count); err != nil || count != outbounds {
							t.Fatalf("priority outbounds=%d want=%d error=%v", count, outbounds, err)
						}
						if test.failed {
							continue
						}
						desired := readDesired(t, item)
						if desired.Clear != test.clear || desired.Label != test.label {
							t.Fatalf("outbox desired = %#v, want clear %v label %q", desired, test.clear, test.label)
						}
						if desired.ManagedPrefix != "priority:" {
							t.Fatalf("managed prefix = %q", desired.ManagedPrefix)
						}
					}
				})
			}
			var issueNumber int
			if err := admin.QueryRowContext(t.Context(), "SELECT issue_number FROM github_outbound WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 ORDER BY id DESC LIMIT 1", organization, project, item).Scan(&issueNumber); err != nil || issueNumber != index+1 {
				t.Fatalf("priority target issue=%d want=%d error=%v", issueNumber, index+1, err)
			}
		})
	}
}
