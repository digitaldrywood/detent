package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type batchFixture struct {
	commandToken  string
	sessionCookie string
	provider      *hostedSecurityProvider
	nativeFixture
	runner   runnerFixture
	details  *memoryRunDetails
	batch    *tracker.GitHubBatch
	sequence int
	backend  *batchSourceFixture
	clock    *leaseTestClock
}

type batchSourceFixture struct {
	mu             sync.Mutex
	pages          map[string]tracker.GitHubDiscoveryPage
	snapshots      map[int]tracker.GitHubIssueSnapshot
	failures       map[int]error
	discoveryError error
	discoveryGaps  bool
	partial        map[int]bool
	calls          []GitHubImportRequest
	beforeFetch    func(GitHubImportRequest)
}

func (backend *batchSourceFixture) FetchImportPage(ctx context.Context, request GitHubImportRequest) (GitHubImportPage, error) {
	backend.mu.Lock()
	backend.calls = append(backend.calls, request)
	hook := backend.beforeFetch
	backend.mu.Unlock()
	if hook != nil {
		hook(request)
	}
	if err := ctx.Err(); err != nil {
		return GitHubImportPage{}, err
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if request.Stage == "batch" {
		if backend.discoveryError != nil {
			return GitHubImportPage{}, backend.discoveryError
		}
		page, ok := backend.pages[request.Discovery.Cursor]
		if !ok {
			return GitHubImportPage{}, errors.New("unexpected discovery cursor")
		}
		if backend.discoveryGaps {
			return GitHubImportPage{BatchPage: &page, Gaps: []string{"partial page"}}, nil
		}
		return GitHubImportPage{BatchPage: &page}, nil
	}
	if backend.partial[request.IssueNumber] {
		return GitHubImportPage{Gaps: []string{"partial discussion"}}, nil
	}
	if err := backend.failures[request.IssueNumber]; err != nil {
		return GitHubImportPage{}, err
	}
	snapshot, ok := backend.snapshots[request.IssueNumber]
	if !ok {
		return GitHubImportPage{}, errors.New("source context unavailable")
	}
	switch request.Stage {
	case "issue":
		return GitHubImportPage{Issue: &IssueSource{Number: request.IssueNumber, URL: snapshot.URL, NodeID: snapshot.Provenance.ExternalID, Title: snapshot.Title, Body: snapshot.Body, AuthorID: snapshot.Provenance.AuthorID, CreatedAt: snapshot.Provenance.CreatedAt, UpdatedAt: snapshot.Provenance.UpdatedAt}}, nil
	case "comments":
		start := 0
		if request.Cursor != "" {
			var err error
			start, err = strconv.Atoi(request.Cursor)
			if err != nil {
				return GitHubImportPage{}, err
			}
		}
		page := GitHubImportPage{}
		for _, comment := range snapshot.Comments[start:min(start+30, len(snapshot.Comments))] {
			page.Records = append(page.Records, GitHubImportRecord{Kind: "comment", Body: comment.Body, Provenance: comment.Provenance})
		}
		if start+30 < len(snapshot.Comments) {
			page.NextCursor = strconv.Itoa(start + 30)
		}
		return page, nil
	}
	return GitHubImportPage{}, errors.New("unexpected import stage")
}

func newBatchFixture(t *testing.T) *batchFixture {
	t.Helper()
	clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
	details := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: details, Clock: clock.Now})
	f := newBatchFixtureForProject(t, newNativeFixture(t, service, "", "Example batch"))
	f.clock, f.details = clock, details
	return f
}

func newBatchFixtureForProject(t *testing.T, f nativeFixture) *batchFixture {
	t.Helper()
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
		project, err := writer.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.Settings.RepositoryEnabled = true
		project.Settings.Repository, project.Settings.CheckoutRepository, project.Settings.Intake = "example-org/example-project", "example-org/example-project", "manual"
		project.States = nativeTriageStates(append(project.States, tracker.NativeState{Name: "Backlog", OperatorOnly: true, Transitions: []string{"Todo"}}, tracker.NativeState{Name: "Blocked", Transitions: []string{"Todo", "Backlog"}}))
		_, err = writer.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	backend := &batchSourceFixture{pages: map[string]tracker.GitHubDiscoveryPage{}, snapshots: map[int]tracker.GitHubIssueSnapshot{}, failures: map[int]error{}, partial: map[int]bool{}}
	f.service.config.ImportBackend = backend
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	runner.enroll(t)
	fixture := &batchFixture{nativeFixture: f, runner: runner, backend: backend}
	fixture.heartbeat(t)
	return fixture
}

func (f *batchFixture) scope(t *testing.T) hubscope.NativeScope {
	t.Helper()
	credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	return hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
}

func (f *batchFixture) refresh(t *testing.T) {
	t.Helper()
	batch, err := f.service.loadGitHubBatchRecord(t.Context(), f.scope(t))
	if err != nil {
		t.Fatal(err)
	}
	f.batch = batch
}

func (f *batchFixture) advance(t *testing.T) error {
	t.Helper()
	scope, err := f.service.gitHubIntegrationScope(t.Context(), f.project.OrganizationID, f.project.ID)
	if err == nil {
		err = f.service.advanceGitHubBatchRecord(t.Context(), scope)
	}
	f.refresh(t)
	return err
}

func (f *batchFixture) command(t *testing.T, request tracker.GitHubBatchCommand, status int) {
	t.Helper()
	f.sequence++
	request.IdempotencyKey = fmt.Sprint("command-", f.sequence)
	if f.batch != nil {
		request.Revision = f.batch.Revision
	}
	token := f.commandToken
	if token == "" {
		token = testHubAdminToken
	}
	var response *httptest.ResponseRecorder
	if f.sessionCookie != "" {
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, f.base+"/onboarding/issue-intake", bytes.NewReader(raw)).WithContext(t.Context())
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		req.Header.Set("X-CSRF-Token", hostedCSRF(f.sessionCookie))
		req.AddCookie(&http.Cookie{Name: hostedCookie, Value: f.sessionCookie})
		response = httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, req)
	} else {
		response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/onboarding/issue-intake", token, request)
	}
	requireNativeStatus(t, response, status)
	if status == http.StatusOK {
		var view githubBatchView
		decodeHubResponse(t, response, &view)
		f.batch = view.Batch
	}
}

func (f *batchFixture) heartbeat(t *testing.T) *tracker.GitHubBatchTask {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(f.runner.binding.MachineID)+"/heartbeat", f.runner.redemption.Credential, map[string]any{"version": "test", "capacity": 1, "display_name": "runner", "checkout_repository": "example-org/example-project", "local_checks": runnerauth.LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"codex"}}})
	requireNativeStatus(t, response, http.StatusOK)
	var routing runnerauth.RoutingSnapshot
	decodeHubResponse(t, response, &routing)
	return routing.GitHubIntake
}

func previewIssue(number int, closed bool) tracker.GitHubIssuePreview {
	return tracker.GitHubIssuePreview{Number: number, ID: fmt.Sprint("I_", number), URL: fmt.Sprintf("https://github.com/example-org/example-project/issues/%d", number), Title: fmt.Sprint("Source ", number), Body: "Preview", Closed: closed, Labels: []string{"enhancement"}}
}
func batchSnapshot(number int) tracker.GitHubIssueSnapshot {
	snapshot := linkedSnapshot()
	snapshot.URL = previewIssue(number, false).URL
	snapshot.Provenance.ExternalID = previewIssue(number, false).ID
	snapshot.Comments[0].Provenance.ExternalID = fmt.Sprint("C_", number)
	return snapshot
}

func (f *batchFixture) preview(t *testing.T, closed bool) {
	t.Helper()
	f.backend.pages[""] = tracker.GitHubDiscoveryPage{Total: 3, Issues: []tracker.GitHubIssuePreview{previewIssue(12, false), previewIssue(13, closed), previewIssue(14, false)}}
	for _, number := range []int{12, 13, 14} {
		f.backend.snapshots[number] = batchSnapshot(number)
	}
	f.command(t, tracker.GitHubBatchCommand{Action: "discover", IncludeClosed: closed}, http.StatusOK)
	if !f.batch.HubOwned || f.heartbeat(t) != nil {
		t.Fatalf("batch escaped Hub ownership: %+v", f.batch)
	}
	if err := f.advance(t); err != nil {
		t.Fatal(err)
	}
	if f.batch.Status != "preview" || len(f.batch.Page.Issues) != 3 {
		t.Fatalf("preview=%+v", f.batch)
	}
}

func (f *batchFixture) issue(t *testing.T, id tracker.NativeWorkItemID) tracker.NativeIssue {
	t.Helper()
	row, version, err := f.service.readWorkItemRecord(t.Context(), f.scope(t), string(id))
	if err != nil {
		t.Fatal(err)
	}
	issue, err := f.service.hydrateWorkItemRecord(t.Context(), row, version)
	if err != nil {
		t.Fatal(err)
	}
	return issue
}

func (f *batchFixture) itemCount(t *testing.T) int {
	t.Helper()
	count := 0
	var after int64
	for {
		var rows []corestore.WorkItem
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			rows, err = reader.WorkItems(t.Context(), f.project.ID, after, 200)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		count += len(rows)
		if len(rows) < 200 {
			return count
		}
		after = rows[len(rows)-1].ID
	}
}

func (f *batchFixture) commentCount(t *testing.T, id tracker.NativeWorkItemID) int {
	t.Helper()
	var rows []corestore.Comment
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		rows, err = reader.SelectComments(t.Context(), f.project.ID, id, 0, false, 200)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func (f *batchFixture) save(t *testing.T) {
	t.Helper()
	scope := f.scope(t)
	current, err := f.service.loadGitHubBatchState(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	scope.GitHubIntakeRevision = &hubscope.GitHubIntakeRevision{Issuer: current.Issuer, FallbackRunner: current.FallbackRunnerID, RequireAdminKey: current.RequireAdminKey}
	if err := f.service.saveGitHubBatchRecord(t.Context(), scope, f.batch); err != nil {
		t.Fatal(err)
	}
}

func (f *batchFixture) restart(t *testing.T) {
	t.Helper()
	service := tfConvReopenConversationService(t, f.service)
	f.service = service
	f.runner.service = service
	if err := service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	f.refresh(t)
}

func TestGitHubBatchIntakeRetryAndNativeOwnership(t *testing.T) {
	for _, test := range []struct {
		name                                                         string
		closed, rateLimit, mcp, importDisabled, integration, partial bool
		projectLane                                                  string
		unmapped                                                     bool
	}{
		{name: "partial import and native edit"}, {name: "429 after first import", rateLimit: true}, {name: "explicit mixed open and closed history", closed: true}, {name: "MCP bounded intake receipts and authority", mcp: true}, {name: "manual import disabled", importDisabled: true}, {name: "corrected project mapping for precreated issue", projectLane: "Backlog"}, {name: "corrected dispatchable project mapping requires confirmation", projectLane: "Todo"}, {name: "corrected unmapped project status uses destination", projectLane: "Backlog", unmapped: true}, {name: "recorded integration actor destination correction", projectLane: "Backlog", integration: true}, {name: "partial source discussion", partial: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newBatchFixture(t)
			if test.integration {
				f = newHostedBatchFixture(t)
				f.commandToken = seedBatchServiceKey(t, f)
			}
			f.preview(t, test.closed)
			integrationActor := ""
			if test.integration {
				integrationActor = f.batch.RequestedBy.PrincipalID
				if f.batch.RequestedBy.Kind != "integration" {
					t.Fatalf("service key actor=%+v", f.batch.RequestedBy)
				}
			}
			if test.projectLane != "" {
				f.batch.Discovery.ProjectV2 = &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Blocked"}}
				for index := range f.batch.Page.Issues {
					f.batch.Page.Issues[index].ProjectStatus = "Ready"
				}
				f.save(t)
			}
			if test.importDisabled {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
					project, err := writer.ProjectRecord(t.Context(), f.project.ID)
					if err != nil {
						return err
					}
					project.Settings.Intake = "disabled"
					_, err = writer.UpdateProjectRecord(t.Context(), project)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}, Destination: "Backlog"}, http.StatusUnprocessableEntity)
				if f.itemCount(t) != 0 || f.heartbeat(t) != nil {
					t.Fatal("disabled import created issues or assigned intake")
				}
				return
			}
			if test.mcp {
				exerciseBatchMCP(t, f)
				return
			}
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12, 12, 13, 14}, Destination: "Todo"}, http.StatusUnprocessableEntity)
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12, 12, 13, 14}, Destination: "Backlog"}, http.StatusOK)
			if len(f.batch.Items) != 3 || f.batch.Destination != "Backlog" {
				t.Fatalf("selection=%+v", f.batch)
			}
			f.backend.failures[13] = githubconnector.ErrNotFound
			if test.partial {
				delete(f.backend.failures, 13)
				f.backend.partial[13] = true
			}
			if test.rateLimit {
				f.backend.failures[13] = &githubconnector.StatusError{StatusCode: 429, RetryAfter: time.Hour, Err: githubconnector.ErrRateLimited}
			}
			if err := f.advance(t); err != nil {
				t.Fatal(err)
			}
			if f.batch.Items[0].Status != "completed" || f.batch.Items[1].Status != "failed" || f.batch.Items[1].WorkItemID == "" || f.batch.Status != "finished" {
				t.Fatalf("partial progress=%+v", f.batch)
			}
			if want := "completed"; test.rateLimit {
				if f.batch.Items[2].Status != "failed" || f.batch.Items[2].RetryAt == "" {
					t.Fatalf("throttle lost remaining item: %+v", f.batch.Items[2])
				}
			} else if f.batch.Items[2].Status != want {
				t.Fatalf("later valid item did not progress: %+v", f.batch.Items[2])
			}
			revision := f.batch.Revision
			f.restart(t)
			if f.batch.Revision != revision || f.batch.Items[0].Status != "completed" {
				t.Fatalf("lost checkpoints after restart: %+v", f.batch)
			}
			id := f.batch.Items[1].WorkItemID
			native := f.issue(t, id)
			title, body := "Native task title", "Native body during retry"
			if test.projectLane == "Todo" {
				body += issueContractTestSections
			}
			response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(id), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "native-edit"}, ExpectedRevision: native.Revision, Title: &title, Body: &body})
			requireNativeStatus(t, response, http.StatusOK)
			if test.rateLimit {
				f.command(t, tracker.GitHubBatchCommand{Action: "retry"}, http.StatusUnprocessableEntity)
				f.clock.Advance(2 * time.Hour)
			}
			retry := tracker.GitHubBatchCommand{Action: "retry"}
			if test.projectLane != "" {
				retry.ProjectV2 = &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": test.projectLane}}
				if test.unmapped {
					retry.ProjectV2.StatusLanes = map[string]string{"Other": "Blocked"}
				}
				if test.projectLane == "Todo" {
					f.command(t, retry, http.StatusUnprocessableEntity)
					if got := f.issue(t, id); got.State != "Blocked" || f.batch.Items[1].Status != "failed" {
						t.Fatalf("unconfirmed retry changed issue: %+v", got)
					}
					retry.AllowDispatch = true
				}
			}
			delete(f.backend.failures, 13)
			delete(f.backend.partial, 13)
			f.command(t, retry, http.StatusOK)
			if test.integration {
				scope, err := f.service.gitHubIntegrationScope(t.Context(), f.project.OrganizationID, f.project.ID)
				if err != nil {
					t.Fatal(err)
				}
				scope.SourceActor = f.batch.RequestedBy
				state, err := f.service.loadGitHubBatchState(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				scope, err = f.service.bindGitHubBatchIssuer(t.Context(), scope, state)
				if err != nil {
					t.Fatal(err)
				}
				original := f.issue(t, id)
				for _, fault := range []string{"missing batch", "wrong batch", "stale batch", "wrong repository", "wrong actor", "completed item", "unapproved destination"} {
					invalid := scope
					fence := *scope.GitHubIntakeRevision
					invalid.GitHubIntakeRevision = &fence
					target, destination := id, "Backlog"
					switch fault {
					case "missing batch":
						invalid.GitHubIntakeRevision = nil
					case "wrong batch":
						invalid.GitHubIntakeRevision.BatchID = "other-batch"
					case "stale batch":
						invalid.GitHubIntakeRevision.Revision--
					case "wrong repository":
						invalid.IntegrationRepository = "other/repository"
					case "wrong actor":
						invalid.SourceActor = &tracker.Actor{Kind: "integration", PrincipalID: "other-key"}
					case "completed item":
						target = f.batch.Items[0].WorkItemID
					case "unapproved destination":
						destination = "Todo"
					}
					_, err := f.service.transitionNativeIssueTenant(t.Context(), invalid, string(target), tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "denied-" + strings.ReplaceAll(fault, " ", "-")}, ExpectedRevision: original.Revision, State: destination, Reason: "user_requested"})
					if err == nil {
						t.Fatalf("invalid integration routing accepted: %s", fault)
					}
					if after := f.issue(t, id); after.Revision != original.Revision || after.State != original.State {
						t.Fatalf("denied routing changed issue: fault=%s issue=%+v", fault, after)
					}
				}
			}
			if err := f.advance(t); err != nil {
				t.Fatal(err)
			}
			native = f.issue(t, id)
			wantLane := "Backlog"
			if test.projectLane != "" {
				wantLane = test.projectLane
				for _, index := range []int{0, 2} {
					completed := f.issue(t, f.batch.Items[index].WorkItemID)
					if completed.State != "Blocked" || f.batch.Items[index].Status != "completed" {
						t.Fatalf("retry remapped completed issue: %+v", completed)
					}
				}
				unmapped := f.batch.Items[1].UnmappedProjectStatus
				if (unmapped != nil) != test.unmapped || test.unmapped && *unmapped != "Ready" {
					t.Fatalf("unmapped status=%+v", f.batch.Items[1])
				}
			}
			if native.WorkItemID != id || native.Title != title || native.Body != body || native.State != wantLane || native.LinkedSource.Status != "complete" {
				t.Fatalf("native ownership=%+v", native)
			}
			if test.integration {
				var events []eventstore.Event
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					var err error
					events, err = reader.Events(t.Context(), f.project.ID, id, 0, 100)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if !slices.ContainsFunc(events, func(event eventstore.Event) bool {
					return event.Type == "workflow.transitioned" && event.ActorKind == "integration" && event.ActorPrincipalID == integrationActor
				}) {
					t.Fatalf("routing rewrote recorded actor: %+v", events)
				}
			}
			if comments, count := f.commentCount(t, id), f.itemCount(t); comments != 1 || count != 3 {
				t.Fatalf("comments=%d issues=%d", comments, count)
			}
			for range 3 {
				if f.heartbeat(t) != nil {
					t.Fatal("completed Hub intake assigned runner work")
				}
			}
			f.preview(t, test.closed)
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12, 13, 14}, Destination: "Todo", AllowDispatch: true}, http.StatusOK)
			if err := f.advance(t); err != nil {
				t.Fatal(err)
			}
			for _, item := range f.batch.Items {
				if item.Status != "skipped" {
					t.Fatalf("duplicate=%+v", item)
				}
			}
		})
	}
}

func exerciseBatchMCP(t *testing.T, f *batchFixture) {
	t.Helper()
	captureds := make(chan context.Context, 1)
	f.service.echo.GET("/api/v2/organizations/:organization/mcp-batch-fixture", func(c echo.Context) error { captureds <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
	path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/mcp-batch-fixture"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, testHubAdminToken, nil), http.StatusOK)
	captured := <-captureds
	connect := func(id string) context.Context {
		ctx := operatortool.BindConnection(captured, id, "batch fixture")
		if err := (hostedOperatorExecutor{f.service}).OpenConnection(ctx); err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	ctx := connect("batch-tools")
	e := hubProjectExecutor{f.service}
	f.batch.Error = "provider credential-secret"
	f.batch.Page.Issues[0].Body = strings.Repeat("preview body", 30000)
	f.save(t)
	input := operatortool.GitHubBatchInput{Revision: f.batch.Revision, Action: "apply", Numbers: []int{12, 13, 14}, Destination: "Backlog"}
	input.Destination = "Todo"
	var refusal *operatortool.RequestError
	if _, err := e.Execute(ctx, projectCall(t, "command_git_hub_batch", string(f.project.ID), "dispatch-preview", input)); !errors.As(err, &refusal) || refusal.Code != "invalid_request" || refusal.Message != "Select a destination and explicitly confirm any dispatchable lane" {
		t.Fatalf("unconfirmed dispatch=%v", err)
	}
	input.Destination = "Backlog"
	call := projectCall(t, "command_git_hub_batch", string(f.project.ID), "batch-apply", input)
	a := projectAction(t, e, ctx, call)
	if a.Status != chatpkg.ActionSucceeded || strings.Contains(a.Result, "credential-secret") || strings.Contains(a.Result, `"body"`) {
		t.Fatalf("batch apply=%+v", a)
	}
	stored, err := f.service.loadGitHubBatchState(t.Context(), f.scope(t))
	if err != nil || !stored.RequireAdminKey {
		t.Fatalf("MCP intake lost existing admin-key authority: %+v error=%v", stored, err)
	}

	if replay := projectAction(t, e, connect("batch-reconnected"), call); replay.Result != a.Result {
		t.Fatalf("batch replay=%+v", replay)
	}
	input.Numbers = []int{12}
	if _, err := e.Execute(connect("batch-conflict"), projectCall(t, "command_git_hub_batch", string(f.project.ID), "batch-apply", input)); !errors.Is(err, mutation.ErrConflict) {
		t.Fatalf("changed batch replay=%v", err)
	}
	read := operatortool.Call{Name: "get_git_hub_batch", Arguments: json.RawMessage(`{"project_id":"` + string(f.project.ID) + `","limit":1}`)}
	result, err := (hostedOperatorExecutor{f.service}).Execute(ctx, read)
	if err != nil || strings.Contains(string(result.Content), "credential-secret") || strings.Contains(string(result.Content), `"body"`) || !strings.Contains(string(result.Content), `"next_cursor":"1"`) {
		t.Fatalf("batch page=%s %v", result.Content, err)
	}
	var envelope struct {
		Data projectBatchPage `json:"data"`
	}
	if json.Unmarshal(result.Content, &envelope) != nil || len(envelope.Data.Batch.Items) != 1 || len(envelope.Data.Batch.Page.Issues) != 1 {
		t.Fatalf("batch bounds=%s", result.Content)
	}
	preview := envelope.Data.Batch.Page.Issues[0]
	if preview.Number != f.batch.Page.Issues[0].Number || preview.Title != f.batch.Page.Issues[0].Title || preview.Status != "open" {
		t.Fatalf("batch preview=%+v", preview)
	}
	storedBatch, err := f.service.loadGitHubBatchRecord(t.Context(), f.scope(t))
	if err != nil || storedBatch.Page.Issues[0].Body != strings.Repeat("preview body", 30000) {
		t.Fatalf("stored batch preview body changed: %v", err)
	}
	if _, err := e.Execute(ctx, projectCall(t, "command_git_hub_batch", string(f.project.ID), "stale-batch", input)); !errors.Is(err, mutation.ErrConflict) {
		t.Fatalf("stale batch=%v", err)
	}
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-batch")
	read.Arguments = json.RawMessage(`{"project_id":"` + string(other.project.ID) + `"}`)
	result, err = e.Execute(ctx, read)
	if err != nil || strings.Contains(string(result.Content), f.batch.ID) {
		t.Fatalf("cross-project batch=%s %v", result.Content, err)
	}
}

func TestGitHubBatchDiscoveryFailureAndSourceValidation(t *testing.T) {
	f := newBatchFixture(t)
	f.backend.discoveryError = githubconnector.ErrNotFound
	f.command(t, tracker.GitHubBatchCommand{Action: "discover", Labels: []string{"enhancement"}}, http.StatusOK)
	if err := f.advance(t); err == nil || f.batch.Status != "discovery_failed" || f.batch.Error == "" {
		t.Fatalf("discovery failure=%+v error=%v", f.batch, err)
	}
	if f.heartbeat(t) != nil {
		t.Fatal("private access failure assigned runner polling")
	}
	f.backend.discoveryError = nil
	f.command(t, tracker.GitHubBatchCommand{Action: "retry"}, http.StatusOK)
	for _, test := range []struct {
		name string
		page tracker.GitHubDiscoveryPage
	}{
		{"closed source", tracker.GitHubDiscoveryPage{Total: 1, Issues: []tracker.GitHubIssuePreview{previewIssue(12, true)}}},
		{"duplicate source", tracker.GitHubDiscoveryPage{Total: 2, Issues: []tracker.GitHubIssuePreview{previewIssue(12, false), previewIssue(12, false)}}},
		{"stationary cursor", tracker.GitHubDiscoveryPage{Total: 2, NextCursor: "", Issues: []tracker.GitHubIssuePreview{{ID: "I_12", URL: "https://github.com/other/repository/issues/12", Number: 12, Title: "Foreign"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.backend.pages[""] = test.page
			if err := f.advance(t); err == nil {
				t.Fatal("invalid discovery accepted")
			}
			if len(f.batch.Page.Issues) != 0 {
				t.Fatal("invalid page changed durable preview")
			}
		})
	}
	f.backend.pages[""] = tracker.GitHubDiscoveryPage{Total: 1, Issues: []tracker.GitHubIssuePreview{previewIssue(12, false)}}
	if err := f.advance(t); err != nil {
		t.Fatal(err)
	}
	f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{999}}, http.StatusUnprocessableEntity)
	f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}, Destination: "Todo", AllowDispatch: true}, http.StatusOK)
	snapshot := batchSnapshot(12)
	snapshot.Provenance.ExternalID = "I_wrong"
	f.backend.snapshots[12] = snapshot
	if err := f.advance(t); err != nil {
		t.Fatal(err)
	}
	if f.batch.Items[0].Status != "failed" || !strings.Contains(f.batch.Items[0].Error, "stable source identity") {
		t.Fatalf("mismatched snapshot accepted: %+v", f.batch.Items[0])
	}
	request := tracker.GitHubBatchResult{Mutation: tracker.Mutation{IdempotencyKey: "unassigned-result"}, BatchID: f.batch.ID, Revision: f.batch.Revision, Number: 12, Snapshot: &snapshot}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/onboarding/issue-intake/result", f.runner.redemption.Credential, request), http.StatusNotFound)
	t.Run("partial discovery does not become a complete preview", func(t *testing.T) {
		current := newBatchFixture(t)
		current.backend.pages[""] = tracker.GitHubDiscoveryPage{}
		current.backend.discoveryGaps = true
		current.command(t, tracker.GitHubBatchCommand{Action: "discover"}, http.StatusOK)
		if err := current.advance(t); err == nil || current.batch.Status != "discovery_failed" || current.batch.Error == "" || len(current.batch.Items) != 0 {
			t.Fatalf("partial discovery=%+v error=%v", current.batch, err)
		}
		current.command(t, tracker.GitHubBatchCommand{Action: "retry"}, http.StatusOK)
		current.backend.discoveryGaps = false
		if err := current.advance(t); err != nil || current.batch.Status != "preview" {
			t.Fatalf("discovery recovery=%+v error=%v", current.batch, err)
		}
	})

}

func TestGitHubBatchRunnerOperations(t *testing.T) {
	for _, runner := range []string{"", "missing-runner"} {
		t.Run(runner, func(t *testing.T) {
			f := newBatchFixture(t)
			f.backend.pages[""] = tracker.GitHubDiscoveryPage{}
			f.command(t, tracker.GitHubBatchCommand{Action: "discover", RunnerID: runner}, http.StatusOK)
			if !f.batch.HubOwned || f.batch.RunnerID != "" || f.heartbeat(t) != nil {
				t.Fatalf("Hub intake depends on a worker transport: %+v", f.batch)
			}
			if err := f.advance(t); err != nil {
				t.Fatal(err)
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/onboarding/issue-intake", f.runner.redemption.Credential, tracker.GitHubBatchCommand{Mutation: tracker.Mutation{IdempotencyKey: "worker-command"}, Revision: f.batch.Revision, Action: "discover"})
			if response.Code == http.StatusOK {
				t.Fatal("runner obtained administrator intake authority")
			}
		})
	}
	t.Run("selection follows first requested occurrence", func(t *testing.T) {
		f := newBatchFixture(t)
		f.preview(t, false)
		f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{14, 12, 14, 13, 12}, Destination: "Backlog"}, http.StatusOK)
		numbers := make([]int, len(f.batch.Items))
		for index, item := range f.batch.Items {
			numbers[index] = item.Number
		}
		if !slices.Equal(numbers, []int{14, 12, 13}) {
			t.Fatalf("selection lost first occurrence or request order: %v", numbers)
		}
	})

	t.Run("runner transport without Hub import backend preserves full intake", func(t *testing.T) {
		f := newBatchFixture(t)
		f.workerPreview(t)
		f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12, 13, 14}, Destination: "Backlog"}, http.StatusOK)
		if err := f.advance(t); err != nil {
			t.Fatal(err)
		}
		task := f.heartbeat(t)
		if task == nil || len(task.Items) != 3 || task.Items[0].Number != 12 {
			t.Fatalf("worker selected page=%+v", task)
		}
		original := tracker.GitHubBatchResult{Mutation: tracker.Mutation{IdempotencyKey: "worker-source-first"}, BatchID: f.batch.ID, Revision: f.batch.Revision, Results: []tracker.GitHubBatchItemResult{{Number: 12, Snapshot: new(batchSnapshot(12))}, {Number: 13, Error: "Source context incomplete: private repository"}, {Number: 14, Snapshot: new(batchSnapshot(14))}}}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/onboarding/issue-intake/result", f.runner.redemption.Credential, original)
		requireNativeStatus(t, response, http.StatusOK)
		first := response.Body.String()
		f.refresh(t)
		if f.batch.Items[0].Status != "completed" || f.batch.Items[1].Status != "failed" || f.batch.Items[2].Status != "completed" || f.itemCount(t) != 3 {
			t.Fatalf("worker partial import=%+v count=%d", f.batch, f.itemCount(t))
		}
		replay := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/onboarding/issue-intake/result", f.runner.redemption.Credential, original)
		requireNativeStatus(t, replay, http.StatusOK)
		if replay.Body.String() != first {
			t.Fatalf("worker receipt replay changed: %s", replay.Body.String())
		}
		changed := original
		changed.Number = 99
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/onboarding/issue-intake/result", f.runner.redemption.Credential, changed), http.StatusConflict)

		edited := f.issue(t, f.batch.Items[1].WorkItemID)
		title, body := "Native retry title", "Native retry body"
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(edited.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "worker-native-edit"}, ExpectedRevision: edited.Revision, Title: &title, Body: &body}), http.StatusOK)
		f.command(t, tracker.GitHubBatchCommand{Action: "retry", RunnerID: f.runner.binding.RunnerID}, http.StatusOK)
		next := f.heartbeat(t)
		if next == nil || len(next.Items) != 1 || next.Items[0].Number != 13 {
			t.Fatalf("worker retry selection=%+v", next)
		}
		f.report(t, tracker.GitHubBatchResult{Number: 13, Snapshot: new(batchSnapshot(13))}, http.StatusOK)
		current := f.issue(t, edited.WorkItemID)
		if current.Title != title || current.Body != body || current.LinkedSource.Status != "complete" || f.commentCount(t, current.WorkItemID) != 1 || f.itemCount(t) != 3 {
			t.Fatalf("worker intake overwrote native content or duplicated data: %+v", current)
		}
		if task := f.heartbeat(t); task != nil {
			t.Fatalf("finished worker intake redelivered: %+v", task)
		}
	})
	t.Run("requester current authority fences delivery and writes", func(t *testing.T) {
		for _, fault := range []string{"credential revoked", "credential expired", "membership inactive", "project grant removed", "session revoked"} {
			t.Run(fault, func(t *testing.T) {
				first := newHostedBatchFixture(t)
				secondCfg := first.service.config
				secondCfg.Hosted = &HostedConfig{OrganizationID: "org_first_second", WorkOSOrganizationID: "provider_org_first_second", PublicURL: "https://second.example.test", Provider: first.provider}
				bindBatchTenant(t, first, secondCfg.Hosted)
				secondService := openPostgresHubTestService(t, projectRecordRepositoryFor(t, hostedPostgresAdmin(t, first.service), 4, first.clock.Now), secondCfg)
				second := newBatchFixtureForProject(t, newNativeFixture(t, secondService, "org_first_second", "Example unaffected intake"))
				second.clock = first.clock
				first.commandToken = seedBatchServiceKey(t, first)
				second.commandToken = seedBatchServiceKey(t, second)
				if fault == "credential expired" {
					first.commandToken = seedBatchExpiringKey(t, first, time.Minute)
				}
				if fault == "session revoked" {
					first.sessionCookie = createBatchSession(t, first)
				}
				for _, current := range []*batchFixture{first, second} {
					current.workerPreview(t)
					current.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}, Destination: "Backlog"}, http.StatusOK)
					if err := current.advance(t); err != nil {
						t.Fatal(err)
					}
					if task := current.heartbeat(t); task == nil || len(task.Items) != 1 {
						t.Fatalf("valid requester did not deliver before fault: %+v", task)
					}
				}
				state, err := first.service.loadGitHubBatchState(t.Context(), first.scope(t))
				if err != nil {
					t.Fatal(err)
				}
				before := state.Revision
				switch fault {
				case "credential expired":
					first.clock.Advance(2 * time.Minute)
				case "session revoked":
					if err := first.service.config.TenantRepository.WriteTenant(t.Context(), first.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
						return writer.RevokeAccessSession(t.Context(), apikey.HashToken(first.sessionCookie), first.clock.Now())
					}); err != nil {
						t.Fatal(err)
					}
				default:
					caller := first.scope(t).Credential
					if err := first.service.config.TenantRepository.WriteTenant(t.Context(), first.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
						switch fault {
						case "credential revoked":
							key, err := writer.CredentialByHash(t.Context(), apikey.HashToken(first.commandToken))
							if err != nil {
								return err
							}
							return writer.RevokeAccessCredential(t.Context(), key.ID, first.clock.Now())
						case "membership inactive":
							member, err := writer.AccessMember(t.Context(), caller.Hosted.Subject)
							if err != nil {
								return err
							}
							member.Active = false
							return writer.UpdateAccessMember(t.Context(), member)
						case "project grant removed":
							key, err := writer.CredentialByHash(t.Context(), apikey.HashToken(first.commandToken))
							if err != nil {
								return err
							}
							return writer.DeleteAccessTokenGrant(t.Context(), key.ID, first.project.ID)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				if task := second.heartbeat(t); task == nil {
					t.Fatal("fault in first tenant removed unaffected tenant task")
				}
				if task := first.heartbeat(t); task != nil {
					t.Fatalf("revoked requesting authority delivered task: %+v", task)
				}
				scope, err := first.service.gitHubIntegrationScope(t.Context(), first.project.OrganizationID, first.project.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := first.service.advanceGitHubBatchRecord(t.Context(), scope); err == nil {
					t.Fatal("revoked requester authorized maintenance write")
				}
				rejected := performHubAPIRequest(t, first.service, http.MethodPost, first.base+"/onboarding/issue-intake/result", first.runner.redemption.Credential, tracker.GitHubBatchResult{Mutation: tracker.Mutation{IdempotencyKey: "revoked-requester-result"}, BatchID: state.ID, Revision: state.Revision, Number: 12, Snapshot: new(batchSnapshot(12))})
				if rejected.Code == http.StatusOK {
					t.Fatal("revoked requester authorized result write")
				}
				var row projectstore.IntakeBatch
				if err := first.service.config.TenantRepository.ReadTenant(t.Context(), first.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					var err error
					row, err = reader.(projectstore.IntakeBatchReader).ProjectIntakeBatch(t.Context(), first.project.ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if row.Revision != before {
					t.Fatalf("requester fault changed batch revision=%d want=%d", row.Revision, before)
				}
				second.report(t, tracker.GitHubBatchResult{Number: 12, Snapshot: new(batchSnapshot(12))}, http.StatusOK)
				if second.batch.Status != "finished" {
					t.Fatalf("unaffected tenant did not finish: %+v", second.batch)
				}
			})
		}
	})

	for _, fault := range []string{"different runner", "runner revoked", "runner removed", "runner project grant removed", "checkout report expired"} {
		t.Run(fault, func(t *testing.T) {
			f := newBatchFixture(t)
			f.workerPreview(t)
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}, Destination: "Backlog"}, http.StatusOK)
			if err := f.advance(t); err != nil {
				t.Fatal(err)
			}
			before := f.batch.Revision
			token := f.runner.redemption.Credential
			switch fault {
			case "different runner":
				other := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Heartbeat)
				other.enroll(t)
				token = other.redemption.Credential
			case "checkout report expired":
				f.clock.Advance(6 * time.Minute)
			default:
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
					identity, err := writer.RunnerIdentity(t.Context(), f.runner.binding.RunnerID)
					if err != nil {
						return err
					}
					switch fault {
					case "runner revoked":
						return writer.RevokeAccessCredential(t.Context(), identity.TokenID, f.clock.Now())
					case "runner removed":
						return writer.RemoveRunnerIdentity(t.Context(), identity.RunnerID, f.clock.Now())
					default:
						return writer.DeleteAccessTokenGrant(t.Context(), identity.TokenID, f.project.ID)
					}
				}); err != nil {
					t.Fatal(err)
				}
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/onboarding/issue-intake/result", token, tracker.GitHubBatchResult{Mutation: tracker.Mutation{IdempotencyKey: "unassigned-worker-result"}, BatchID: f.batch.ID, Revision: before, Number: 12, Snapshot: new(batchSnapshot(12))})
			if response.Code == http.StatusOK {
				t.Fatal("invalid runner authority published source")
			}
			f.refresh(t)
			if f.batch.Revision != before || f.batch.Items[0].Status != "pending" || f.commentCount(t, f.batch.Items[0].WorkItemID) != 0 {
				t.Fatalf("invalid runner changed intake: %+v", f.batch)
			}
		})
	}

	for _, role := range []string{"owner", "admin", "member"} {
		t.Run("HTTP personal write key with "+role+" role", func(t *testing.T) {
			f := newHostedBatchFixture(t)
			f.commandToken = f.token
			var membership string
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
				member, err := writer.AccessMember(t.Context(), "user_"+string(f.project.OrganizationID))
				if err != nil {
					return err
				}
				membership = member.MembershipID
				member.Role = role
				return writer.UpdateAccessMember(t.Context(), member)
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.provider.SetMembershipRole(t.Context(), membership, role); err != nil {
				t.Fatal(err)
			}
			if role == "member" {
				f.command(t, tracker.GitHubBatchCommand{Action: "discover", RunnerID: f.runner.binding.RunnerID}, http.StatusNotFound)
				if f.itemCount(t) != 0 {
					t.Fatal("ordinary member created intake work")
				}
				return
			}
			f.workerPreview(t)
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}, Destination: "Backlog"}, http.StatusOK)
			state, err := f.service.loadGitHubBatchState(t.Context(), f.scope(t))
			if err != nil {
				t.Fatal(err)
			}
			if state.RequireAdminKey {
				t.Fatal("HTTP owner/admin write key acquired MCP-only boundary")
			}
			f.report(t, tracker.GitHubBatchResult{Number: 12, Snapshot: new(batchSnapshot(12))}, http.StatusOK)
			if f.batch.Status != "finished" || f.itemCount(t) != 1 {
				t.Fatalf("HTTP administrator write key lost intake: %+v", f.batch)
			}
		})
	}

	t.Run("legacy payload requires current authorized adoption", func(t *testing.T) {
		f := newBatchFixture(t)
		f.workerPreview(t)
		f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}, Destination: "Backlog"}, http.StatusOK)
		legacy := f.scope(t)
		batch := *f.batch
		if err := f.service.saveGitHubBatchRecord(t.Context(), legacy, &batch); err != nil {
			t.Fatal(err)
		}
		f.refresh(t)
		state, err := f.service.loadGitHubBatchState(t.Context(), f.scope(t))
		if err != nil || state.Issuer != nil {
			t.Fatalf("legacy payload decode=%+v %v", state, err)
		}
		if task := f.heartbeat(t); task != nil {
			t.Fatalf("legacy issuer fabricated: %+v", task)
		}
		scope, err := f.service.gitHubIntegrationScope(t.Context(), f.project.OrganizationID, f.project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.advanceGitHubBatchRecord(t.Context(), scope); err == nil {
			t.Fatal("legacy payload wrote without current issuer")
		}
		f.command(t, tracker.GitHubBatchCommand{Action: "retry", RunnerID: f.runner.binding.RunnerID}, http.StatusOK)
		if task := f.heartbeat(t); task == nil || len(task.Items) != 1 {
			t.Fatalf("current adoption did not restore task: %+v", task)
		}
		f.report(t, tracker.GitHubBatchResult{Number: 12, Snapshot: new(batchSnapshot(12))}, http.StatusOK)
		if f.batch.Status != "finished" || f.itemCount(t) != 1 {
			t.Fatalf("legacy adoption incomplete: %+v", f.batch)
		}
	})

}

func TestGitHubBatchSkipsLinkedAndLegacyImportedIdentity(t *testing.T) {
	for _, mode := range []string{"linked URL", "legacy source key", "stable provenance identity"} {
		t.Run(mode, func(t *testing.T) {
			f := newBatchFixture(t)
			var existing tracker.NativeIssue
			if mode == "linked URL" {
				f.backend.snapshots[12] = batchSnapshot(12)
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "prior-linked"}, GitHubIssueURL: previewIssue(12, false).URL})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &existing)
			} else {
				snapshot := batchSnapshot(12)
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "prior-import"}, Title: "Native edits on old import", Body: "Preserved", State: "Backlog", Provenance: &snapshot.Provenance})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &existing)
				if mode == "stable provenance identity" {
					row, _, err := f.service.readWorkItemRecord(t.Context(), f.scope(t), string(existing.WorkItemID))
					if err != nil {
						t.Fatal(err)
					}
					existing.Revision++
					raw, err := json.Marshal(existing)
					if err != nil {
						t.Fatal(err)
					}
					version, err := eventstore.NewContent(f.service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: existing.WorkItemID, RecordID: string(existing.WorkItemID), Revision: int64(existing.Revision)}, raw)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
						row.NativeSourceKey = new("github:old-url")
						updated, err := writer.UpdateWorkItem(t.Context(), row)
						if err != nil {
							return err
						}
						if updated.Revision != int64(existing.Revision) {
							return corestore.ErrRevisionConflict
						}
						_, err = writer.CreateCollaborationVersion(t.Context(), version)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.preview(t, false)
			before := len(f.backend.calls)
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}}, http.StatusOK)
			if err := f.advance(t); err != nil {
				t.Fatal(err)
			}
			if len(f.batch.Items) != 1 || f.batch.Items[0].Status != "skipped" || f.batch.Items[0].WorkItemID != existing.WorkItemID {
				t.Fatalf("duplicate result=%+v", f.batch.Items)
			}
			if len(f.backend.calls) != before || f.itemCount(t) != 1 {
				t.Fatalf("existing source fetched or duplicated: calls=%d/%d issues=%d", len(f.backend.calls), before, f.itemCount(t))
			}
			if got := f.issue(t, existing.WorkItemID); got.Title != existing.Title || got.Body != existing.Body {
				t.Fatalf("duplicate overwrote native content: %+v", got)
			}
		})
	}
}

func TestGitHubBatchHonorsFirstRunIntakeWithoutAnotherSourceRead(t *testing.T) {
	f := newBatchFixture(t)
	f.preview(t, false)
	f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}, Destination: "Todo", AllowDispatch: true}, http.StatusOK)
	f.backend.failures[12] = githubconnector.ErrNotFound
	if err := f.advance(t); err != nil {
		t.Fatal(err)
	}
	id := f.batch.Items[0].WorkItemID
	if id == "" {
		t.Fatal("incomplete source lost its native identity")
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(id)+"/source-intake", testHubAdminToken, tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: "first-run-intake"}, Snapshot: batchSnapshot(12)}), http.StatusOK)
	f.command(t, tracker.GitHubBatchCommand{Action: "retry"}, http.StatusOK)
	before := len(f.backend.calls)
	if err := f.advance(t); err != nil {
		t.Fatal(err)
	}
	if f.batch.Status != "finished" || f.batch.Items[0].Status != "completed" || len(f.backend.calls) != before {
		t.Fatalf("first-run snapshot fetched again: %+v calls=%d/%d", f.batch, len(f.backend.calls), before)
	}
}

func TestHeartbeatDoesNotReconcileGitHubBatchItems(t *testing.T) {
	f := newBatchFixture(t)
	f.preview(t, false)
	f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12, 13}, Destination: "Todo", AllowDispatch: true}, http.StatusOK)
	before, calls := f.batch.Revision, len(f.backend.calls)
	for range 3 {
		if f.heartbeat(t) != nil {
			t.Fatal("Hub intake assigned worker work")
		}
	}
	f.refresh(t)
	if f.batch.Revision != before || f.batch.Items[0].Status != "pending" || f.batch.Items[1].Status != "pending" || len(f.backend.calls) != calls {
		t.Fatalf("heartbeat reconciled intake: %+v", f.batch)
	}
	if err := f.advance(t); err != nil {
		t.Fatal(err)
	}
	if f.batch.Status != "finished" {
		t.Fatalf("maintenance did not reconcile intake: %+v", f.batch)
	}
	t.Run("worker heartbeat projects tasks without reconciling", func(t *testing.T) {
		f := newBatchFixture(t)
		f.workerPreview(t)
		f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12, 13}, Destination: "Todo", AllowDispatch: true}, http.StatusOK)
		before := f.batch.Revision
		for range 3 {
			task := f.heartbeat(t)
			if task == nil || len(task.Items) != 2 {
				t.Fatalf("worker heartbeat lost task: %+v", task)
			}
		}
		f.refresh(t)
		if f.batch.Revision != before || f.batch.Items[0].WorkItemID != "" || f.batch.Items[1].WorkItemID != "" || len(f.backend.calls) != 0 {
			t.Fatalf("worker heartbeat reconciled source: %+v", f.batch)
		}
		if err := f.advance(t); err != nil {
			t.Fatal(err)
		}
		if f.batch.Items[0].WorkItemID == "" || f.batch.Items[1].WorkItemID == "" {
			t.Fatalf("maintenance did not prepare worker selection: %+v", f.batch)
		}
	})

	t.Run("unavailable optional intake payload does not fail heartbeat", func(t *testing.T) {
		f := newBatchFixture(t)
		f.workerPreview(t)
		f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}, Destination: "Backlog"}, http.StatusOK)
		if task := f.heartbeat(t); task == nil || len(task.Items) != 1 {
			t.Fatalf("valid optional task=%+v", task)
		}
		var record projectstore.IntakeBatch
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			record, err = reader.(projectstore.IntakeBatchReader).ProjectIntakeBatch(t.Context(), f.project.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		details := f.details
		details.mu.Lock()
		raw := bytes.Clone(details.data[record.Payload.PayloadObjectKey])
		delete(details.data, record.Payload.PayloadObjectKey)
		details.mu.Unlock()
		t.Cleanup(func() { details.mu.Lock(); details.data[record.Payload.PayloadObjectKey] = raw; details.mu.Unlock() })
		f.clock.Advance(time.Minute)
		if task := f.heartbeat(t); task != nil {
			t.Fatalf("unavailable optional task leaked: %+v", task)
		}
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
			identity, err := reader.RunnerIdentity(t.Context(), f.runner.binding.RunnerID)
			if err != nil {
				return err
			}
			if !identity.LastHeartbeatAt.Equal(f.clock.Now()) {
				t.Fatalf("optional payload failure lost heartbeat: %s want %s", identity.LastHeartbeatAt, f.clock.Now())
			}
			current, err := reader.(projectstore.IntakeBatchReader).ProjectIntakeBatch(t.Context(), f.project.ID)
			if err != nil {
				return err
			}
			if current.Revision != record.Revision {
				t.Fatalf("optional read mutated batch: %d want %d", current.Revision, record.Revision)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		details.mu.Lock()
		details.data[record.Payload.PayloadObjectKey] = raw
		details.mu.Unlock()
		if task := f.heartbeat(t); task == nil || len(task.Items) != 1 {
			t.Fatalf("restored optional task did not recover: %+v", task)
		}
	})

}

func TestGitHubBatchExplicitRunnerReassignmentKeepsPendingSelection(t *testing.T) {
	f := newBatchFixture(t)
	f.preview(t, false)
	f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}}, http.StatusOK)
	f.backend.failures[12] = githubconnector.ErrNotFound
	if err := f.advance(t); err != nil {
		t.Fatal(err)
	}
	f.command(t, tracker.GitHubBatchCommand{Action: "retry", RunnerID: "unrelated-runner"}, http.StatusOK)
	if !f.batch.HubOwned || f.batch.RunnerID != "" || len(f.batch.Items) != 1 || f.batch.Items[0].Number != 12 || f.batch.Items[0].Status != "pending" {
		t.Fatalf("retry lost Hub-owned selection: %+v", f.batch)
	}
	t.Run("worker assignment changes while selected native identity survives", func(t *testing.T) {
		f := newBatchFixture(t)
		f.workerPreview(t)
		f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}, Destination: "Backlog"}, http.StatusOK)
		if err := f.advance(t); err != nil {
			t.Fatal(err)
		}
		f.report(t, tracker.GitHubBatchResult{Number: 12, Error: "Source context incomplete: private repository"}, http.StatusOK)
		id := f.batch.Items[0].WorkItemID
		other := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Heartbeat)
		other.enroll(t)
		old := f.runner
		f.runner = other
		f.heartbeat(t)
		f.command(t, tracker.GitHubBatchCommand{Action: "retry", RunnerID: other.binding.RunnerID}, http.StatusOK)
		if f.batch.HubOwned || f.batch.RunnerID != other.binding.RunnerID || len(f.batch.Items) != 1 || f.batch.Items[0].WorkItemID != id || f.batch.Items[0].Status != "pending" {
			t.Fatalf("reassignment lost selected identity: %+v", f.batch)
		}
		f.runner = old
		if task := f.heartbeat(t); task != nil {
			t.Fatalf("old runner retained reassigned work: %+v", task)
		}
		f.report(t, tracker.GitHubBatchResult{Number: 12, Snapshot: new(batchSnapshot(12))}, http.StatusNotFound)
		f.runner = other
		if task := f.heartbeat(t); task == nil || len(task.Items) != 1 {
			t.Fatalf("new runner lost pending selection: %+v", task)
		}
		f.report(t, tracker.GitHubBatchResult{Number: 12, Snapshot: new(batchSnapshot(12))}, http.StatusOK)
		if f.batch.Status != "finished" || f.itemCount(t) != 1 {
			t.Fatalf("reassignment did not converge: %+v", f.batch)
		}
	})

}

func TestGitHubBatchImportsDiscussionBeyondDefaultAPILimit(t *testing.T) {
	f := newBatchFixture(t)
	f.preview(t, false)
	snapshot := batchSnapshot(12)
	snapshot.Comments = nil
	for index := range 40 {
		snapshot.Comments = append(snapshot.Comments, tracker.GitHubIssueComment{Body: fmt.Sprint("Comment ", index), Provenance: tracker.Provenance{Provider: "github", ExternalID: fmt.Sprint("C_", index), AuthorID: "example-author", CreatedAt: f.service.config.now(), UpdatedAt: f.service.config.now(), ObservedAt: f.service.config.now()}})
	}
	f.backend.snapshots[12] = snapshot
	f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}}, http.StatusOK)
	if err := f.advance(t); err != nil {
		t.Fatal(err)
	}
	if count := f.commentCount(t, f.batch.Items[0].WorkItemID); count != 40 {
		t.Fatalf("discussion count=%d want40", count)
	}
	var cursors []string
	for _, call := range f.backend.calls {
		if call.Stage == "comments" {
			cursors = append(cursors, call.Cursor)
		}
	}
	if !slices.Equal(cursors, []string{"", "30"}) {
		t.Fatalf("discussion paging=%v", cursors)
	}
}

func TestGitHubBatchAppPagesAndPartialRetry(t *testing.T) {
	for _, test := range []struct {
		name          string
		fail, restart bool
	}{{name: "100 issue page"}, {name: "invalid issue rolls back and retries", fail: true}, {name: "restart leaves import for background maintenance", restart: true}} {
		t.Run(test.name, func(t *testing.T) {
			f := newBatchFixture(t)
			reads := 0
			f.service.config.ImportBackend = scheduledContractImporter(func(_ context.Context, request GitHubImportRequest) (GitHubImportPage, error) {
				reads++
				if request.Stage == "issue" {
					snapshot := batchSnapshot(request.IssueNumber)
					return GitHubImportPage{Issue: &IssueSource{Number: request.IssueNumber, URL: snapshot.URL, NodeID: snapshot.Provenance.ExternalID, Title: snapshot.Title, Body: snapshot.Body, AuthorID: snapshot.Provenance.AuthorID, CreatedAt: snapshot.Provenance.CreatedAt, UpdatedAt: snapshot.Provenance.UpdatedAt}}, nil
				}
				if request.Stage == "comments" {
					return GitHubImportPage{}, nil
				}
				if request.Stage != "batch" || request.Discovery == nil || !request.Discovery.Snapshots {
					t.Fatalf("source request=%+v", request)
				}
				page := tracker.GitHubDiscoveryPage{Total: 101}
				first, last := 1, 100
				if request.Discovery.Cursor != "" {
					first, last = 101, 101
				} else {
					page.NextCursor = "page2"
				}
				for number := first; number <= last; number++ {
					preview := previewIssue(number, false)
					snapshot := batchSnapshot(number)
					snapshot.Body = strings.Repeat("complete-source-body", 100) + issueContractTestSections
					preview.Snapshot = &snapshot
					preview.Labels = []string{"bug", "phase:one"}
					if number%2 == 0 {
						preview.Labels = append(preview.Labels, "detent:backlog")
					} else {
						preview.Labels = append(preview.Labels, "detent:todo")
					}
					if test.fail && number == 50 {
						snapshot.Body = strings.Repeat("x", (256<<10)+1)
					}
					page.Issues = append(page.Issues, preview)
				}
				return GitHubImportPage{BatchPage: &page}, nil
			})
			f.command(t, tracker.GitHubBatchCommand{Action: "start", Destination: "Backlog", AllowDispatch: true}, http.StatusOK)
			if test.restart {
				f.restart(t)
				if reads != 0 || !f.service.ready.Load() {
					t.Fatalf("startup performed import before readiness: reads=%d ready=%t", reads, f.service.ready.Load())
				}
			}
			if f.heartbeat(t) != nil {
				t.Fatal("App import assigned to runner")
			}
			if err := f.advance(t); err != nil {
				t.Fatal(err)
			}
			if len(f.batch.Items) != 100 || reads != 1 || f.batch.Status != "discovering" {
				t.Fatalf("first tick budget=%+v reads=%d", f.batch, reads)
			}
			if err := f.advance(t); err != nil {
				t.Fatal(err)
			}
			if f.batch.Status != "finished" || len(f.batch.Items) != 101 || reads != 2 {
				t.Fatalf("progress=%s items=%d reads=%d", f.batch.Status, len(f.batch.Items), reads)
			}
			for _, item := range f.batch.Items {
				if test.fail && item.Number == 50 {
					if item.Status != "failed" || item.WorkItemID != "" || item.Error == "" {
						t.Fatalf("failed item=%+v", item)
					}
					continue
				}
				issue := f.issue(t, item.WorkItemID)
				lane := "Todo"
				if item.Number%2 == 0 {
					lane = "Backlog"
				}
				if item.Status != "completed" || issue.State != lane || !slices.Equal(issue.Labels, []string{"bug", "phase:one"}) || !strings.Contains(issue.Body, "complete-source-body") {
					t.Fatalf("item=%+v issue=%+v", item, issue)
				}
			}
			view, err := f.service.projectGitHubBatch(t.Context(), f.scope(t))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "complete-source-body") || strings.Contains(string(raw), `"snapshot":`) {
				t.Fatal("batch read echoed source bodies")
			}
			if test.fail {
				f.command(t, tracker.GitHubBatchCommand{Action: "retry"}, http.StatusOK)
				if err := f.advance(t); err != nil {
					t.Fatal(err)
				}
				if f.batch.Items[49].Status != "completed" || reads != 4 {
					t.Fatalf("retry item=%+v reads=%d", f.batch.Items[49], reads)
				}
			}
			f.command(t, tracker.GitHubBatchCommand{Action: "start", Destination: "Backlog", AllowDispatch: true}, http.StatusOK)
			for range 2 {
				if err := f.advance(t); err != nil {
					t.Fatal(err)
				}
			}
			for _, item := range f.batch.Items {
				if item.Status != "skipped" {
					t.Fatalf("rerun duplicated item=%+v", item)
				}
			}
			if count := f.itemCount(t); count != 101 {
				t.Fatalf("duplicate issues=%d", count)
			}
		})
	}
}
func TestGitHubBatchLabelLaneConfirmation(t *testing.T) {
	for _, test := range []struct {
		name          string
		project       *tracker.GitHubProjectV2Source
		projectStatus string
		fallback      bool
		selective     bool
		rejected      bool
		unmapped      bool
		labels        []string
		mapping       map[string]string
		allow         bool
		ignore        bool
		lane          string
	}{
		{name: "mapped project status", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Blocked"}}, projectStatus: "Ready", lane: "Blocked"},
		{name: "unmapped project status", labels: []string{"bug", "detent:blocked"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Blocked"}}, projectStatus: "Unknown", unmapped: true, lane: "Backlog"},
		{name: "missing project status", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Blocked"}}, unmapped: true, lane: "Backlog"},
		{name: "project status overrides lane label", labels: []string{"bug", "detent:blocked"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Todo"}}, projectStatus: "Ready", allow: true, lane: "Todo"},
		{name: "dispatchable project mapping requires authorization", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Todo"}}, projectStatus: "Ready", rejected: true},
		{name: "runner fallback project mapping", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Blocked"}}, projectStatus: "Ready", fallback: true, lane: "Blocked"},
		{name: "selected project mapping", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Blocked"}}, projectStatus: "Ready", fallback: true, selective: true, lane: "Blocked"},
		{name: "selected unmapped project status", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Blocked"}}, projectStatus: "Unknown", unmapped: true, fallback: true, selective: true, lane: "Backlog"},
		{name: "unknown project lane refused", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Unknown"}}, rejected: true},
		{name: "missing project identifier refused", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{StatusLanes: map[string]string{"Ready": "Blocked"}}, rejected: true},
		{name: "missing project mappings refused", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source"}, rejected: true},
		{name: "default destination", labels: []string{"bug"}, lane: "Backlog"},
		{name: "lane label", labels: []string{"bug", "detent:todo"}, allow: true, lane: "Todo"},
		{name: "hyphenated lane label", labels: []string{"bug", "detent:in-progress"}, allow: true, lane: "In Progress"},
		{name: "dispatchable lane label without authorization stays in destination", labels: []string{"bug", "detent:todo"}, lane: "Backlog"},
		{name: "holding lane label without authorization", labels: []string{"bug", "detent:blocked"}, lane: "Blocked"},
		{name: "ignored lane labels", labels: []string{"bug", "detent:blocked"}, mapping: map[string]string{"detent:blocked": "Blocked"}, ignore: true, lane: "Backlog"},
		{name: "custom mapping", labels: []string{"incoming", "bug"}, mapping: map[string]string{"incoming": "Todo"}, allow: true, lane: "Todo"},
		{name: "conflicting lane labels stay in destination", labels: []string{"bug", "detent:todo", "detent:blocked"}, allow: true, lane: "Backlog"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newBatchFixture(t)
			preview := previewIssue(12, false)
			snapshot := batchSnapshot(12)
			preview.Labels, preview.ProjectStatus = test.labels, test.projectStatus
			if !test.fallback {
				preview.Snapshot = &snapshot
			}
			f.backend.pages[""] = tracker.GitHubDiscoveryPage{Total: 1, Issues: []tracker.GitHubIssuePreview{preview}}
			f.backend.snapshots[12] = snapshot
			command := tracker.GitHubBatchCommand{Action: "start", Destination: "Backlog", AllowDispatch: test.allow, LabelLanes: test.mapping, IgnoreLanes: test.ignore, ProjectV2: test.project}
			if test.selective {
				command.Action = "discover"
			}
			status := http.StatusOK
			if test.rejected {
				status = http.StatusUnprocessableEntity
			}
			if test.fallback {
				f.service.config.ImportBackend = nil
				command.RunnerID = f.runner.binding.RunnerID
			}
			f.command(t, command, status)
			if test.rejected {
				if f.itemCount(t) != 0 {
					t.Fatal("refused mapping created issues")
				}
				return
			}
			var workerDiscovery *tracker.GitHubDiscovery
			if test.fallback {
				task := f.heartbeat(t)
				if task == nil || task.Discovery == nil {
					t.Fatalf("worker routing discovery=%+v", task)
				}
				workerDiscovery = task.Discovery
				f.report(t, tracker.GitHubBatchResult{Page: &tracker.GitHubDiscoveryPage{Total: 1, Issues: []tracker.GitHubIssuePreview{preview}}}, http.StatusOK)
			} else if err := f.advance(t); err != nil {
				t.Fatal(err)
			}
			if test.selective {
				f.command(t, tracker.GitHubBatchCommand{Action: "apply", Destination: "Backlog", Numbers: []int{12}, AllowDispatch: test.allow}, http.StatusOK)
				if err := f.advance(t); err != nil {
					t.Fatal(err)
				}
			}
			if test.fallback {
				if err := f.advance(t); err != nil {
					t.Fatal(err)
				}
				task := f.heartbeat(t)
				if task == nil || len(task.Items) != 1 {
					t.Fatalf("worker routing selected source=%+v", task)
				}
				f.report(t, tracker.GitHubBatchResult{Number: 12, Snapshot: &snapshot}, http.StatusOK)
			}
			if f.batch.Status != "finished" || len(f.batch.Items) != 1 || f.batch.Items[0].Status != "completed" {
				t.Fatalf("routing batch=%+v", f.batch)
			}
			issue := f.issue(t, f.batch.Items[0].WorkItemID)
			if issue.State != test.lane {
				t.Fatalf("lane=%s want=%s issue=%+v", issue.State, test.lane, issue)
			}
			if test.project != nil {
				if (f.batch.Items[0].UnmappedProjectStatus != nil) != test.unmapped {
					t.Fatalf("unmapped report=%+v", f.batch.Items[0])
				}
				got := workerDiscovery
				if !test.fallback {
					got = f.backend.calls[0].Discovery
				}
				if got == nil || got.ProjectV2 == nil || got.ProjectV2.ID != test.project.ID {
					t.Fatalf("discovery lost ProjectV2: %+v", got)
				}
			}
			for _, label := range issue.Labels {
				if strings.HasPrefix(strings.ToLower(label), "detent:") {
					t.Fatalf("lane label retained as issue label: %s", label)
				}
			}
			if f.heartbeat(t) != nil {
				t.Fatal("typed routing assigned a worker task")
			}
		})
	}
}

func TestGitHubBatchAppFallbackAndThrottle(t *testing.T) {
	for _, test := range []struct {
		name      string
		err       error
		throttled bool
	}{{name: "App lacks repository access", err: githubconnector.ErrNotFound}, {name: "App throttle retains deadline", err: &githubconnector.StatusError{StatusCode: 429, RetryAfter: time.Hour, Err: githubconnector.ErrRateLimited}, throttled: true}} {
		t.Run(test.name, func(t *testing.T) {
			f := newBatchFixture(t)
			f.backend.discoveryError = test.err
			f.command(t, tracker.GitHubBatchCommand{Action: "start", Destination: "Backlog"}, http.StatusOK)
			if err := f.advance(t); !errors.Is(err, test.err) {
				t.Fatalf("source error=%v want=%v", err, test.err)
			}
			if !f.batch.HubOwned || f.batch.Status != "discovery_failed" || f.batch.Error == "" || f.heartbeat(t) != nil {
				t.Fatalf("source failure escaped Hub authority: %+v", f.batch)
			}
			if test.throttled {
				deadline, err := time.Parse(time.RFC3339Nano, f.batch.RetryAt)
				if err != nil || !deadline.Equal(f.clock.Now().Add(time.Hour)) {
					t.Fatalf("source deadline=%s error=%v", f.batch.RetryAt, err)
				}
				f.command(t, tracker.GitHubBatchCommand{Action: "retry"}, http.StatusUnprocessableEntity)
				f.clock.Advance(2 * time.Hour)
			}
			f.backend.discoveryError = nil
			f.backend.pages[""] = tracker.GitHubDiscoveryPage{}
			f.command(t, tracker.GitHubBatchCommand{Action: "retry"}, http.StatusOK)
			if err := f.advance(t); err != nil || f.batch.Status != "finished" {
				t.Fatalf("source recovery=%+v error=%v", f.batch, err)
			}
		})
	}
	t.Run("selected runner restores discovery when App cannot access repository", func(t *testing.T) {
		f := newBatchFixture(t)
		f.backend.discoveryError = githubconnector.ErrNotFound
		f.command(t, tracker.GitHubBatchCommand{Action: "start", Destination: "Backlog", RunnerID: f.runner.binding.RunnerID}, http.StatusOK)
		if err := f.advance(t); err != nil {
			t.Fatal(err)
		}
		if f.batch.HubOwned || f.batch.RunnerID != f.runner.binding.RunnerID || f.batch.Status != "discovering" {
			t.Fatalf("App fallback lost durable intent: %+v", f.batch)
		}
		task := f.heartbeat(t)
		if task == nil || task.Discovery == nil {
			t.Fatalf("App fallback discovery=%+v", task)
		}
		f.report(t, tracker.GitHubBatchResult{Page: &tracker.GitHubDiscoveryPage{Total: 1, Issues: []tracker.GitHubIssuePreview{previewIssue(12, false)}}}, http.StatusOK)
		f.report(t, tracker.GitHubBatchResult{Number: 12, Snapshot: new(batchSnapshot(12))}, http.StatusOK)
		if f.batch.Status != "finished" || f.batch.Items[0].Status != "completed" || f.itemCount(t) != 1 {
			t.Fatalf("App fallback incomplete: %+v", f.batch)
		}
	})
	for _, sourceErr := range []error{errors.New("temporary source transport outage"), &githubconnector.StatusError{StatusCode: 503, Err: errors.New("temporary source backend outage")}} {
		t.Run(sourceErr.Error(), func(t *testing.T) {
			f := newBatchFixture(t)
			f.preview(t, false)
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}, Destination: "Backlog"}, http.StatusOK)
			before := f.batch.Revision
			f.backend.failures[12] = sourceErr
			if err := f.advance(t); !errors.Is(err, sourceErr) {
				t.Fatalf("infrastructure error=%v", err)
			}
			if f.batch.Revision != before || f.batch.Items[0].Status != "pending" || f.batch.Items[0].Error != "" {
				t.Fatalf("infrastructure outage charged to source: %+v", f.batch)
			}
			delete(f.backend.failures, 12)
			if err := f.advance(t); err != nil {
				t.Fatal(err)
			}
			if f.batch.Status != "finished" || f.batch.Items[0].Status != "completed" || f.itemCount(t) != 1 {
				t.Fatalf("automatic retry did not converge: %+v", f.batch)
			}
		})
	}

	t.Run("requester revoked during fetch cannot publish source", func(t *testing.T) {
		f := newHostedBatchFixture(t)
		f.commandToken = seedBatchServiceKey(t, f)
		f.preview(t, false)
		f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}, Destination: "Backlog"}, http.StatusOK)
		before := f.batch.Revision
		f.backend.beforeFetch = func(request GitHubImportRequest) {
			if request.Stage != "issue" {
				return
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
				key, err := writer.CredentialByHash(t.Context(), apikey.HashToken(f.commandToken))
				if err != nil {
					return err
				}
				return writer.RevokeAccessCredential(t.Context(), key.ID, f.clock.Now())
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.advance(t); err == nil {
			t.Fatal("source published after requester revocation")
		}
		if f.batch.Revision != before || f.batch.Items[0].Status != "pending" {
			t.Fatalf("revoked fetch changed batch: %+v", f.batch)
		}
		scope := f.scope(t)
		var item tracker.NativeWorkItemID
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
			source, err := reader.LinkedSourceByURL(t.Context(), f.project.ID, previewIssue(12, false).URL)
			item = source.WorkItemID
			return err
		}); err != nil {
			t.Fatal(err)
		}
		row, version, err := f.service.readWorkItemRecord(t.Context(), scope, string(item))
		if err != nil {
			t.Fatal(err)
		}
		issue, err := f.service.hydrateWorkItemRecord(t.Context(), row, version)
		if err != nil {
			t.Fatal(err)
		}
		if issue.LinkedSource == nil || issue.LinkedSource.Status != "pending" || f.commentCount(t, item) != 0 {
			t.Fatalf("revoked requester published source: %+v", issue)
		}
	})

	for _, test := range []struct {
		name   string
		owned  bool
		status string
		reads  int
	}{{"typed ownership refusal overrides active payload", false, "discovering", 0}, {"typed status refusal overrides active payload", true, "finished", 0}, {"typed importing status selects batch", true, "importing", 0}, {"typed discovery selects batch", true, "discovering", 1}} {
		t.Run(test.name, func(t *testing.T) {
			f := newBatchFixture(t)
			f.backend.pages[""] = tracker.GitHubDiscoveryPage{}
			f.command(t, tracker.GitHubBatchCommand{Action: "discover"}, http.StatusOK)
			f.batch.HubOwned, f.batch.Status = test.owned, test.status
			if !test.owned {
				f.batch.RunnerID = f.runner.binding.RunnerID
			}
			f.save(t)
			f.clock.Advance(time.Hour)
			if !test.owned {
				f.heartbeat(t)
			}
			if err := f.service.maintainGitHubBatches(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(f.backend.calls) != test.reads {
				t.Fatalf("typed selection source reads=%d want=%d", len(f.backend.calls), test.reads)
			}
		})
	}
}

func TestGitHubBatchMaintenanceTraversal(t *testing.T) {
	for _, test := range []struct {
		name                      string
		owned                     bool
		status, intake            string
		malformed, invalidProject bool
	}{{"disabled intake", true, "discovering", "disabled", false, false}, {"typed ownership refusal", false, "discovering", "manual", false, false}, {"typed status refusal", true, "finished", "manual", false, false}, {"malformed batch payload", true, "discovering", "manual", true, false}, {"invalid project integration", true, "discovering", "manual", false, true}} {
		t.Run(test.name, func(t *testing.T) {
			f := newBatchFixture(t)
			f.backend.pages[""] = tracker.GitHubDiscoveryPage{}
			for index := range githubBatchMaintenanceLimit {
				projectID := tracker.ProjectID(fmt.Sprintf("prefix_%03d", index))
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
					record, err := writer.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: f.project.OrganizationID, ID: projectID, Name: string(projectID), Profile: "native"}, States: nativeFixtureStates()})
					if err != nil {
						return err
					}
					record.Settings.Repository, record.Settings.CheckoutRepository, record.Settings.Intake = "example-org/example-project", "example-org/example-project", test.intake
					if test.invalidProject {
						record.Settings.Repository, record.Settings.CheckoutRepository = "", ""
					}
					_, err = writer.UpdateProjectRecord(t.Context(), record)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				issuerScope := testGitHubBatchIssuer(t, f.service, f.project.OrganizationID, projectID)
				batch := &tracker.GitHubBatch{ID: fmt.Sprint("prefix_batch_", index), Revision: 1, HubOwned: test.owned, Status: test.status, Limit: 1000, RequestedBy: new(issuerScope.Actor()), Discovery: tracker.GitHubDiscovery{Repository: "example-org/example-project"}}
				if !test.owned {
					batch.RunnerID = f.runner.binding.RunnerID
				}
				row, err := f.service.prepareGitHubBatchRecord(t.Context(), issuerScope, batch)
				if err != nil {
					t.Fatal(err)
				}
				if test.malformed {
					payload, err := eventstore.NewContent(f.service.runDetailStore()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: f.project.OrganizationID}, json.RawMessage(`{"hub_owned":"unknown","status":"discovering"}`))
					if err != nil {
						t.Fatal(err)
					}
					row.Payload = payload
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
					return writer.(projectstore.IntakeBatchWriter).SaveProjectIntakeBatch(t.Context(), row, 0)
				}); err != nil {
					t.Fatal(err)
				}
			}
			f.command(t, tracker.GitHubBatchCommand{Action: "discover"}, http.StatusOK)
			f.clock.Advance(time.Hour)
			ticks := 1
			if test.status == "discovering" {
				ticks = githubBatchMaintenanceLimit + 1
			}
			for tick := range ticks {
				err := f.service.maintainGitHubBatches(t.Context())
				wantError := (test.malformed || !test.owned) && tick < githubBatchMaintenanceLimit
				if (err != nil) != wantError {
					t.Fatalf("tick=%d error=%v want error=%t", tick, err, wantError)
				}
				if tick < ticks-1 && len(f.backend.calls) != 0 {
					t.Fatalf("tick exceeded one-project budget: reads=%d", len(f.backend.calls))
				}
			}
			f.refresh(t)
			if f.batch.Status != "preview" || len(f.backend.calls) != 1 {
				t.Fatalf("later project stranded: batch=%+v calls=%d", f.batch, len(f.backend.calls))
			}
			projectID := tracker.ProjectID("prefix_000")
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
				project, err := writer.ProjectRecord(t.Context(), projectID)
				if err != nil {
					return err
				}
				project.Settings.Intake = "manual"
				project.Settings.Repository, project.Settings.CheckoutRepository = "example-org/example-project", "example-org/example-project"
				_, err = writer.UpdateProjectRecord(t.Context(), project)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			issuerScope := testGitHubBatchIssuer(t, f.service, f.project.OrganizationID, projectID)
			batch := &tracker.GitHubBatch{ID: "prefix_batch_0", Revision: 2, HubOwned: true, Status: "discovering", Limit: 1000, RequestedBy: new(issuerScope.Actor()), Discovery: tracker.GitHubDiscovery{Repository: "example-org/example-project"}}
			row, err := f.service.prepareGitHubBatchRecord(t.Context(), issuerScope, batch)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
				return writer.(projectstore.IntakeBatchWriter).SaveProjectIntakeBatch(t.Context(), row, 1)
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.service.maintainGitHubBatches(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(f.backend.calls) != 2 {
				t.Fatalf("re-enabled project did not progress: calls=%d", len(f.backend.calls))
			}
		})
	}
}

func TestGitHubBatchMaintenanceReconcilesRunnerProjects(t *testing.T) {
	first := newHostedBatchFixture(t)
	cfg := first.service.config
	cfg.Hosted = &HostedConfig{OrganizationID: "org_first_second", WorkOSOrganizationID: "provider_org_first_second", PublicURL: "https://hub.example.test", Provider: first.provider}
	bindBatchTenant(t, first, cfg.Hosted)
	secondService := openPostgresHubTestService(t, projectRecordRepositoryFor(t, hostedPostgresAdmin(t, first.service), 4, first.clock.Now), cfg)
	if secondService.database.hostedOrganization != "org_first_second" {
		t.Fatalf("second service selected organization=%s", secondService.database.hostedOrganization)
	}
	second := newBatchFixtureForProject(t, newNativeFixture(t, secondService, "org_first_second", "Example second batch"))
	for _, f := range []*batchFixture{first, second} {
		f.preview(t, false)
		f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12, 13}, Destination: "Todo", AllowDispatch: true}, http.StatusOK)
		f.backend.failures[12], f.backend.failures[13] = githubconnector.ErrNotFound, githubconnector.ErrNotFound
		if err := f.advance(t); err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.batch.Items[0].WorkItemID)+"/source-intake", testHubAdminToken, tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: "first-run-intake"}, Snapshot: batchSnapshot(12)}), http.StatusOK)
		f.command(t, tracker.GitHubBatchCommand{Action: "retry"}, http.StatusOK)
	}
	first.clock.Advance(time.Hour)
	beforeSecond := second.batch.Revision
	for index, f := range []*batchFixture{first, second} {
		before := len(f.backend.calls)
		if err := f.service.maintainGitHubBatches(t.Context()); err != nil {
			t.Fatal(err)
		}
		f.refresh(t)
		if f.batch.Items[0].Status != "completed" || f.batch.Items[1].Status != "failed" || len(f.backend.calls) != before+1 {
			t.Fatalf("tenant maintenance lost current snapshot: %+v calls=%d/%d", f.batch, len(f.backend.calls), before)
		}
		if index == 0 {
			second.refresh(t)
			if second.batch.Revision != beforeSecond {
				t.Fatal("first configured service mutated second tenant")
			}
		}
	}
}

func TestGitHubBatchMaintenanceItemBudgetAndConcurrentResult(t *testing.T) {
	for _, test := range []struct {
		name                  string
		items                 int
		concurrent, authority bool
	}{{"item budget", githubBatchMaintenanceLimit + 1, false, false}, {"concurrent result", 2, true, false}, {"current repository authority changed", 2, false, true}} {
		t.Run(test.name, func(t *testing.T) {
			f := newBatchFixture(t)
			var numbers []int
			for start := 1; start <= test.items; start += 100 {
				page := tracker.GitHubDiscoveryPage{Total: test.items}
				for number := start; number <= min(start+99, test.items); number++ {
					preview := previewIssue(number, false)
					snapshot := batchSnapshot(number)
					if !test.concurrent && !test.authority {
						preview.Snapshot = &snapshot
					}
					f.backend.snapshots[number] = snapshot
					page.Issues = append(page.Issues, preview)
					numbers = append(numbers, number)
				}
				if start+100 <= test.items {
					page.NextCursor = strconv.Itoa(start + 100)
				}
				cursor := ""
				if start > 1 {
					cursor = strconv.Itoa(start)
				}
				f.backend.pages[cursor] = page
			}
			f.command(t, tracker.GitHubBatchCommand{Action: "discover"}, http.StatusOK)
			if err := f.advance(t); err != nil {
				t.Fatal(err)
			}
			if test.items > 100 {
				f.command(t, tracker.GitHubBatchCommand{Action: "more"}, http.StatusOK)
				if err := f.advance(t); err != nil {
					t.Fatal(err)
				}
			}
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: numbers, Destination: "Backlog"}, http.StatusOK)
			if test.concurrent {
				f.backend.beforeFetch = func(request GitHubImportRequest) {
					if request.Stage == "issue" {
						f.backend.beforeFetch = nil
						f.command(t, tracker.GitHubBatchCommand{Action: "cancel"}, http.StatusOK)
					}
				}
			}
			if test.authority {
				f.backend.beforeFetch = func(request GitHubImportRequest) {
					if request.Stage != "issue" {
						return
					}
					f.backend.beforeFetch = nil
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
						project, err := writer.ProjectRecord(t.Context(), f.project.ID)
						if err != nil {
							return err
						}
						project.Settings.Repository, project.Settings.CheckoutRepository = "other/repository", "other/repository"
						_, err = writer.UpdateProjectRecord(t.Context(), project)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := f.advance(t)
			if test.authority {
				if err == nil || f.batch.Status != "importing" || f.batch.Items[0].Status != "pending" || f.batch.Items[1].Status != "pending" || f.itemCount(t) != 1 {
					t.Fatalf("owner failure reported partial success: batch=%+v error=%v count=%d", f.batch, err, f.itemCount(t))
				}
				var source eventstore.LinkedSource
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					var err error
					source, err = reader.LinkedSourceByURL(t.Context(), f.project.ID, previewIssue(1, false).URL)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if source.SnapshotObjectKey != "" {
					t.Fatal("changed repository authority published source content")
				}
				return
			}
			if test.concurrent {
				if !errors.Is(err, corestore.ErrRevisionConflict) || f.batch.Status != "cancelled" || f.itemCount(t) != 1 {
					t.Fatalf("concurrent command overwritten: %+v error=%v count=%d", f.batch, err, f.itemCount(t))
				}
				var source eventstore.LinkedSource
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					var err error
					source, err = reader.LinkedSourceByURL(t.Context(), f.project.ID, previewIssue(1, false).URL)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if source.SnapshotObjectKey != "" {
					t.Fatal("cancelled batch published an in-flight snapshot")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if f.batch.Status != "importing" || f.itemCount(t) != githubBatchMaintenanceLimit || f.batch.Items[100].Status != "pending" {
				t.Fatalf("item budget exceeded: status=%s count=%d last=%+v", f.batch.Status, f.itemCount(t), f.batch.Items[100])
			}
			if err := f.advance(t); err != nil {
				t.Fatal(err)
			}
			if f.batch.Status != "finished" || f.itemCount(t) != 101 {
				t.Fatalf("next tick lost progress: %+v count=%d", f.batch, f.itemCount(t))
			}
		})
	}
	t.Run("resume after source commit before batch checkpoint", func(t *testing.T) {
		f := newBatchFixture(t)
		f.preview(t, false)
		f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12, 13}, Destination: "Backlog"}, http.StatusOK)
		revision := f.batch.Revision
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f.backend.beforeFetch = func(request GitHubImportRequest) {
			if request.Stage == "issue" && request.IssueNumber == 13 {
				cancel()
			}
		}
		scope, err := f.service.gitHubIntegrationScope(t.Context(), f.project.OrganizationID, f.project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.advanceGitHubBatchRecord(ctx, scope); !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupted source read=%v", err)
		}
		f.refresh(t)
		if f.batch.Revision != revision || f.batch.Items[0].Status != "pending" {
			t.Fatalf("interruption wrote an incomplete batch checkpoint: %+v", f.batch)
		}
		f.backend.beforeFetch = nil
		if err := f.advance(t); err != nil {
			t.Fatal(err)
		}
		if f.batch.Status != "finished" || f.itemCount(t) != 2 || f.batch.Items[0].Status != "completed" || f.batch.Items[1].Status != "completed" {
			t.Fatalf("receipt-backed recovery duplicated or skipped owned imports: %+v count=%d", f.batch, f.itemCount(t))
		}
		reads := 0
		for _, request := range f.backend.calls {
			if request.Stage == "issue" && request.IssueNumber == 12 {
				reads++
			}
		}
		if reads != 1 {
			t.Fatalf("committed source fetched again: reads=%d", reads)
		}
	})

}

func TestGitHubBatchUsesCatalogJobCap(t *testing.T) {
	for _, test := range []struct {
		plan string
		cap  int
	}{{"free", 10000}, {"starter", 10000}} {
		t.Run(test.plan, func(t *testing.T) {
			f := newBatchFixture(t)
			plans := capacityHostedPlans()
			if err := f.service.database.configureHostedPlans(t.Context(), &HostedConfig{Plans: &plans}); err != nil {
				t.Fatal(err)
			}
			if test.plan != "free" {
				if err := f.service.database.applyHostedPlanCommand(t.Context(), bootstrapTokenID, hostedPlanCommand{ID: "paid-plan", Action: "base", ExpectedRevision: 1, Plan: PlanReference{ID: test.plan, Version: 1}, Reason: "fixture paid plan"}); err != nil {
					t.Fatal(err)
				}
			}
			reads := 0
			f.service.config.ImportBackend = scheduledContractImporter(func(_ context.Context, request GitHubImportRequest) (GitHubImportPage, error) {
				reads++
				if request.Stage != "batch" || request.Discovery == nil {
					t.Fatalf("unexpected source request=%+v", request)
				}
				offset := 0
				if request.Discovery.Cursor != "" {
					var err error
					offset, err = strconv.Atoi(request.Discovery.Cursor)
					if err != nil {
						t.Fatal(err)
					}
				}
				page := tracker.GitHubDiscoveryPage{Total: 201}
				for number := offset + 1; number <= min(offset+100, 201); number++ {
					preview := previewIssue(number, false)
					snapshot := batchSnapshot(number)
					preview.Snapshot = &snapshot
					page.Issues = append(page.Issues, preview)
				}
				if offset+100 < 201 {
					page.NextCursor = strconv.Itoa(offset + 100)
				}
				return GitHubImportPage{BatchPage: &page}, nil
			})
			f.command(t, tracker.GitHubBatchCommand{Action: "start", Destination: "Backlog"}, http.StatusOK)
			if f.batch.Limit != test.cap {
				t.Fatalf("job cap=%d want=%d", f.batch.Limit, test.cap)
			}
			for tick := range 3 {
				if err := f.advance(t); err != nil {
					t.Fatal(err)
				}
				if f.itemCount(t) != min((tick+1)*100, 201) || reads != tick+1 {
					t.Fatalf("bounded progress tick=%d count=%d reads=%d", tick, f.itemCount(t), reads)
				}
			}
			if f.itemCount(t) != 201 || f.batch.Status != "finished" || reads != 3 {
				t.Fatalf("catalog job count=%d status=%s reads=%d", f.itemCount(t), f.batch.Status, reads)
			}
		})
	}
}

func TestGitHubBatchSourceCloseOptIn(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprintf("source_closed=%t", closed), func(t *testing.T) {
			f := newBatchFixture(t)
			f.preview(t, closed)
			f.command(t, tracker.GitHubBatchCommand{Action: "close_source"}, http.StatusUnprocessableEntity)
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}}, http.StatusOK)
			if err := f.advance(t); err != nil {
				t.Fatal(err)
			}
			f.preview(t, closed)
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12, 13, 14}}, http.StatusOK)
			f.backend.failures[14] = githubconnector.ErrNotFound
			if err := f.advance(t); err != nil {
				t.Fatal(err)
			}
			readOutbound := func(id tracker.NativeWorkItemID) (projectstore.GitHubOutbound, error) {
				var row projectstore.GitHubOutbound
				err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					var err error
					row, err = reader.(projectstore.GitHubOutboxWriter).GitHubOutbound(t.Context(), "native:"+string(id)+":intake")
					return err
				})
				return row, err
			}
			for _, item := range f.batch.Items {
				if _, err := readOutbound(item.WorkItemID); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("source write without opt-in: item=%+v error=%v", item, err)
				}
			}
			f.command(t, tracker.GitHubBatchCommand{Action: "close_source"}, http.StatusUnprocessableEntity)
			f.service.database.linkedSourceBase = "https://cloud.example"
			f.service.config.OutboxBackend = &recordingOutboxBackend{}
			f.command(t, tracker.GitHubBatchCommand{Action: "close_source"}, http.StatusOK)
			f.command(t, tracker.GitHubBatchCommand{Action: "close_source"}, http.StatusOK)
			if f.batch.Items[0].SourceCloseRequested || f.batch.Items[2].SourceCloseRequested || f.batch.Items[1].SourceCloseRequested == closed {
				t.Fatalf("incorrect close selection: %+v", f.batch.Items)
			}
			for index, item := range f.batch.Items {
				row, err := readOutbound(item.WorkItemID)
				if index != 1 || closed {
					if !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("nonselected source closed: %+v error=%v", item, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				raw, err := eventstore.NewContent(f.service.runDetailStore()).Receipt(t.Context(), row.Payload)
				if err != nil {
					t.Fatal(err)
				}
				var desired WorkpadDesired
				if err := json.Unmarshal(raw, &desired); err != nil {
					t.Fatal(err)
				}
				var slug string
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					organization, err := reader.AccessOrganization(t.Context())
					slug = organization.Slug
					return err
				}); err != nil {
					t.Fatal(err)
				}
				want := "Tracked in Detent: https://cloud.example" + issueBrowserPath(slug, f.project.Slug, 2)
				if desired.Body != want || !desired.CloseSource || desired.StateReason != "" || desired.IssueNumber != 13 || row.ProjectID != f.project.ID || row.WorkItemID != item.WorkItemID {
					t.Fatalf("close payload=%+v row=%+v want body=%s", desired, row, want)
				}
			}
		})
	}
}

func newHostedBatchFixture(t *testing.T) *batchFixture {
	t.Helper()
	clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
	provider := newHostedSecurityProvider()
	for _, org := range []string{"org_first", "org_first_second"} {
		member := auth.Membership{ID: "membership_" + org, UserID: "user_" + org, OrganizationID: "provider_" + org, Status: "active"}
		member.Role.Slug = "owner"
		provider.members[member.ID] = member
	}
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}, Clock: clock.Now, Hosted: &HostedConfig{OrganizationID: "org_first", WorkOSOrganizationID: "provider_org_first", PublicURL: "https://hub.example.test", Provider: provider}})
	f := newBatchFixtureForProject(t, newNativeFixture(t, service, "org_first", "Example hosted batch"))
	f.clock, f.provider = clock, provider
	return f
}

func bindBatchTenant(t *testing.T, f *batchFixture, hosted *HostedConfig) {
	t.Helper()
	deployment, generation := hosted.deployment()
	if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE hosted_tenant SET provider_id=$2,bootstrap_subject=$3,deployment=$4,public_url=$5,allocation_generation=$6 WHERE organization_id=$1", hosted.OrganizationID, hosted.WorkOSOrganizationID, hosted.BootstrapSubject, deployment, hosted.PublicURL, generation); err != nil {
		t.Fatal(err)
	}
}

func seedBatchServiceKey(t *testing.T, f *batchFixture, ttl ...time.Duration) string {
	t.Helper()
	credential := f.scope(t).Credential
	token := hubapi.NewID("service_token")
	id := "service_" + string(f.project.ID)
	now := f.service.config.now()
	lifetime := 24 * time.Hour
	if len(ttl) != 0 {
		lifetime = ttl[0]
	}
	expires := now.Add(lifetime)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := writer.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: id, Name: "Example intake service", Hash: apikey.HashToken(token), Scope: "operator", Kind: "service", KeyScope: "admin", UserID: credential.Hosted.Subject, MembershipID: credential.HostedMembership, NativeOnly: true, ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires}); err != nil {
			return err
		}
		return writer.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.project.OrganizationID, TokenID: id, ProjectID: f.project.ID})
	}); err != nil {
		t.Fatal(err)
	}
	return token
}

func testGitHubBatchIssuer(t *testing.T, service *Service, organization tracker.OrganizationID, project tracker.ProjectID) hubscope.NativeScope {
	t.Helper()
	credential, _, err := service.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken(testHubAdminToken), "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := hubscope.NativeScope{Organization: organization, Project: project, Credential: credential}
	issuer := hubscope.OperatorChatIdentity{Identity: hubscope.OperatorIdentity(credential, string(organization))}
	authority := scope
	scope.GitHubIntakeRevision = &hubscope.GitHubIntakeRevision{Issuer: &issuer, Authority: &authority}
	return scope
}

func (f *batchFixture) report(t *testing.T, request tracker.GitHubBatchResult, status int) tracker.GitHubBatchReceipt {
	t.Helper()
	f.sequence++
	if request.IdempotencyKey == "" {
		request.IdempotencyKey = fmt.Sprint("worker-result-", f.sequence)
	}
	request.BatchID, request.Revision = f.batch.ID, f.batch.Revision
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/onboarding/issue-intake/result", f.runner.redemption.Credential, request)
	requireNativeStatus(t, response, status)
	var receipt tracker.GitHubBatchReceipt
	if status == http.StatusOK {
		decodeHubResponse(t, response, &receipt)
		f.refresh(t)
		if receipt.Revision != f.batch.Revision || receipt.Status != f.batch.Status {
			t.Fatalf("worker receipt lost batch checkpoint: %+v batch=%+v", receipt, f.batch)
		}
	}
	return receipt
}

func (f *batchFixture) workerPreview(t *testing.T) {
	t.Helper()
	f.service.config.ImportBackend = nil
	f.command(t, tracker.GitHubBatchCommand{Action: "discover", RunnerID: f.runner.binding.RunnerID}, http.StatusOK)
	task := f.heartbeat(t)
	if task == nil || task.Discovery == nil || task.BatchID != f.batch.ID {
		t.Fatalf("worker discovery missing: %+v", task)
	}
	f.report(t, tracker.GitHubBatchResult{Page: &tracker.GitHubDiscoveryPage{Total: 3, Issues: []tracker.GitHubIssuePreview{previewIssue(12, false), previewIssue(13, false), previewIssue(14, false)}}}, http.StatusOK)
}

func seedBatchExpiringKey(t *testing.T, f *batchFixture, ttl time.Duration) string {
	t.Helper()
	credential := f.scope(t).Credential
	token := hubapi.NewID("expiring_token")
	id := "expiring_" + string(f.project.ID)
	now := f.service.config.now()
	expires := now.Add(ttl)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := writer.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: id, Name: "Example expiring intake key", Hash: apikey.HashToken(token), Scope: "operator", Kind: "service", KeyScope: "admin", UserID: credential.Hosted.Subject, MembershipID: credential.HostedMembership, NativeOnly: true, ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires}); err != nil {
			return err
		}
		return writer.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.project.OrganizationID, TokenID: id, ProjectID: f.project.ID})
	}); err != nil {
		t.Fatal(err)
	}
	return token
}

func createBatchSession(t *testing.T, f *batchFixture) string {
	t.Helper()
	credential := f.scope(t).Credential
	now := f.service.config.now()
	identity := auth.Identity{Subject: credential.Hosted.Subject, Email: "operator@example.test", EmailVerified: true, Hosted: &auth.HostedIdentity{Subject: credential.Hosted.Subject, OrganizationID: f.service.config.Hosted.WorkOSOrganizationID, SessionID: "session_" + string(f.project.ID), CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}}
	f.provider.mu.Lock()
	f.provider.sessions[identity.Hosted.SessionID] = *identity.Hosted
	f.provider.mu.Unlock()
	cookie, _, err := f.service.hostedSessions.CreateIdentitySession(t.Context(), identity)
	if err != nil {
		t.Fatal(err)
	}
	return cookie
}
