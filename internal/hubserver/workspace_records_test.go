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
	saved := []struct{ path, token, id string }{}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		fixture := newNativeFixture(t, service, organization, "Example workspace project")
		foreign := newNativeFixture(t, service, organization, "Other project")
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
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, foreign.base+"/workspaces/"+session.ID, foreign.token, nil), http.StatusNotFound)
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodDelete, path+"/"+session.ID, fixture.token, nil), http.StatusNoContent)
				closed := performHubAPIRequest(t, service, http.MethodGet, path+"/"+session.ID, fixture.token, nil)
				requireNativeStatus(t, closed, http.StatusOK)
				decodeHubResponse(t, closed, &session)
				if session.State != "closed" {
					t.Fatalf("workspace close not persisted: %+v", session)
				}
				saved = append(saved, struct{ path, token, id string }{path, fixture.token, session.ID})
			})
		}
	}
	if len(saved) != 4 {
		t.Fatalf("saved workspaces=%d want 4", len(saved))
	}
	reopenWorkspaceRepository(t, service)
	for index, record := range saved {
		response := performHubAPIRequest(t, service, http.MethodGet, record.path+"/"+record.id, record.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var session workspacesession.Session
		decodeHubResponse(t, response, &session)
		if session.ID != record.id || session.State != "closed" {
			t.Fatalf("workspace after reopen: %+v", session)
		}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, record.path+"/"+record.id, saved[(index+2)%4].token, nil), http.StatusUnauthorized)
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
	return service.workspaces.readWorkspaceIDRecord(t.Context(), id)
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
	workspace, err := service.workspaces.readWorkspaceIDRecord(t.Context(), id)
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

func reopenWorkspaceRepository(t *testing.T, service *Service) {
	t.Helper()
	previous := service.database.postgres
	admin := workspaceTestDB(t, service)
	cfg := *service.config.Postgres
	cfg.LeaseClock = service.config.now
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.Open(t.Context(), cfg, postgres.RoleMigrations(os.DirFS("postgres/migrations")))
	if err != nil {
		t.Fatal(err)
	}
	service.database.postgres = repository
	service.config.TenantRepository = repository
	service.config.AccessRepository = repository
	service.config.BillingRepository = repository
	service.database.billingRepository = repository
	service.requestMetrics.repository = repository
	projectRecordDatabases.Store(repository, admin)
	t.Cleanup(func() { projectRecordDatabases.Delete(repository) })
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
