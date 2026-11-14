package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func linkedFixture(t *testing.T) nativeFixture {
	t.Helper()
	f := newNativeFixture(t, nil, "", "linked")
	f.service.config.ImportBackend = linkedTestImporter{snapshot: linkedSnapshot()}
	_, repository, _, err := tracker.ParseGitHubIssueURL(linkedSnapshot().URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.Settings.Repository, project.Settings.CheckoutRepository, project.Settings.RepositoryEnabled = repository, repository, true
		project.Settings.Intake = "manual"
		project.States = hubrecords.NativeTriageStates(append(nativeFixtureStates(), tracker.NativeState{Name: "Cancelled", Terminal: true}))
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f nativeFixture) link(t *testing.T, key string) tracker.NativeIssue {
	t.Helper()
	r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, GitHubIssueURL: "github.com/Acme/Orders/issues/12", State: "Todo"})
	requireNativeStatus(t, r, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, r, &issue)
	return issue
}

func TestLinkedIssueCreation(t *testing.T) {
	t.Parallel()
	f := linkedFixture(t)
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), scope.Project)
		if err != nil {
			return err
		}
		project.Settings.Repository, project.Settings.CheckoutRepository, project.Settings.RepositoryEnabled = "", "", false
		states := append([]tracker.NativeState{{Name: "Backlog", Transitions: []string{"Todo"}}}, nativeFixtureStates()...)
		project.States = hubrecords.NativeTriageStates(append(states, tracker.NativeState{Name: "Cancelled", Terminal: true}))
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var first tracker.NativeIssue
	for _, test := range []struct {
		name, state, source, wantState string
		want                           int
	}{
		{"default in unbound project", "", "", "Backlog", http.StatusOK},
		{"linked intake", "", "github.com/Acme/Orders/issues/12", "Triage", http.StatusOK},
		{"default with Triage first", "", "", "Backlog", http.StatusOK},
		{"explicit unlinked Triage", "Triage", "", "", http.StatusUnprocessableEntity},
		{"explicit ordinary state", "Todo", "", "Todo", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.source != "" {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
					project, err := w.ProjectRecord(t.Context(), scope.Project)
					if err != nil {
						return err
					}
					_, repository, _, err := tracker.ParseGitHubIssueURL(test.source)
					if err != nil {
						return err
					}
					project.Settings.Repository, project.Settings.CheckoutRepository, project.Settings.RepositoryEnabled = repository, repository, true
					_, err = w.UpdateProjectRecord(t.Context(), project)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: test.name}, Title: test.name, Body: nativeContractTestBody, State: test.state, GitHubIssueURL: test.source}
			r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
			requireNativeStatus(t, r, test.want)
			if test.want != http.StatusOK {
				requireNativeCode(t, r, test.want, "invalid_request")
				return
			}
			var issue tracker.NativeIssue
			decodeHubResponse(t, r, &issue)
			if issue.State != test.wantState || (issue.LinkedSource != nil) != (test.source != "") {
				t.Fatalf("created issue=%+v", issue)
			}
			if test.source != "" {
				first = issue
			}
			project, err := f.service.readProjectRecord(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			wantStates := []tracker.NativeState{
				{Name: "Triage", Transitions: []string{"Backlog", "Todo", "Cancelled"}},
				{Name: "Backlog", Transitions: []string{"Todo"}},
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Done"}},
				{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
				{Name: "Cancelled", Terminal: true, Transitions: []string{}},
			}
			if !reflect.DeepEqual(project.States, wantStates) {
				t.Fatalf("states=%+v want=%+v", project.States, wantStates)
			}
		})
	}
	if len(first.ExternalReferences) < 1 || first.ExternalReferences[0].Repository != "acme/orders" || first.ExternalReferences[0].Number != 12 {
		t.Fatalf("linked issue lost source reference: %+v", first.ExternalReferences)
	}
	if first.LinkedSource == nil || first.LinkedSource.Status != "complete" || first.LinkedSource.URL != "https://github.com/acme/orders/issues/12" {
		t.Fatalf("linked issue = %#v", first)
	}
	if first.Title != "linked intake" || first.Body != nativeContractTestBody || first.LinkedSource.Snapshot == nil || first.LinkedSource.Snapshot.Title != linkedSnapshot().Title {
		t.Fatalf("source hydration changed supplied fields: %+v", first)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(first.WorkItemID)+"/comments", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var comments tracker.Page[tracker.NativeComment]
	decodeHubResponse(t, response, &comments)
	if len(comments.Items) != 1 || comments.Items[0].Body != "Source discussion" || comments.Items[0].Provenance == nil || comments.Items[0].Provenance.ExternalID != "C_source" {
		t.Fatalf("initial source discussion=%+v", comments.Items)
	}
	backend := f.service.config.ImportBackend
	f.service.config.ImportBackend = scheduledContractImporter(func(context.Context, GitHubImportRequest) (GitHubImportPage, error) {
		t.Error("receipt replay or existing source refetched GitHub")
		return GitHubImportPage{}, errors.New("transport unavailable")
	})
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "linked intake"}, Title: "linked intake", Body: nativeContractTestBody, GitHubIssueURL: "github.com/Acme/Orders/issues/12"})
	requireNativeStatus(t, response, http.StatusOK)
	var replay tracker.NativeIssue
	decodeHubResponse(t, response, &replay)
	if replay.WorkItemID != first.WorkItemID || replay.LinkedSource == nil || replay.LinkedSource.Status != "complete" {
		t.Fatalf("linked replay=%+v", replay)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), scope.Project)
		if err != nil {
			return err
		}
		project.Settings.DuplicateChallenge = true
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 5 {
		wg.Go(func() {
			if got := f.link(t, fmt.Sprint("duplicate-", i)); got.WorkItemID != first.WorkItemID {
				t.Errorf("duplicate issue = %s", got.WorkItemID)
			}
		})
	}
	wg.Wait()
	f.service.config.ImportBackend = backend
	for _, url := range []string{"https://github.com/acme/unrelated/issues/12", "https://github.com/acme/orders/pull/12", "https://example.com/acme/orders/issues/12"} {
		r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: url}, GitHubIssueURL: url, State: "Todo"})
		requireNativeStatus(t, r, http.StatusUnprocessableEntity)
	}
	for _, name := range []string{"fetched fields", "invalid source is atomic", "source discussion bound", "repository changed during fetch"} {
		t.Run(name, func(t *testing.T) {
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "Example linked project "+name)
			snapshot := linkedSnapshot()
			snapshot.URL = "https://github.com/example-org/example-project/issues/12"
			configure := func(repository string) {
				t.Helper()
				if err := other.service.config.TenantRepository.WriteTenant(t.Context(), other.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					project, err := w.ProjectRecord(t.Context(), other.project.ID)
					if err != nil {
						return err
					}
					project.Settings.Repository, project.Settings.CheckoutRepository, project.Settings.RepositoryEnabled = repository, repository, true
					_, err = w.UpdateProjectRecord(t.Context(), project)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			configure("example-org/example-project")
			if name == "invalid source is atomic" {
				snapshot.Comments[0].Body = strings.Repeat("x", 64<<10+1)
			}
			if name == "source discussion bound" {
				comment := snapshot.Comments[0]
				snapshot.Comments = nil
				for i := range 1001 {
					comment.Provenance.ExternalID = fmt.Sprintf("source-comment-%d", i)
					snapshot.Comments = append(snapshot.Comments, comment)
				}
			}
			other.service.config.ImportBackend = scheduledContractImporter(func(ctx context.Context, request GitHubImportRequest) (GitHubImportPage, error) {
				if name == "repository changed during fetch" && request.Stage == "issue" {
					configure("example-org/another-project")
				}
				return (linkedTestImporter{snapshot: snapshot}).FetchImportPage(ctx, request)
			})
			defer func() { other.service.config.ImportBackend = backend }()
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "initial-source"}, GitHubIssueURL: snapshot.URL}
			response := performHubAPIRequest(t, other.service, http.MethodPost, other.base+"/work-items", other.token, request)
			if name == "fetched fields" {
				requireNativeStatus(t, response, http.StatusOK)
				var issue tracker.NativeIssue
				decodeHubResponse(t, response, &issue)
				if issue.WorkItemID == first.WorkItemID || issue.Title != snapshot.Title || issue.Body != snapshot.Body || issue.LinkedSource == nil || issue.LinkedSource.Status != "complete" {
					t.Fatalf("fetched issue=%+v", issue)
				}
				readWorkItem(t, other, issue.WorkItemID, "")
				readWorkItem(t, f, first.WorkItemID, "")
				return
			}
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			if err := other.service.config.TenantRepository.ReadTenant(t.Context(), other.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				items, err := r.WorkItems(t.Context(), other.project.ID, 0, 2)
				if err != nil {
					return err
				}
				if len(items) != 0 {
					t.Fatalf("failed creation persisted items=%+v", items)
				}
				_, err = r.LinkedSourceByURL(t.Context(), other.project.ID, snapshot.URL)
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("failed creation source err=%v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}

}

func linkedSnapshot() tracker.GitHubIssueSnapshot {
	now := time.Now().UTC()
	prov := tracker.Provenance{Provider: "github", ExternalID: "I_source", AuthorID: "author", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute), ObservedAt: now}
	comment := prov
	comment.ExternalID = "C_source"
	return tracker.GitHubIssueSnapshot{URL: "https://github.com/acme/orders/issues/12", Title: "Fetched source title", Body: "Full source body" + issueContractTestSections, Provenance: prov, Comments: []tracker.GitHubIssueComment{{Body: "Source discussion", Provenance: comment}}}
}

func moveLinkedFixtureToTodo(t *testing.T, f nativeFixture, item tracker.NativeWorkItemID) {
	t.Helper()
	previous := readWorkItem(t, f, item, "")
	moved := monthlyBudgetState(t, f, item, "Todo")
	document, err := searchstore.NewContent(f.service.runDetailStore()).Prepare(t.Context(), searchstore.Document{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: item, ID: "issue:" + string(item), SourceID: string(item), Kind: "issue", Scope: "issue", Revision: int64(moved.Revision), UpdatedAt: moved.UpdatedAt}, searchstore.Text{Title: moved.Title, Body: moved.Body, Labels: moved.Labels})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.PutSearch(t.Context(), document, int64(previous.Revision))
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLinkedIssueIntakeAtomicAndNativeEdits(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"unedited", "native edits", "title edit", "body edit", "edit back to placeholder", "incomplete source contract"} {
		projections := []string{"typed", "unknown", "backfilled", "opaque differs"}
		if mode == "incomplete source contract" {
			projections = []string{"typed"}
		}
		for _, projection := range projections {
			t.Run(mode+"/"+projection, func(t *testing.T) {
				f := newDefaultNativeFixture(t, Config{})
				snapshot := linkedSnapshot()
				snapshot.URL = "https://github.com/example-org/example-project/issues/12"
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					project, err := w.ProjectRecord(t.Context(), f.project.ID)
					if err != nil {
						return err
					}
					project.Settings.Repository, project.Settings.CheckoutRepository, project.Settings.RepositoryEnabled = "example-org/example-project", "example-org/example-project", true
					_, err = w.UpdateProjectRecord(t.Context(), project)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				titleOwned := mode != "unedited" && mode != "incomplete source contract" && mode != "body edit"
				bodyOwned := mode != "unedited" && mode != "incomplete source contract" && mode != "title edit"
				var issue tracker.NativeIssue
				if projection == "typed" {
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "linked-create"}, GitHubIssueURL: snapshot.URL, State: "Todo"})
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &issue)
					if titleOwned || bodyOwned {
						title, body := issue.Title, issue.Body
						if body == "" {
							body = nativeContractTestBody
						}
						if titleOwned && mode != "edit back to placeholder" {
							title = "Native title"
						}
						if bodyOwned && mode != "edit back to placeholder" {
							body = "Native body" + issueContractTestSections
						}
						update := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "edit"}, ExpectedRevision: issue.Revision}
						if titleOwned {
							update.Title = &title
						}
						if bodyOwned {
							update.Body = &body
						}
						response = performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token, update)
						requireNativeStatus(t, response, http.StatusOK)
					} else if mode == "unedited" {
						response = performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "label-only"}, ExpectedRevision: issue.Revision, Labels: &[]string{"example"}})
						requireNativeStatus(t, response, http.StatusOK)
					}
					moveLinkedFixtureToTodo(t, f, issue.WorkItemID)
				} else {
					source := coreWorkItemNeutralFixture(t, f.project.OrganizationID, f.project.ID, false)
					linked := newCoreNeutralFixture(t, f.project.OrganizationID, "linked_issue_sources")
					source.Tables["linked_issue_sources"] = linked.Tables["linked_issue_sources"]
					source.Records["native_comments"] = nil
					versions := source.Records["collaboration_versions"][:0]
					for _, row := range source.Records["collaboration_versions"] {
						if strings.HasPrefix(string(row.Values["record_id"].Data), "wi_core_import") {
							versions = append(versions, row)
						}
					}
					source.Records["collaboration_versions"] = versions
					for itemIndex, suffix := range []string{"", "_second"} {
						item := tracker.NativeWorkItemID("wi_core_import" + suffix)
						url := fmt.Sprintf("https://github.com/example-org/example-project/issues/%d", 12+itemIndex)
						source.Add("linked_issue_sources", map[string]any{"work_item_id": item, "source_url": url, "title_supplied": false, "body_supplied": false, "snapshot_json": nil})
						source.Set("queue_entries", map[string]string{"issue_id": strconv.Itoa(41 + itemIndex*2)}, map[string]any{"scope": string(f.project.ID)})
						title, body := "Updated title", nativeContractTestBody
						if titleOwned && mode != "edit back to placeholder" {
							title = "Native title"
						}
						if bodyOwned && mode != "edit back to placeholder" {
							body = "Native body" + issueContractTestSections
						}
						at := parseTestTime(t, "2026-10-02T00:00:00Z")
						current := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: item, Number: 1 + itemIndex*2, Revision: 2, Profile: "native"}, Title: title, Body: body, State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, Labels: []string{"needs-review"}, Assignees: []string{"example-assignee"}, CreatedAt: parseTestTime(t, "2026-10-01T00:00:00Z"), UpdatedAt: at, LastActivityAt: at, LinkedSource: &tracker.LinkedIssueSource{URL: url, Status: "pending"}}
						raw, err := json.Marshal(current)
						if err != nil {
							t.Fatal(err)
						}
						keys, values, err := neutralimport.VersionObject(raw)
						if err != nil {
							t.Fatal(err)
						}
						raw, err = neutralimport.VersionObjectBytes(keys, values)
						if err != nil {
							t.Fatal(err)
						}
						source.Set("collaboration_versions", map[string]string{"record_id": string(item), "revision": "2"}, map[string]any{"encoding": "snapshot", "record_json": string(raw)})
						source.Set("issues", map[string]string{"native_id": string(item)}, map[string]any{"title": title, "body": body})
						fields := []string{"labels"}
						if titleOwned {
							fields = append(fields, "title")
						}
						if bodyOwned {
							fields = append(fields, "body")
						}
						data, _ := json.Marshal(tracker.CollaborationData{Revision: 2, Fields: fields})
						updates := map[string]any{"data_json": string(data)}
						if projection == "backfilled" || projection == "opaque differs" {
							typedTitle, typedBody := titleOwned, bodyOwned
							if projection == "opaque differs" {
								typedTitle, typedBody = !typedTitle, !typedBody
							}
							updates["page_projection_ready"], updates["edited_title"], updates["edited_body"] = true, typedTitle, typedBody
						}
						source.Set("collaboration_events", map[string]string{"work_item_id": string(item), "type": "issue.edited"}, updates)
					}
					input := coreimport.CoreWorkItemImportSource{BundlePath: source.Write(), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Objects: f.service.runDetailStore()}
					if count, err := coreimport.ImportCoreWorkItems(t.Context(), f.service.database.postgres, input); err != nil || count != 2 {
						t.Fatalf("historical ownership import count=%d error=%v", count, err)
					}
					issue = readWorkItem(t, f, "wi_core_import", "")
					if projection == "opaque differs" {
						titleOwned, bodyOwned = !titleOwned, !bodyOwned
					}
				}
				issue = readWorkItem(t, f, issue.WorkItemID, "")
				path := f.base + "/work-items/" + string(issue.WorkItemID)
				title, body := issue.Title, issue.Body
				assertOwnership := func() {
					t.Helper()
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						linked, err := r.LinkedSource(t.Context(), f.project.ID, issue.WorkItemID)
						if err != nil {
							return err
						}
						if linked.TitleOwned != titleOwned || linked.BodyOwned != bodyOwned {
							t.Fatalf("ownership title=%t body=%t want title=%t body=%t", linked.TitleOwned, linked.BodyOwned, titleOwned, bodyOwned)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				assertOwnership()
				if titleOwned && projection == "typed" {
					current := readWorkItem(t, f, issue.WorkItemID, "")
					same := current.Title
					response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "resave-owned-title"}, ExpectedRevision: current.Revision, Title: &same})
					requireNativeStatus(t, response, http.StatusOK)
					if after := readWorkItem(t, f, issue.WorkItemID, ""); after.Revision != current.Revision || after.Title != same {
						t.Fatalf("no-op edit of an owned linked title changed the issue: revision %d -> %d", current.Revision, after.Revision)
					}
					assertOwnership()
				}
				commentCount := func() int {
					t.Helper()
					count := 0
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						rows, err := r.Comments(t.Context(), f.project.ID, issue.WorkItemID, 0, 100)
						count = len(rows)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					return count
				}
				runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
				runner.enroll(t)
				worker := runner.redemption.Credential
				descriptor := hubTestPolicy()
				approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
				heartbeat := map[string]any{"display_name": runner.redemption.DisplayName, "capacity": 2, "version": "test", "backend_isolation": runner.redemption.BackendIsolation, "capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: f.service.config.Clock()}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", worker, heartbeat), http.StatusOK)
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					return w.ReplaceRunnerCheckoutReport(t.Context(), runnerstore.CheckoutReport{OrganizationID: f.project.OrganizationID, RunnerID: string(runner.binding.RunnerID), ProjectID: f.project.ID, Repository: "example-org/example-project", ReportedAt: f.service.config.Clock()})
				}); err != nil {
					t.Fatal(err)
				}
				r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{WorkItemID: issue.WorkItemID, MachineID: runner.binding.MachineID, SessionID: "session", PolicyID: descriptor.ID, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
				requireNativeStatus(t, r, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, r, &lease)
				if mode == "incomplete source contract" {
					snapshot.Body = "## Roadmap Summary\nRepair the failed build."
					request := tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: "incomplete-contract", LeaseID: lease.ID, FencingToken: lease.FencingToken}, Snapshot: snapshot}
					response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/source-intake", worker, request)
					requireNativeStatus(t, response, http.StatusUnprocessableEntity)
					preserved := readWorkItem(t, f, issue.WorkItemID, "")
					if preserved.LinkedSource.Status != "pending" || preserved.Body != issue.Body {
						t.Fatalf("refused intake persisted: %+v", preserved)
					}
					return
				}
				request := tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: "intake", LeaseID: lease.ID, FencingToken: lease.FencingToken}, Snapshot: snapshot}
				invalid := request
				invalid.IdempotencyKey = "invalid"
				invalid.Snapshot.Comments = append([]tracker.GitHubIssueComment{}, snapshot.Comments...)
				invalid.Snapshot.Comments = append(invalid.Snapshot.Comments, tracker.GitHubIssueComment{})
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/source-intake", worker, invalid), http.StatusUnprocessableEntity)
				if count := commentCount(); count != 0 {
					t.Fatalf("partial comments = %d", count)
				}
				stale := request
				stale.IdempotencyKey, stale.FencingToken = "stale", lease.FencingToken+1
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/source-intake", worker, stale), http.StatusConflict)
				var wg sync.WaitGroup
				responses := make(chan *httptest.ResponseRecorder, 3)
				for i := range 3 {
					wg.Go(func() {
						next := request
						next.IdempotencyKey = fmt.Sprint("retry-", i)
						responses <- performHubAPIRequest(t, f.service, http.MethodPost, path+"/source-intake", worker, next)
					})
				}
				wg.Wait()
				close(responses)
				for r := range responses {
					requireNativeStatus(t, r, http.StatusOK)
				}
				r = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
				requireNativeStatus(t, r, http.StatusOK)
				var hydrated tracker.NativeIssue
				decodeHubResponse(t, r, &hydrated)
				if !titleOwned {
					title = snapshot.Title
				}
				if !bodyOwned {
					body = snapshot.Body
				}
				if hydrated.Title != title || hydrated.Body != body || hydrated.LinkedSource.Status != "complete" || hydrated.LinkedSource.Snapshot.Body != snapshot.Body || hydrated.Provenance.ExternalID != "I_source" {
					t.Fatalf("hydrated = %#v", hydrated)
				}
				if count := commentCount(); count != 1 {
					t.Fatalf("comments = %d", count)
				}
				assertOwnership()
				request.IdempotencyKey = "later-source-edit"
				request.Snapshot.Title, request.Snapshot.Body = "Updated GitHub title", "Updated GitHub body"+issueContractTestSections
				request.Snapshot.Provenance.UpdatedAt = snapshot.Provenance.UpdatedAt.Add(time.Second)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/source-intake", worker, request), http.StatusOK)
				r = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
				requireNativeStatus(t, r, http.StatusOK)
				decodeHubResponse(t, r, &hydrated)
				if !titleOwned {
					title = request.Snapshot.Title
				}
				if !bodyOwned {
					body = request.Snapshot.Body
				}
				if hydrated.Title != title || hydrated.Body != body || hydrated.LinkedSource.Snapshot.Body != request.Snapshot.Body {
					t.Fatalf("later GitHub edit = %#v", hydrated)
				}
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					rows, err := r.Events(t.Context(), f.project.ID, issue.WorkItemID, 0, 100)
					if err != nil {
						return err
					}
					count := 0
					for _, row := range rows {
						if row.Type == "github.imported" {
							count++
						}
					}
					if count != 2 {
						t.Fatalf("import events = %d", count)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
