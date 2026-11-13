package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type linkedTestImporter struct{ snapshot tracker.GitHubIssueSnapshot }

func (i linkedTestImporter) FetchImportPage(_ context.Context, request GitHubImportRequest) (GitHubImportPage, error) {
	snapshot := i.snapshot
	if request.Stage == "issue" {
		return GitHubImportPage{Issue: &IssueSource{NodeID: snapshot.Provenance.ExternalID, Number: linkedSourceNumber(snapshot.URL), URL: snapshot.URL, Title: snapshot.Title, Body: snapshot.Body, AuthorID: snapshot.Provenance.AuthorID, CreatedAt: snapshot.Provenance.CreatedAt, UpdatedAt: snapshot.Provenance.UpdatedAt}}, nil
	}
	page := GitHubImportPage{}
	for _, comment := range snapshot.Comments {
		page.Records = append(page.Records, GitHubImportRecord{Kind: "comment", Body: comment.Body, Provenance: comment.Provenance})
	}
	return page, nil
}

func linkedWebhookPayload(t *testing.T, action string, snapshot tracker.GitHubIssueSnapshot, comment *githubWebhookComment) []byte {
	t.Helper()
	owner, repository, node, fullName := "acme", "orders", "R_repo", "acme/orders"
	number, state, author := 12, "open", snapshot.Provenance.AuthorID
	if action == "closed" {
		state = "closed"
	}
	created, err := json.Marshal(snapshot.Provenance.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := json.Marshal(snapshot.Provenance.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(snapshot.Body)
	if err != nil {
		t.Fatal(err)
	}
	payload := githubWebhookPayload{Repository: &githubWebhookRepository{NodeID: &node, Name: &repository, FullName: &fullName, Owner: &githubWebhookActor{Login: &owner}},
		Issue: &githubWebhookIssue{NodeID: &snapshot.Provenance.ExternalID, Number: &number, Title: &snapshot.Title, Body: body, HTMLURL: &snapshot.URL, State: &state, User: &githubWebhookActor{Login: &author}, Labels: json.RawMessage(`[]`), Assignees: json.RawMessage(`[]`), CreatedAt: created, UpdatedAt: updated}, Comment: comment}
	raw, err := json.Marshal(struct {
		githubWebhookPayload
		Action string `json:"action"`
	}{payload, action})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func applyLinkedWebhook(t *testing.T, f nativeFixture, action, event string, snapshot tracker.GitHubIssueSnapshot, comment *githubWebhookComment) {
	t.Helper()
	raw := linkedWebhookPayload(t, action, snapshot, comment)
	var payload githubWebhookPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	scope, err := f.service.gitHubIntegrationScope(t.Context(), f.project.OrganizationID, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), linkedSourceURLKey{}, "https://cloud.example")
	identity, err := hubapi.CommandHash(raw)
	if err != nil {
		t.Fatal(err)
	}
	row := projectstore.GitHubWebhook{DeliveryID: identity, EventType: event, Action: action, Repository: scope.integrationRepository, ReceivedAt: f.service.config.now()}
	if err := f.service.applyGitHubWebhookRecord(ctx, scope, row, payload); err != nil {
		t.Fatal(err)
	}
}

func TestNativeGitHubWebhookIntake(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, body, author, action, wantState string }{
		{"public reporter", "A public report", "outside-reporter", "opened", "Triage"},
		{"empty body is not a spam signal", "", "reporter", "opened", "Triage"},
		{"unavailable author retains report", "Report", "", "opened", "Triage"},
		{"closed source awaiting intake", "Closed report" + issueContractTestSections, "reporter", "closed", "Todo"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := linkedFixture(t)
			snapshot := linkedSnapshot()
			snapshot.Body, snapshot.Provenance.AuthorID = test.body, test.author
			f.service.config.GitHubWebhookSecret = []byte(testWebhookSecret)
			if test.action == "closed" {
				pendingLinkedIssue(t, f)
			}
			payload := string(linkedWebhookPayload(t, test.action, snapshot, nil))
			for range 2 {
				requireNativeStatus(t, sendSignedWebhookRequest(t, f.service, "intake-delivery", "issues", payload), http.StatusAccepted)
			}
			applyLinkedWebhook(t, f, test.action, "issues", snapshot, nil)
			var source eventstore.LinkedSource
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				source, err = r.LinkedSourceByURL(t.Context(), f.project.ID, snapshot.URL)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			id := source.WorkItemID
			issue := readWorkItem(t, f, id, "")
			if issue.State != test.wantState || issue.LinkedSource.Status != "complete" || issue.Body != test.body || issue.Terminal || len(issue.Assignees) != 0 {
				t.Fatalf("intake = %+v", issue)
			}
			if test.author == "" {
				test.author = "unavailable"
			}
			if issue.Provenance.AuthorID != test.author {
				t.Fatalf("author = %+v", issue.Provenance)
			}
			manual := f.link(t, "manual-dedup")
			if manual.WorkItemID != issue.WorkItemID {
				t.Fatal("manual intake created a duplicate")
			}
			var count int
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("intake outbox rows = %d, %v", count, err)
			}
		})
	}
}

func TestNativeGitHubSourceUpdates(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"Triage", "Todo", "Cancelled", "Done"} {
		t.Run(state, func(t *testing.T) {
			f := linkedFixture(t)
			snapshot := linkedSnapshot()
			applyLinkedWebhook(t, f, "opened", "issues", snapshot, nil)
			var source eventstore.LinkedSource
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				source, err = r.LinkedSourceByURL(t.Context(), f.project.ID, snapshot.URL)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			id := source.WorkItemID
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				organization, err := w.AccessOrganization(t.Context())
				if err != nil {
					return err
				}
				return w.SetAccessOrganizationSlug(t.Context(), &organization.Slug, "local-organization")
			}); err != nil {
				t.Fatal(err)
			}
			issue := readWorkItem(t, f, id, "")
			destinations := []string{state}
			if state == "Done" {
				destinations = []string{"Todo", "Done"}
			}
			for index, destination := range destinations {
				if issue.State == destination {
					continue
				}
				request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "source-update-state-" + strconv.Itoa(index)}, ExpectedRevision: issue.Revision, State: destination, Reason: "user_requested"}
				ctx := context.WithValue(t.Context(), linkedSourceURLKey{}, "https://cloud.example")
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, f.base+"/work-items/"+string(id)+"/workflow", f.token, request).WithContext(ctx))
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &issue)
			}
			for _, action := range []string{"closed", "reopened"} {
				snapshot.Provenance.UpdatedAt = snapshot.Provenance.UpdatedAt.Add(time.Second)
				applyLinkedWebhook(t, f, action, "issues", snapshot, nil)
				applyLinkedWebhook(t, f, action, "issues", snapshot, nil)
			}
			author := "commenter"
			comment := &githubWebhookComment{NodeID: "C_webhook", Body: "External discussion", User: &githubWebhookActor{Login: &author}, CreatedAt: snapshot.Provenance.UpdatedAt, UpdatedAt: snapshot.Provenance.UpdatedAt}
			applyLinkedWebhook(t, f, "created", "issue_comment", snapshot, comment)
			applyLinkedWebhook(t, f, "created", "issue_comment", snapshot, comment)
			issue = readWorkItem(t, f, id, "")
			if issue.State != state {
				t.Fatalf("state changed = %s", issue.State)
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(id)+"/comments", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var comments tracker.Page[tracker.NativeComment]
			decodeHubResponse(t, response, &comments)
			if len(comments.Items) != 3 || comments.NextCursor != "" {
				t.Fatalf("comments = %+v", comments)
			}
			var closes int
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&closes); err != nil {
				t.Fatal(err)
			}
			want := 0
			if state == "Cancelled" {
				want = 1
			}
			if closes != want {
				t.Fatalf("closes = %d, want %d", closes, want)
			}
			if state == "Cancelled" {
				var key string
				if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT command_key FROM github_outbound WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", f.project.OrganizationID, f.project.ID, id).Scan(&key); err != nil {
					t.Fatal(err)
				}
				var outbound projectstore.GitHubOutbound
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					outbound, err = r.(projectstore.GitHubOutboxWriter).GitHubOutbound(t.Context(), key)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				raw, err := eventstore.NewContent(f.service.runDetailStore()).Receipt(t.Context(), outbound.Payload)
				if err != nil {
					t.Fatal(err)
				}
				var desired WorkpadDesired
				if err := json.Unmarshal(raw, &desired); err != nil {
					t.Fatal(err)
				}
				if desired.Body != "Cancelled in Detent: https://cloud.example/local-organization/linked/issues/1" || !desired.CloseSource || desired.StateReason != "not_planned" {
					t.Fatalf("cancellation summary = %+v", desired)
				}
				calls := make(chan OutboxItem, 1)
				f.service.config.OutboxBackend = githubOutboundSignal{calls: calls}
				if worked, err := f.service.ProcessOutbox(t.Context()); err != nil || !worked {
					t.Fatalf("outbox processing = %t, %v", worked, err)
				}
				select {
				case item := <-calls:
					if item.IssueNumber != 12 || item.Profile != "native" || item.RepositoryOwner != "acme" {
						t.Fatalf("outbox source routing = %+v", item)
					}
				default:
					t.Fatal("outbox processing did not deliver the source mutation")
				}
			}
			r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(id)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "private"}, Body: "Private Cloud discussion"})
			requireNativeStatus(t, r, http.StatusOK)
			var total int
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&total); err != nil || total != want {
				t.Fatalf("Cloud discussion mirrored: %d, %v", total, err)
			}
		})
	}
}

func TestNativeTriageWorkflowProjection(t *testing.T) {
	t.Parallel()
	for _, custom := range []bool{false, true} {
		for _, bound := range []bool{false, true} {
			t.Run(strconv.FormatBool(custom)+"/"+strconv.FormatBool(bound), func(t *testing.T) {
				f := newNativeFixture(t, nil, "", "triage")
				scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
				if bound {
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
						project, err := w.ProjectRecord(t.Context(), scope.project)
						if err != nil {
							return err
						}
						project.Settings.CheckoutRepository = "acme/orders"
						_, err = w.UpdateProjectRecord(t.Context(), project)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				states := nativeFixtureStates()
				states = append(states, tracker.NativeState{Name: "Triage", Dispatchable: true, Transitions: []string{"Todo"}})
				if custom {
					states = []tracker.NativeState{
						{Name: "Ready", Transitions: []string{"Working"}},
						{Name: "Working", Dispatchable: true},
						{Name: "Triage", Dispatchable: true, Transitions: []string{"Working"}},
					}
				}
				current, err := f.service.readProjectRecord(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, updateProjectIntegrationRequest{Mutation: tracker.Mutation{IdempotencyKey: "triage-workflow"}, ExpectedRevision: tracker.Revision(current.Settings.Revision), Intake: "disabled", Projection: "disabled", States: &states})
				requireNativeStatus(t, response, http.StatusOK)
				project, err := f.service.readProjectRecord(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				if project.States[0].Name != "Triage" || project.States[0].Terminal || project.States[0].Dispatchable {
					t.Fatalf("lanes = %+v", project.States)
				}
				if custom && !reflect.DeepEqual(project.States, []tracker.NativeState{
					{Name: "Triage", Transitions: []string{"Working"}},
					{Name: "Ready", Transitions: []string{"Working"}},
					{Name: "Working", Dispatchable: true, Transitions: []string{}},
				}) {
					t.Fatalf("custom workflow changed: %+v", project.States)
				}
			})
		}
	}
}

func pendingLinkedIssue(t *testing.T, f nativeFixture) tracker.NativeIssue {
	t.Helper()
	source := coreWorkItemNeutralFixtureBody(t, f.project.OrganizationID, f.project.ID, false, "")
	linked := newCoreNeutralFixture(t, f.project.OrganizationID, "linked_issue_sources")
	source.tables["linked_issue_sources"] = linked.tables["linked_issue_sources"]
	source.add("linked_issue_sources", map[string]any{"work_item_id": "wi_core_import", "source_url": linkedSnapshot().URL, "title_supplied": false, "body_supplied": false, "snapshot_json": nil})
	source.set("issues", map[string]string{"native_id": "wi_core_import"}, map[string]any{"assignees_json": "[]"})
	var originalPrevious, canonicalPrevious []byte
	versions := 0
	for _, row := range source.records["collaboration_versions"] {
		if string(row.Values["record_id"].Data) != "wi_core_import" {
			continue
		}
		raw := row.Values["record_json"].Data
		if string(row.Values["encoding"].Data) == "patch" {
			var err error
			raw, err = applyNativeVersionPatch(originalPrevious, raw)
			if err != nil {
				t.Fatal(err)
			}
		}
		originalPrevious = raw
		var historical tracker.NativeIssue
		if err := json.Unmarshal(raw, &historical); err != nil {
			t.Fatal(err)
		}
		historical.Assignees = nil
		historical.LinkedSource = &tracker.LinkedIssueSource{URL: linkedSnapshot().URL, Status: "pending"}
		raw, err := json.Marshal(historical)
		if err != nil {
			t.Fatal(err)
		}
		keys, fields, err := nativeVersionObject(raw)
		if err != nil {
			t.Fatal(err)
		}
		raw, err = nativeVersionObjectBytes(keys, fields)
		if err != nil {
			t.Fatal(err)
		}
		encoded, encoding, err := encodeNativeVersion(canonicalPrevious, "snapshot", raw, int64(historical.Revision))
		if err != nil {
			t.Fatalf("historical linked version encoding=%s error=%v", encoding, err)
		}
		source.set("collaboration_versions", map[string]string{"record_id": "wi_core_import", "revision": strconv.FormatInt(int64(historical.Revision), 10)}, map[string]any{"encoding": encoding, "record_json": string(encoded)})
		canonicalPrevious = raw
		versions++
	}
	if versions != 2 {
		t.Fatalf("historical linked versions = %d, want 2", versions)
	}
	input := CoreWorkItemImportSource{BundlePath: source.write(), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Objects: f.service.runDetailStore()}
	if count, err := ImportCoreWorkItems(t.Context(), f.service.database.postgres, input); err != nil || count != 2 {
		t.Fatalf("historical pending source import = %d, %v", count, err)
	}
	other := readWorkItem(t, f, "wi_core_import_second", "")
	if other.State != "Todo" || other.Revision != 2 || other.Title != "Updated title" || other.Body != "" {
		t.Fatalf("historical companion issue = %+v", other)
	}
	issue := readWorkItem(t, f, "wi_core_import", "")
	if issue.State != "Todo" {
		t.Fatalf("historical pending source lane = %s, want Todo", issue.State)
	}
	if issue.LinkedSource == nil || issue.LinkedSource.Status != "pending" {
		t.Fatal("linked source must await intake")
	}
	return issue
}

func TestNativeGitHubTriageMigration(t *testing.T) {
	t.Parallel()
	for _, binding := range []string{"", "checkout", "repository"} {
		t.Run(binding, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "triage-migration")
			historicalStates := nativeFixtureStates()
			if binding != "" {
				historicalStates = append([]tracker.NativeState{{Name: "Triage", Transitions: []string{"Todo"}}}, historicalStates...)
			}
			states, err := json.Marshal(historicalStates)
			if err != nil {
				t.Fatal(err)
			}
			source := newCoreNeutralFixture(t, f.project.OrganizationID, "projects", "repositories")
			projectID := tracker.ProjectID("prj_historical_triage")
			values := map[string]any{"id": projectID, "name": "Historical project", "slug": "historical-project", "profile": "native", "states_json": string(states), "created_at": testTimestamp, "content_settings_revision": 1, "integration_revision": 1, "model_selection_revision": 1}
			if binding == "checkout" {
				values["checkout_repository"] = "acme/orders"
			}
			if binding == "repository" {
				source.add("repositories", map[string]any{"id": 1, "github_node_id": "R_migration", "github_owner": "acme", "github_name": "orders", "created_at": testTimestamp, "updated_at": testTimestamp})
				values["repository_id"] = 1
			}
			source.add("projects", values)
			input := postgres.CoreProjectImportSource{BundlePath: source.write(), OrganizationID: f.project.OrganizationID, Objects: f.service.runDetailStore()}
			if count, err := f.service.database.postgres.ImportCoreProjects(t.Context(), input); err != nil || count != 1 {
				t.Fatalf("historical project import = %d, %v", count, err)
			}
			project, err := f.service.readProjectRecord(t.Context(), nativeScope{organization: f.project.OrganizationID, project: projectID})
			if err != nil {
				t.Fatal(err)
			}
			want := historicalStates
			if binding == "" {
				want = []tracker.NativeState{
					{Name: "Triage", Transitions: []string{"Todo", "Backlog", "Cancelled"}},
					{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done", "Cancelled"}},
					{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Blocked", "Human Review", "Merging", "Done", "Cancelled"}},
					{Name: "Blocked", Transitions: []string{"Todo", "In Progress", "Done", "Cancelled"}},
					{Name: "Human Review", Transitions: []string{"Done", "In Progress", "Blocked", "Merging", "Cancelled"}},
					{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Blocked", "Human Review", "In Progress", "Cancelled"}},
					{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
					{Name: "Backlog", Transitions: []string{"Todo", "Cancelled"}},
					{Name: "Cancelled", Terminal: true, Transitions: []string{"Todo"}},
				}
			}
			if !reflect.DeepEqual(project.States, want) {
				t.Fatalf("historical workflow changed = %+v, want %+v", project.States, want)
			}
			current, err := f.service.readProjectRecord(t.Context(), nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
			if err != nil || !reflect.DeepEqual(current.States, nativeFixtureStates()) {
				t.Fatalf("current project affected = %+v, %v", current.States, err)
			}
			if project.States[0].Name != "Triage" || project.States[0].Dispatchable || project.States[0].Terminal || !slices.Contains(project.States[0].Transitions, "Todo") {
				t.Fatalf("migrated workflow = %+v", project.States)
			}
			var dispatchable, terminal bool
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT dispatchable, terminal FROM project_workflow_states WHERE organization_id=$1 AND project_id=$2 AND name='Triage'", f.project.OrganizationID, projectID).Scan(&dispatchable, &terminal); err != nil || dispatchable || terminal {
				t.Fatalf("migration flags = %t, %t, %v", dispatchable, terminal, err)
			}
		})
	}
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
	scope := nativeScope{organization: tracker.OrganizationID(f.service.config.Hosted.OrganizationID), project: tracker.ProjectID(f.project)}
	applyLinkedWebhook(t, nativeFixture{service: f.service, project: tracker.NativeProject{ID: scope.project, OrganizationID: scope.organization}}, "opened", "issues", snapshot, nil)
	drainTestNativeBoard(t, f.service)
}
