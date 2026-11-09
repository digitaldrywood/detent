package hubserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestPostgresWorkspaceRoutesPersistSessionsAndDispatch(t *testing.T) {
	service := newWorkspaceNativeService(t, Config{})
	service.workspaces = newWorkspaceService(service, WorkspaceConfig{Enabled: true})
	if err := service.workspaces.start(t.Context()); err != nil {
		close(service.workspaces.stopped)
		t.Fatal(err)
	}
	saved := []struct{ path, token, id, action, run string }{}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		fixture := newNativeFixture(t, service, organization, "Example workspace project")
		foreign := newNativeFixture(t, service, organization, "Other project")
		policy := hubTestPolicy()
		approveHubTestPolicy(t, service, fixture.base+"/policy", policy)
		runner := prepareRunner(t, fixture, runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
		runner.redemption.Capacity = 8
		runner.enroll(t)
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, fixture.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential, map[string]any{"display_name": "Runner", "capacity": 8, "version": "test", "capacity_configuration": workspaceTestCapacity(8, service.config.now()), "workspace_capabilities": execCapabilities, "workspace_isolation": workspacesession.IsolationContainer}), http.StatusOK)
		for index := range 2 {
			t.Run(fmt.Sprintf("%s/%d", organization, index), func(t *testing.T) {
				subject := fixture.create(t, "Example workspace subject")
				path := fixture.base + "/workspaces"
				body := map[string]any{"idempotency_key": fmt.Sprintf("open-example-%d", index), "work_item_id": subject.WorkItemID, "requires": []string{"files"}}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, "", body), http.StatusUnauthorized)
				created := performHubAPIRequest(t, service, http.MethodPost, path, fixture.token, body)
				requireNativeStatus(t, created, http.StatusCreated)
				var session workspacesession.Session
				decodeHubResponse(t, created, &session)
				if session.State != "requested" || session.ID == "" || session.WorkItemID != string(subject.WorkItemID) {
					t.Fatalf("unexpected workspace: %+v", session)
				}
				replay := performHubAPIRequest(t, service, http.MethodPost, path, fixture.token, body)
				requireNativeStatus(t, replay, http.StatusCreated)
				if replay.Body.String() != created.Body.String() {
					t.Fatal("workspace replay changed response")
				}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, path+"/"+session.ID, fixture.token, nil), http.StatusOK)
				listing := performHubAPIRequest(t, service, http.MethodGet, path, fixture.token, nil)
				requireNativeStatus(t, listing, http.StatusOK)
				var page workspaceListResponse
				decodeHubResponse(t, listing, &page)
				found := false
				for _, item := range page.Workspaces {
					found = found || item.ID == session.ID
				}
				if len(page.Workspaces) != index+1 || !found {
					t.Fatalf("workspace page: %+v", page)
				}
				var dispatchID string
				if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					owner := r.(postgres.WorkspaceLifecycleReader)
					item, err := owner.WorkspaceDispatchItem(t.Context(), fixture.project.ID, session.ID)
					if err != nil {
						return err
					}
					dispatch, err := r.WorkItem(t.Context(), fixture.project.ID, tracker.NativeWorkItemID(item))
					if err != nil {
						return err
					}
					if stringValue(dispatch.WorkflowStateName) != "Todo" {
						t.Fatalf("dispatch state: %+v", dispatch)
					}
					dispatchID = item
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				var action workspacesession.Action
				var run projectActionRunReceipt
				t.Run("action execution", func(t *testing.T) {
					claim := performHubAPIRequest(t, service, http.MethodPost, fixture.base+"/claims", runner.redemption.Credential, tracker.NativeClaim{PolicyID: policy.ID, WorkItemID: tracker.NativeWorkItemID(dispatchID), MachineID: runner.binding.MachineID, SessionID: newNativeID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}})
					requireNativeStatus(t, claim, http.StatusOK)
					var lease tracker.NativeLease
					decodeHubResponse(t, claim, &lease)
					identity := workspaceIdentity(lease)
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/"+session.ID+"/worker/bind", runner.redemption.Credential, workspaceBindRequest{workspaceWorkerIdentity: identity, Capabilities: execCapabilities, Isolation: workspacesession.IsolationContainer}), http.StatusOK)
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/"+session.ID+"/worker/heartbeat", runner.redemption.Credential, workspaceHeartbeatRequest{workspaceWorkerIdentity: identity, State: workspacesession.StateReady, Capabilities: execCapabilities}), http.StatusOK)
					authored := performHubAPIRequest(t, service, http.MethodPost, fixture.base+"/actions", fixture.token, map[string]any{"idempotency_key": fmt.Sprintf("action-%d", index), "name": fmt.Sprintf("Example action %d", index), "command": "printf example"})
					requireNativeStatus(t, authored, http.StatusCreated)
					decodeHubResponse(t, authored, &action)
					runsPath := fixture.base + "/actions/" + action.ID + "/runs"
					runBody := projectActionRunRequest{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("run-%d", index)}, WorkspaceID: session.ID, ExpectedActionRevision: tracker.Revision(action.Revision)}
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, runsPath, "", runBody), http.StatusUnauthorized)
					queued := performHubAPIRequest(t, service, http.MethodPost, runsPath, fixture.token, runBody)
					requireNativeStatus(t, queued, http.StatusAccepted)
					decodeHubResponse(t, queued, &run)
					for sequence, status := range []string{workspacesession.RunRunning, workspacesession.RunSucceeded} {
						report := performHubAPIRequest(t, service, http.MethodPost, path+"/"+session.ID+"/worker/action-runs", runner.redemption.Credential, workspaceActionRunReport{WorkspaceIdentity: identity, ActionID: action.ID, RunID: run.RunID, Status: status, OutputSequence: int64(sequence + 1), Output: "example output\n"})
						requireNativeStatus(t, report, http.StatusOK)
					}
				})
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, foreign.base+"/workspaces/"+session.ID, foreign.token, nil), http.StatusNotFound)
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodDelete, path+"/"+session.ID, fixture.token, nil), http.StatusNoContent)
				closed := performHubAPIRequest(t, service, http.MethodGet, path+"/"+session.ID, fixture.token, nil)
				requireNativeStatus(t, closed, http.StatusOK)
				decodeHubResponse(t, closed, &session)
				if session.State != "closed" {
					t.Fatalf("workspace close not persisted: %+v", session)
				}
				t.Run("relay attach sees close", func(t *testing.T) {
					connection := &relayConnection{id: newNativeID("relayconn"), workspaceID: session.ID, scope: nativeScope{organization: organization, project: fixture.project.ID}, out: make(chan relayOutbound, 1), done: make(chan struct{})}
					if err := service.workspaces.relay.attachPerson(connection); err != nil {
						t.Fatal(err)
					}
					service.workspaces.closeIfEndedSinceUpgrade(t.Context(), connection)
					select {
					case notice := <-connection.out:
						if notice.final != workspacesession.CodeWorkspaceClosed {
							t.Fatalf("relay close reason=%q", notice.final)
						}
					default:
						t.Fatal("relay attached after close did not observe its tenant's closed workspace")
					}
				})
				saved = append(saved, struct{ path, token, id, action, run string }{path, fixture.token, session.ID, action.ID, run.RunID})
			})
		}
	}
	if len(saved) != 4 {
		t.Fatalf("saved workspaces=%d want 4", len(saved))
	}
	service = reopenWorkspaceHub(t, service)
	for index, record := range saved {
		response := performHubAPIRequest(t, service, http.MethodGet, record.path+"/"+record.id, record.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var session workspacesession.Session
		decodeHubResponse(t, response, &session)
		if session.ID != record.id || session.State != "closed" {
			t.Fatalf("workspace after reopen: %+v", session)
		}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, record.path+"/"+record.id, saved[(index+2)%4].token, nil), http.StatusUnauthorized)
		if record.run == "" {
			continue
		}
		runPath := strings.TrimSuffix(record.path, "/workspaces") + "/actions/" + record.action + "/runs/" + record.run
		stored := performHubAPIRequest(t, service, http.MethodGet, runPath, record.token, nil)
		requireNativeStatus(t, stored, http.StatusOK)
		var run workspacesession.Run
		decodeHubResponse(t, stored, &run)
		if run.Status != workspacesession.RunSucceeded || run.Command != "printf example" || run.OutputBytes != int64(len("example output\n")) {
			t.Fatalf("action after Hub reopen: %+v", run)
		}
		output := performHubAPIRequest(t, service, http.MethodGet, runPath+"/output", record.token, nil)
		requireNativeStatus(t, output, http.StatusOK)
		if output.Body.String() != "example output\n" {
			t.Fatalf("output after Hub reopen: %q", output.Body.String())
		}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, runPath+"/output", saved[(index+2)%4].token, nil), http.StatusUnauthorized)
	}
}

func TestPostgresWorkspaceClaimUsesCurrentTenantFacts(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := newWorkspaceNativeService(t, Config{now: func() time.Time { return now }, Workspace: &WorkspaceConfig{Enabled: true, maintenanceTicks: make(chan time.Time), Terminal: WorkspaceTerminalConfig{Enabled: true, Isolation: workspacesession.IsolationContainer}}})
	type candidate struct {
		scope nativeScope
		item  tracker.NativeWorkItemID
		want  bool
	}
	var saved []candidate
	evaluate := func(t *testing.T, record candidate, at time.Time, isolation string) {
		t.Helper()
		check := func(r tenantstore.Reader) error {
			eligible, err := workspaceClaimEligible(t.Context(), r.(workspaceReader), record.scope, record.item, time.Hour, isolation, at)
			if err != nil {
				return err
			}
			if eligible != record.want {
				t.Errorf("workspace claim eligible=%v want %v", eligible, record.want)
			}
			return nil
		}
		if err := service.config.TenantRepository.ReadTenant(t.Context(), record.scope.organization, corestore.Serving, check); err != nil {
			t.Fatal(err)
		}
		if err := service.config.TenantRepository.WriteTenant(t.Context(), record.scope.organization, corestore.Serving, func(w tenantstore.Writer) error { return check(w) }); err != nil {
			t.Fatal(err)
		}
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		fixture := newNativeFixture(t, service, organization, "Workspace claim project")
		runner := prepareRunner(t, fixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
		runner.enroll(t)
		credential, status, err := service.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken(runner.redemption.Credential), "", "")
		if err != nil || status != http.StatusOK {
			t.Fatalf("runner authentication status=%d error=%v", status, err)
		}
		scope := nativeScope{organization: organization, project: fixture.project.ID, credential: credential}
		for index := range 2 {
			subject := fixture.create(t, "Workspace claim subject")
			response := performHubAPIRequest(t, service, http.MethodPost, fixture.base+"/workspaces", fixture.token, map[string]any{"idempotency_key": fmt.Sprintf("claim-workspace-%d", index), "work_item_id": subject.WorkItemID, "requires": []string{"files"}})
			requireNativeStatus(t, response, http.StatusCreated)
			var session workspacesession.Session
			decodeHubResponse(t, response, &session)
			if _, err := workspaceTestDB(t, service).ExecContext(t.Context(), "UPDATE workspace_sessions SET requires='{files,terminal}' WHERE organization_id=$1 AND project_id=$2 AND id=$3", organization, fixture.project.ID, session.ID); err != nil {
				t.Fatal(err)
			}
			record := candidate{scope: scope}
			if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				item, err := r.(workspaceReader).WorkspaceDispatchItem(t.Context(), fixture.project.ID, session.ID)
				record.item = tracker.NativeWorkItemID(item)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if index == 0 {
				evaluate(t, record, now, workspacesession.IsolationContainer)
			}
			capabilities := workspacesession.Capabilities{Files: true, Terminal: true}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, fixture.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential, map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "capacity_configuration": workspaceTestCapacity(2, now), "workspace_capabilities": capabilities, "workspace_isolation": workspacesession.IsolationContainer}), http.StatusOK)
			record.want = true
			evaluate(t, record, now, workspacesession.IsolationContainer)
			for _, test := range []struct {
				name string
				at   time.Time
			}{
				{name: "heartbeat boundary", at: now.Add(runnerauth.HeartbeatTimeout)},
				{name: "request deadline", at: now.Add(service.workspaces.config.RequestTimeout)},
			} {
				t.Run(fmt.Sprintf("%s/%d/%s", organization, index, test.name), func(t *testing.T) {
					refused := record
					refused.want = false
					evaluate(t, refused, test.at, workspacesession.IsolationContainer)
				})
			}
			for _, test := range []struct {
				name         string
				capabilities workspacesession.Capabilities
				isolation    string
			}{
				{name: "missing required surface", capabilities: workspacesession.Capabilities{Files: true}, isolation: workspacesession.IsolationContainer},
				{name: "insufficient terminal isolation", capabilities: capabilities, isolation: workspacesession.IsolationUser},
			} {
				t.Run(fmt.Sprintf("%s/%d/%s", organization, index, test.name), func(t *testing.T) {
					if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
						return w.ReplaceRunnerWorkspaceReport(t.Context(), runner.binding.RunnerID, test.capabilities, test.isolation)
					}); err != nil {
						t.Fatal(err)
					}
					refused := record
					refused.want = false
					evaluate(t, refused, now, workspacesession.IsolationContainer)
				})
			}
			if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.ReplaceRunnerWorkspaceReport(t.Context(), runner.binding.RunnerID, capabilities, workspacesession.IsolationContainer); err != nil {
					return err
				}
				if index == 0 {
					return w.CloseWorkspaceItem(t.Context(), fixture.project.ID, record.item, now)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			record.want = index != 0
			evaluate(t, record, now, workspacesession.IsolationContainer)
			saved = append(saved, record)
		}
	}
	service = reopenWorkspaceHub(t, service)
	for index, record := range saved {
		evaluate(t, record, now, workspacesession.IsolationContainer)
		foreign := record
		foreign.scope.project = saved[(index+2)%len(saved)].scope.project
		foreign.want = false
		evaluate(t, foreign, now, workspacesession.IsolationContainer)
	}
}

func workspaceTestDB(t *testing.T, service *Service) *sql.DB {
	t.Helper()
	value, ok := projectRecordDatabases.Load(service.database.postgres)
	if !ok {
		t.Fatal("missing PostgreSQL test database")
	}
	return value.(*sql.DB)
}
func workspaceTestRecord(t *testing.T, service *Service, id string) (workspaceRecord, error) {
	t.Helper()
	return service.workspaces.readWorkspaceIDRecord(t.Context(), service.workspaces.organization(), id)
}
func workspaceTestRun(t *testing.T, service *Service, id string) (actionRunRecord, error) {
	t.Helper()
	var row postgres.ActionRun
	err := service.config.TenantRepository.ReadTenant(t.Context(), service.workspaces.organization(), corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.(workspaceReader).ActionRunByID(t.Context(), id)
		return err
	})
	if err != nil {
		return actionRunRecord{}, err
	}
	record, err := service.hydrateActionRunRecord(t.Context(), row, true)
	return record, err
}
func workspaceTestRecordings(t *testing.T, service *Service, id, relay string) ([]terminalRecordingRecord, error) {
	t.Helper()
	var records []terminalRecordingRecord
	workspace, err := service.workspaces.readWorkspaceIDRecord(t.Context(), service.workspaces.organization(), id)
	if err != nil {
		return nil, err
	}
	err = service.config.TenantRepository.ReadTenant(t.Context(), workspace.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		rows, err := r.(workspaceReader).TerminalRecordings(t.Context(), postgres.TerminalRecordingsInput{OrganizationID: workspace.OrganizationID, ProjectID: workspace.ProjectID, WorkspaceID: id, Limit: workspaceListLimit})
		if err != nil {
			return err
		}
		for _, row := range rows {
			if relay != "" && row.RelaySessionID != relay {
				continue
			}
			records = append(records, terminalFromRecord(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range records {
		var row postgres.TerminalRecording
		err = service.config.TenantRepository.ReadTenant(t.Context(), workspace.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			row, err = r.(workspaceReader).TerminalRecordingByID(t.Context(), postgres.TerminalRecordingInput{OrganizationID: workspace.OrganizationID, ProjectID: workspace.ProjectID, WorkspaceID: id, ID: records[i].ID})
			return err
		})
		if err != nil {
			return nil, err
		}
		raw, err := service.runDetailStore().ReadDetail(t.Context(), string(workspace.OrganizationID), workspaceArchive(row.CastObjectKey, row.CastSha256, row.CastBytes, row.CastContentBytes))
		if err != nil {
			return nil, err
		}
		records[i].Cast = string(raw)
	}
	return records, nil
}

func reopenWorkspaceHub(t *testing.T, service *Service) *Service {
	t.Helper()
	admin := workspaceTestDB(t, service)
	config := service.config
	organization := tracker.OrganizationID(service.localOrganization)
	workspace := service.workspaces.config
	config.Workspace = &workspace
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	return openWorkspaceTenantHub(t, config, admin, organization)
}

func openWorkspaceTenantHub(t *testing.T, config Config, admin *sql.DB, organization tracker.OrganizationID) *Service {
	t.Helper()
	cfg := *config.Postgres
	cfg.LeaseClock = config.now
	config.Postgres = &cfg
	config.TenantRepository = nil
	config.AccessRepository = nil
	config.BillingRepository = nil
	if config.Hosted == nil {
		config.Hosted = startupConfig(startupConnection(t, admin, "detent_runtime_v2", "")).Hosted
	}
	hosted := *config.Hosted
	hosted.OrganizationID = string(organization)
	config.Hosted = &hosted
	repository, err := postgres.Open(t.Context(), cfg, postgres.RoleMigrations(os.DirFS("postgres/migrations")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	projectRecordDatabases.Store(repository, admin)
	t.Cleanup(func() { projectRecordDatabases.Delete(repository) })
	service := openLocalPostgresHubTestService(t, repository, config)
	service.database.hostedOrganization = organization
	service.localOrganization = string(organization)
	service.requestMetrics.organization = organization
	service.registerConversationRoutes(service.echo)
	return service
}

func seedWorkspaceMember(t *testing.T, f *relayFixture, principal, subject, email, role string, grant, write, runners bool) {
	t.Helper()
	now := f.at()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: principal, Name: "Example member " + principal, Hash: apikey.HashToken(principal), Scope: "operator", Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: f.project.OrganizationID, UserID: subject, Email: email, MembershipID: "membership_" + subject, Role: role, Active: true, PrincipalID: principal, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if !grant {
			return nil
		}
		return w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: f.project.OrganizationID, UserID: subject, ProjectID: f.project.ID, CanWrite: write, ManageRunner: runners})
	}); err != nil {
		t.Fatal(err)
	}
}

func workspaceTestIssue(t *testing.T, s *Service, scope nativeScope, title, body string) tracker.NativeWorkItemID {
	t.Helper()
	input := tracker.CreateIssue{Title: title, Body: body, State: "Todo"}
	plan, err := s.loadWorkItemPlan(t.Context(), scope, "", "", input)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = s.planWorkItemRecord(t.Context(), scope, plan, input)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := s.prepareWorkItemFields(t.Context(), scope.organization, plan)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(plan.issue)
	if err != nil {
		t.Fatal(err)
	}
	version, err := eventstore.NewContent(s.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, RecordID: string(plan.issue.WorkItemID), Revision: 1}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := writeWorkItemRecord(t.Context(), w, plan, fields); err != nil {
			return err
		}
		_, err := w.CreateCollaborationVersion(t.Context(), version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return plan.issue.WorkItemID
}

func newWorkspaceNativeService(t *testing.T, cfg Config) *Service {
	t.Helper()
	repository := workspaceTestRepository(t, cfg.now)
	if cfg.Content == nil {
		content := &changeObjectFixture{}
		cfg.Content = content.call
	}
	if cfg.SecretKeys == nil {
		cfg.SecretKeys = secretTestKeys(t, "1", "1")
	}
	var service *Service
	primary := tracker.OrganizationID("org_first")
	if cfg.Hosted == nil {
		service = openLocalPostgresHubTestService(t, repository, cfg)
	} else {
		primary = tracker.OrganizationID(cfg.Hosted.OrganizationID)
		if err := repository.WriteTenant(t.Context(), primary, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: primary, Name: string(primary), Slug: strings.ReplaceAll(string(primary), "_", "-"), Local: true, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)})
		}); err != nil {
			t.Fatal(err)
		}
		service = openPostgresHubTestService(t, repository, cfg)
	}
	for _, organization := range []tracker.OrganizationID{primary + "_second", primary} {
		seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: organization, Name: string(organization), Slug: strings.ReplaceAll(string(organization), "_", "-")})
		key := bytes.Repeat([]byte{0x5a}, 32)
		envelope, err := service.config.SecretKeys.Seal(key, accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		now := service.config.now().UTC().Truncate(time.Microsecond)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: newNativeID("hub"), CursorKey: key}); err != nil {
				return err
			}
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
		}); err != nil {
			t.Fatal(err)
		}

	}
	service.database.hostedOrganization = primary
	service.localOrganization = string(primary)
	service.requestMetrics.organization = primary
	if cfg.Hosted == nil {
		service.registerConversationRoutes(service.echo)
	}
	return service
}

func workspaceTestRepository(t *testing.T, clock func() time.Time) *postgres.Repository {
	t.Helper()
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	pgcfg := *cfg.Postgres
	pgcfg.LeaseClock = clock
	repository, err := postgres.Open(t.Context(), pgcfg, postgres.RoleMigrations(os.DirFS("postgres/migrations")))
	if err != nil {
		t.Fatal(err)
	}
	projectRecordDatabases.Store(repository, admin)
	t.Cleanup(func() {
		projectRecordDatabases.Delete(repository)
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	return repository
}
func newWorkspaceDefaultFixture(t *testing.T, cfg Config) nativeFixture {
	t.Helper()
	return newNativeFixture(t, newWorkspaceNativeService(t, cfg), "", "Example project")
}

func workspaceTestCapacity(capacity int, at time.Time) runnerauth.CapacityConfig {
	return runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: capacity, ClientLimit: capacity, RuntimeLimit: capacity, Manageable: true, ObservedAt: at}
}

func newWorkspaceConversationFixture(t *testing.T, cfg *ConversationConfig, clocks ...func() time.Time) conversationAPIFixture {
	t.Helper()
	configured := ConversationConfig{Enabled: true}
	if cfg != nil {
		configured = *cfg
	}
	if configured.ArchiveContents == nil {
		configured.ArchiveContents = &conversationContentFixture{}
	}
	config := Config{Conversation: &configured}
	if len(clocks) > 0 {
		config.now = clocks[0]
	}
	f := newWorkspaceDefaultFixture(t, config)
	credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	token := tokenResponse{ID: newNativeID("credential"), Token: newNativeID("token")}
	now := f.service.config.now()
	if err = f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: token.ID, Name: "Example operator", Hash: apikey.HashToken(token.Token), Scope: "operator", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.project.OrganizationID, TokenID: token.ID, ProjectID: f.project.ID})
	}); err != nil {
		t.Fatal(err)
	}
	return conversationAPIFixture{nativeFixture: f, ownerID: credential.ID, other: token.Token, otherID: token.ID}
}

type workspaceConversationWorkerFixture struct {
	*conversationWorkerFixture
	operator string
}

func newWorkspaceConversationWorkerFixture(t *testing.T) workspaceConversationWorkerFixture {
	t.Helper()
	fixture := &conversationWorkerFixture{now: time.Now().UTC().Truncate(time.Microsecond)}
	api := newWorkspaceConversationFixture(t, nil, func() time.Time {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		return fixture.now
	})
	fixture.nativeFixture = api.nativeFixture
	fixture.chat = api.service.conversations
	fixture.principal = api.ownerID
	fixture.issue = api.nativeFixture.create(t, "Example conversation work")
	policy := hubTestPolicy()
	approveHubTestPolicy(t, api.service, api.base+"/policy", policy)
	fixture.policy = policy.ID
	runner := prepareRunner(t, api.nativeFixture, runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	runner.enroll(t)
	fixture.worker = runner.redemption.Credential
	requireNativeStatus(t, performHubAPIRequest(t, api.service, http.MethodPost, api.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", fixture.worker, map[string]any{"display_name": "Runner", "capacity": 4, "version": "test", "capacity_configuration": workspaceTestCapacity(4, fixture.now)}), http.StatusOK)
	response := performHubAPIRequest(t, api.service, http.MethodPost, api.base+"/claims", fixture.worker, tracker.NativeClaim{PolicyID: fixture.policy, WorkItemID: fixture.issue.WorkItemID, MachineID: runner.binding.MachineID, SessionID: newNativeID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &fixture.lease)
	fixture.attempt, fixture.run = newNativeID("attempt"), newNativeID("run")
	fixture.startAttempt(t, tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"})
	linked := fixture.now
	var created conversationstore.Conversation
	if err := api.service.config.TenantRepository.WriteTenant(t.Context(), api.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		created, err = w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: api.project.OrganizationID, ID: conversation.NewConversationID(), ProjectID: api.project.ID, OwnerPrincipalID: api.ownerID, Origin: conversationOriginWorker, Title: "Example conversation", Visibility: conversation.VisibilityShared, Status: conversation.StatusActive, WorkItemID: string(fixture.issue.WorkItemID), LinkedAt: &linked, Revision: 1, Execution: conversation.Execution{Status: conversation.ExecutionWaitingForRunner, UpdatedAt: linked}, Preferences: conversation.DefaultPreferences(), CreatedAt: linked, UpdatedAt: linked})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	fixture.record = conversationRecordFromStore(created)
	return workspaceConversationWorkerFixture{conversationWorkerFixture: fixture, operator: api.token}
}
