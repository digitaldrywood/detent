package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/pgtransport"

	"github.com/labstack/echo/v4"

	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var projectRecordDatabases sync.Map

func projectRecordPostgres(t *testing.T) *postgres.Repository {
	repository, _ := projectRecordPostgresWithAdmin(t, 3)
	return repository
}

func projectRecordPostgresWithAdmin(t *testing.T, maxConnections int, configure ...func(*pgxpool.Config)) (*postgres.Repository, *sql.DB) {
	t.Helper()
	template := postgresStartupTemplate
	admin := template.Database(t)
	var databaseName string
	if err := admin.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&databaseName); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{databaseName}.Sanitize()+" CONNECTION LIMIT "+strconv.Itoa(maxConnections+1)); err != nil {
		t.Fatal(err)
	}
	name, password := newNativeID("example"), newNativeID("password")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(password)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline, _ := t.Deadline()
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP OWNED BY "+quoted); err != nil {
			t.Error(err)
		}
		if _, err := admin.ExecContext(ctx, "DROP ROLE "+quoted); err != nil {
			t.Error(err)
		}
	})
	if _, err := admin.ExecContext(t.Context(), "GRANT detent_runtime_v2 TO "+quoted); err != nil {
		t.Fatal(err)
	}
	native := template.NativeConfig(t, admin, name, password)
	native.MaxConns = int32(maxConnections)
	for _, option := range configure {
		option(native)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: maxConnections, ServingConnections: maxConnections - 1, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	projectRecordDatabases.Store(repository, admin)
	t.Cleanup(func() { projectRecordDatabases.Delete(repository) })
	return repository, admin
}

func openPostgresHubTestService(t *testing.T, repository *postgres.Repository, config Config) *Service {
	t.Helper()
	config.TenantRepository = repository
	service := openTestService(t, config)
	service.workerCancel()
	<-service.workerDone
	<-service.secretRewrapDone
	if err := service.database.postgres.Close(); err != nil {
		t.Fatal(err)
	}
	service.database.postgres = repository
	service.requestMetrics.repository = repository
	service.config.AccessRepository = repository
	service.config.TenantRepository = repository
	service.config.BillingRepository = repository
	service.database.billingRepository = repository
	service.operatorChat = chat.NewService(nil, nil, hostedOperatorExecutor{service}, chat.WithClock(service.config.now))
	return service
}

func openLocalPostgresHubTestService(t *testing.T, repository *postgres.Repository, config Config) *Service {
	t.Helper()
	service := openPostgresHubTestService(t, repository, config)
	service.config.Hosted = nil
	service.echo = echo.New()
	service.echo.Use(service.measureRequests, service.policyCacheMiddleware, service.changeContentMiddleware, service.runDetailMiddleware)
	service.registerNativeRoutes(service.echo)
	service.registerRunnerRoutes(service.echo)
	service.registerOperatorTools(service.echo)
	service.operatorChat = chat.NewService(nil, nil, hostedOperatorExecutor{service}, chat.WithClock(service.config.now))
	return service
}

func seedPostgresHubAdmin(t *testing.T, repository tenantstore.Repository, service *Service, organization nativeOrganization) {
	t.Helper()
	now := service.config.now().UTC().Truncate(time.Microsecond)
	err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		existing, err := w.AccessOrganization(t.Context())
		if errors.Is(err, corestore.ErrNotFound) {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization.ID, Name: organization.Name, Slug: organization.Slug, Local: true, CreatedAt: now}); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if existing.Name != organization.Name || existing.Slug != organization.Slug {
			return errors.New("PostgreSQL fixture organization differs from existing row")
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization.ID, ProviderID: "provider_" + string(organization.ID)}); err != nil {
			return err
		}
		credential := accessstore.Credential{
			OrganizationID: organization.ID,
			ID:             "test_hub_admin",
			Name:           "Hub test administrator",
			Hash:           apikey.HashToken(testHubAdminToken),
			Scope:          "admin",
			CreatedAt:      now,
			UpdatedAt:      now,
			KeyScope:       "admin",
			ProjectAccess:  "all",
			Kind:           "service",
		}
		if service.config.Hosted != nil {
			credential.Kind = "personal"
			credential.UserID = "user_" + string(organization.ID)
			credential.MembershipID = "membership_" + string(organization.ID)
		}
		if err := w.CreateAccessCredential(t.Context(), credential); err != nil {
			return err
		}
		if service.config.Hosted == nil {
			return nil
		}
		return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization.ID, UserID: "user_" + string(organization.ID), Email: string(organization.ID) + "@example.test", MembershipID: "membership_" + string(organization.ID), Role: "owner", Active: true, PrincipalID: "test_hub_admin", CreatedAt: now, UpdatedAt: now})
	})
	if err != nil {
		t.Fatalf("seed PostgreSQL Hub administrator: %v", err)
	}
	if service.config.Hosted == nil {
		return
	}
	provider := service.config.Hosted.Provider.(*hostedSecurityProvider)
	provider.mu.Lock()
	membership := auth.Membership{ID: "membership_" + string(organization.ID), UserID: "user_" + string(organization.ID), OrganizationID: "provider_" + string(organization.ID), Status: "active"}
	membership.Role.Slug = "owner"
	provider.members[membership.ID] = membership
	provider.mu.Unlock()
	selectPostgresHubTestOrganization(service, organization.ID)
}

func selectPostgresHubTestOrganization(service *Service, organization tracker.OrganizationID) {
	service.config.Hosted.OrganizationID = string(organization)
	service.config.Hosted.WorkOSOrganizationID = "provider_" + string(organization)
	service.database.hostedOrganization = organization
	if service.requestMetrics != nil {
		service.requestMetrics.organization = organization
	}
}

func TestProjectRecordBackendParity(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}}
			var repository tenantstore.Repository
			if backend == "postgres" {
				repository = projectRecordPostgres(t)
				cfg.TenantRepository = repository
			}
			service := openTestService(t, cfg)
			service.config.AccessRepository = repository
			for _, orgName := range []string{"First organization", "Second organization"} {
				t.Run(orgName, func(t *testing.T) {
					org, err := service.createNativeOrganizationFor(t.Context(), orgName)
					if err != nil {
						t.Fatal(err)
					}
					if repository != nil {
						now := service.config.now()
						if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
							if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org.ID, Name: orgName, CreatedAt: now}); err != nil {
								return err
							}
							if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org.ID, ProviderID: "provider_" + string(org.ID), AllocationGeneration: 1}); err != nil {
								return err
							}
							return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org.ID, ID: "admin", Name: "Example admin", Hash: apikey.HashToken(testHubAdminToken), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now})
						}); err != nil {
							t.Fatal(err)
						}
					}
					base := "/api/v2/organizations/" + string(org.ID) + "/projects"
					input := map[string]any{"idempotency_key": "create", "name": "Example project", "states": nativeFixtureStates()}
					created := performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, input)
					requireNativeStatus(t, created, http.StatusOK)
					var project tracker.NativeProject
					decodeHubResponse(t, created, &project)
					if project.Slug != "example-project" || project.OrganizationID != org.ID {
						t.Fatalf("created project=%+v", project)
					}
					replay := performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, input)
					requireNativeStatus(t, replay, http.StatusOK)
					if replay.Body.String() != created.Body.String() {
						t.Fatalf("creation replay changed response: %s vs %s", replay.Body.String(), created.Body.String())
					}
					base += "/" + string(project.ID)
					read := performHubAPIRequest(t, service, http.MethodGet, base, testHubAdminToken, nil)
					requireNativeStatus(t, read, http.StatusOK)
					var got tracker.NativeProject
					decodeHubResponse(t, read, &got)
					if got.Name != project.Name || got.Slug != project.Slug {
						t.Fatalf("lookup=%+v", got)
					}
					renamed := performHubAPIRequest(t, service, http.MethodPatch, base, testHubAdminToken, map[string]any{"idempotency_key": "rename", "name": "Renamed project", "expected_revision": "1"})
					requireNativeStatus(t, renamed, http.StatusOK)
					var integration ProjectIntegration
					decodeHubResponse(t, renamed, &integration)
					if integration.Revision != 2 || integration.Name != "Renamed project" {
						t.Fatalf("renamed integration=%+v", integration)
					}
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPatch, base, testHubAdminToken, map[string]any{"idempotency_key": "stale", "name": "Rejected", "expected_revision": "1"}), http.StatusConflict)
					same := performHubAPIRequest(t, service, http.MethodPatch, base, testHubAdminToken, map[string]any{"idempotency_key": "rename", "name": "Renamed project", "expected_revision": "1"})
					requireNativeStatus(t, same, http.StatusOK)
					if same.Body.String() != renamed.Body.String() {
						t.Fatal("rename replay changed response")
					}
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPatch, base, testHubAdminToken, map[string]any{"idempotency_key": "rename", "name": "Changed request", "expected_revision": "1"}), http.StatusConflict)
					settings := performHubAPIRequest(t, service, http.MethodPut, base+"/integration", testHubAdminToken, map[string]any{"idempotency_key": "settings", "expected_revision": "2", "intake": "disabled", "projection": "disabled", "aging_enabled": false, "archive_completed_after_days": 14})
					requireNativeStatus(t, settings, http.StatusOK)
					decodeHubResponse(t, settings, &integration)
					if integration.Revision != 3 || integration.AgingEnabled || integration.ArchiveCompletedAfterDays == nil || *integration.ArchiveCompletedAfterDays != 14 {
						t.Fatalf("settings=%+v", integration)
					}
					content := performHubAPIRequest(t, service, http.MethodPut, base+"/content-settings", testHubAdminToken, map[string]any{"idempotency_key": "content", "expected_revision": "0", "require_visual_attachments": true})
					requireNativeStatus(t, content, http.StatusOK)
					var contentSettings projectsettings.Content
					decodeHubResponse(t, content, &contentSettings)
					if contentSettings.Revision != 1 || !contentSettings.RequireVisualAttachments {
						t.Fatalf("content=%+v", contentSettings)
					}
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPut, base+"/content-settings", testHubAdminToken, map[string]any{"idempotency_key": "stale-content", "expected_revision": "0", "require_visual_attachments": false}), http.StatusConflict)
					model := performHubAPIRequest(t, service, http.MethodPut, base+"/model-selection", testHubAdminToken, map[string]any{"idempotency_key": "model", "expected_revision": "1", "selection": nil})
					requireNativeStatus(t, model, http.StatusOK)
					var selection cloudModelSelection
					decodeHubResponse(t, model, &selection)
					if selection.Revision != 2 || selection.Selection != nil {
						t.Fatalf("model=%+v", selection)
					}
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, base+"/model-selection", testHubAdminToken, nil), http.StatusOK)
					modelOverride := performHubAPIRequest(t, service, http.MethodPut, base+"/model-selection", testHubAdminToken, map[string]any{"idempotency_key": "model-override", "expected_revision": "2", "selection": map[string]any{"enabled": false}})
					requireNativeStatus(t, modelOverride, http.StatusOK)
					decodeHubResponse(t, modelOverride, &selection)
					if selection.Revision != 3 || selection.Selection == nil || selection.Selection.Enabled == nil || *selection.Selection.Enabled {
						t.Fatalf("model override=%+v", selection)
					}
					storedModel := performHubAPIRequest(t, service, http.MethodGet, base+"/model-selection", testHubAdminToken, nil)
					requireNativeStatus(t, storedModel, http.StatusOK)
					decodeHubResponse(t, storedModel, &selection)
					if selection.Revision != 3 || selection.Selection == nil || selection.Selection.Enabled == nil || *selection.Selection.Enabled {
						t.Fatalf("stored model override=%+v", selection)
					}
					markdown := "---\ntracker:\n  kind: hub_native\n  lanes: [{name: Backlog, role: holding}, {name: Todo, role: active}, {name: In Progress, role: active}, {name: Rework, role: active}, {name: Merging, role: active}, {name: Blocked, role: holding}, {name: Human Review, role: holding}, {name: Plan Review, role: holding}, {name: Done, role: terminal}, {name: Cancelled, role: terminal}]\nplan:\n  enabled: true\n---\nComplete the issue.\n"
					workflow := performHubAPIRequest(t, service, http.MethodPut, base+"/integration", testHubAdminToken, map[string]any{"idempotency_key": "workflow", "expected_revision": "3", "intake": "disabled", "projection": "disabled", "workflow_markdown": markdown})
					requireNativeStatus(t, workflow, http.StatusOK)
					decodeHubResponse(t, workflow, &integration)
					if integration.Revision != 4 || integration.WorkflowMarkdown != markdown {
						t.Fatalf("workflow=%+v", integration)
					}
					var token tokenResponse
					if repository != nil {
						token.Token = "example-ungranted-" + string(org.ID)
						now := service.config.now()
						if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
							return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org.ID, ID: "ungranted", Name: "Example ungranted", Hash: apikey.HashToken(token.Token), Scope: "operator", ProjectAccess: "selected", Kind: "service", CreatedAt: now, UpdatedAt: now})
						}); err != nil {
							t.Fatal(err)
						}
					} else {
						tokenReply := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, map[string]any{"name": "ungranted-" + string(org.ID), "scope": "operator"})
						requireNativeStatus(t, tokenReply, http.StatusCreated)
						decodeHubResponse(t, tokenReply, &token)
					}
					deniedStatus := http.StatusNotFound
					if repository != nil {
						deniedStatus = http.StatusForbidden
					}
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, base, token.Token, nil), deniedStatus)
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPatch, base, token.Token, map[string]any{"idempotency_key": "unauthorized", "name": "Rejected", "expected_revision": "3"}), deniedStatus)
					read = performHubAPIRequest(t, service, http.MethodGet, base, testHubAdminToken, nil)
					requireNativeStatus(t, read, http.StatusOK)
					decodeHubResponse(t, read, &got)
					if got.Name != "Renamed project" || got.Slug != project.Slug || got.WorkflowMarkdown != markdown {
						t.Fatalf("final project=%+v", got)
					}
					if repository != nil {
						var count int
						if err := service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM projects WHERE id=?", project.ID).Scan(&count); err != nil {
							t.Fatal(err)
						}
						if count != 0 {
							t.Fatal("PostgreSQL project was also written to SQLite")
						}
						if err := repository.ReadTenant(t.Context(), org.ID, corestore.Serving, func(r tenantstore.Reader) error {
							record, err := r.ProjectRecord(t.Context(), project.ID)
							if err == nil && (record.Settings.Revision != 4 || record.Core.ContentSettingsRevision != 1 || record.Settings.ModelRevision != 3) {
								t.Fatalf("independent revisions=%+v", record)
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
				})
			}
			if repository != nil {
				t.Run("hosted participants", func(t *testing.T) { verifyProjectRecordParticipants(t, repository) })
			}
		})
	}
}

type projectRecordParticipantFixture struct{ reject bool }

func (p *projectRecordParticipantFixture) CreateProject(ctx context.Context, w tenantstore.Writer, actor tenantstore.ProjectActor, record projectstore.Record) error {
	if actor.HostedSubject == "" || actor.PrincipalID == "" {
		return nativeInvalid("missing actor")
	}
	_, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: record.Core.OrganizationID, ProjectID: record.Core.ID, NativeID: tracker.NativeWorkItemID("participant_" + string(record.Core.ID)), Number: 1, Title: "Participant write"})
	if err != nil {
		return err
	}
	if p.reject {
		return nativePublicInvalid("Participant rejected creation")
	}
	return nil
}

func (p *projectRecordParticipantFixture) UpdateProject(context.Context, tenantstore.Writer, tenantstore.ProjectActor, projectstore.Record, projectstore.Record) error {
	return nil
}
func (p *projectRecordParticipantFixture) RetireProject(context.Context, tenantstore.Writer, tenantstore.ProjectActor, projectstore.Record, time.Time) error {
	return nil
}

func verifyProjectRecordParticipants(t *testing.T, repository tenantstore.Repository) {
	fixture := newHostedSecurityFixture(t)
	fixture.service.config.TenantRepository = repository
	fixture.service.config.AccessRepository = repository
	fixture.service.config.RunDetails = &memoryRunDetails{}
	now := fixture.service.config.now().UTC()
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_security", Name: "Example organization", Slug: "example-organization", CreatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_security", ProviderID: "org_provider"})
	}); err != nil {
		t.Fatalf("seed PostgreSQL hosted access organization: %v", err)
	}
	owner := fixture.user(t, "owner", "owner", "owner@example.test", "", "")
	endpoint := "/api/v2/organizations/org_security/projects"
	request := map[string]any{"idempotency_key": "hosted-create", "name": "Hosted project", "grant_access": true}
	requireNativeStatus(t, fixture.request(t, owner, http.MethodPost, endpoint, request), http.StatusServiceUnavailable)
	participant := &projectRecordParticipantFixture{reject: true}
	fixture.service.config.ProjectParticipants = participant
	requireNativeStatus(t, fixture.request(t, owner, http.MethodPost, endpoint, request), http.StatusUnprocessableEntity)
	if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		records, err := r.Projects(t.Context(), "", 10)
		if err == nil && len(records) != 0 {
			t.Fatalf("rejected lifecycle committed projects=%+v", records)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	participant.reject = false
	response := fixture.request(t, owner, http.MethodPost, endpoint, request)
	requireNativeStatus(t, response, http.StatusCreated)
	var project tracker.NativeProject
	decodeHubResponse(t, response, &project)
	if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		record, err := r.ProjectRecord(t.Context(), project.ID)
		if err != nil {
			return err
		}
		if record.Core.Name != "Hosted project" {
			t.Fatalf("created hosted project=%+v", record)
		}
		item, err := r.WorkItem(t.Context(), project.ID, tracker.NativeWorkItemID("participant_"+string(project.ID)))
		if err == nil && item.Title != "Participant write" {
			t.Fatalf("participant write=%+v", item)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := fixture.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM projects WHERE id=?", project.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("hosted creation fell back to SQLite")
	}
}

func TestPostgresProjectParticipantsAuditLifecycleAndRevokeGrants(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	service := openPostgresHubTestService(t, repository, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}})
	org := nativeOrganization{ID: "org_project_lifecycle", Name: "Project lifecycle", Slug: "project-lifecycle"}
	seedPostgresHubAdmin(t, repository, service, org)
	now := service.config.now().UTC()
	actor := tenantstore.ProjectActor{CredentialID: "test_hub_admin", PrincipalID: "test_hub_admin", HostedSubject: "user_owner", Audit: accessstore.Audit{OrganizationID: org.ID, SessionID: "session_project_lifecycle", ActualActor: "user_owner", EffectiveUser: "user_owner", StartedAt: now, ExpiresAt: now.Add(time.Hour)}}
	participants := tenantProjectParticipants{service: service}
	var record projectstore.Record
	if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		record, err = w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org.ID, ID: "project_lifecycle", Name: "Lifecycle", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo"}}})
		if err != nil {
			return err
		}
		return participants.CreateProject(t.Context(), w, actor, record)
	}); err != nil {
		t.Fatal(err)
	}
	record.Core.Name = "Lifecycle updated"
	record.Core.Slug = "lifecycle-updated"
	if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
		updated, err := w.UpdateProjectRecord(t.Context(), record)
		if err != nil {
			return err
		}
		return participants.UpdateProject(t.Context(), w, actor, record, updated)
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
		current, err := w.ProjectRecord(t.Context(), record.Core.ID)
		if err != nil {
			return err
		}
		if err := participants.RetireProject(t.Context(), w, actor, current, service.config.now()); err != nil {
			return err
		}
		return w.RetireProject(t.Context(), record.Core.ID, current.Settings.Revision, service.config.now())
	}); err != nil {
		t.Fatal(err)
	}
	var created, updated, retired, grants int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FILTER (WHERE event='project_created'),count(*) FILTER (WHERE event='project_updated'),count(*) FILTER (WHERE event='project_retired') FROM hosted_audit WHERE organization_id=$1 AND project_id=$2`, org.ID, record.Core.ID).Scan(&created, &updated, &retired); err != nil {
		t.Fatal(err)
	}
	if created != 1 || updated != 1 || retired != 1 {
		t.Fatalf("project lifecycle audits = %d created, %d updated, %d retired", created, updated, retired)
	}
	if err := admin.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM hosted_project_grants WHERE organization_id=$1 AND project_id=$2)+(SELECT count(*) FROM token_grants WHERE organization_id=$1 AND project_id=$2)`, org.ID, record.Core.ID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if grants != 0 {
		t.Fatalf("retired project retained %d access grants", grants)
	}
}

func TestPostgresProjectCreationSerializesWithAdministratorPromotion(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 4)
	service := openPostgresHubTestService(t, repository, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	organization, err := service.createNativeOrganizationFor(t.Context(), "Example organization")
	if err != nil {
		t.Fatal(err)
	}
	seedPostgresHubAdmin(t, repository, service, organization)
	now := service.config.now().UTC()
	principal := "credential_example_member"
	user := "user_example_member"
	err = repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: principal, Name: "Example member", Hash: apikey.HashToken("example-member-token"), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ProjectAccess: "selected", Kind: "personal"}); err != nil {
			return err
		}
		return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization.ID, UserID: user, Email: "member@example.test", MembershipID: "membership_example_member", Role: "member", Active: true, PrincipalID: principal, CreatedAt: now, UpdatedAt: now})
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "RESET ROLE"); err != nil {
		t.Fatal(err)
	}
	projectID := tracker.ProjectID(newNativeID("project"))
	createReady := make(chan struct{})
	releaseCreate := make(chan struct{})
	createDone := make(chan error, 1)
	promoteDone := make(chan error, 1)
	createJoined := false
	promoteStarted := false
	promoteJoined := false
	go func() {
		createDone <- repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization.ID, ID: projectID, Name: "Example project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if err := w.InitializeProjectAccess(t.Context(), projectID, "test_hub_admin"); err != nil {
				return err
			}
			close(createReady)
			<-releaseCreate
			return nil
		})
	}()
	t.Cleanup(func() {
		select {
		case <-releaseCreate:
		default:
			close(releaseCreate)
		}
		if !createJoined {
			<-createDone
		}
		if promoteStarted && !promoteJoined {
			<-promoteDone
		}
	})
	select {
	case <-createReady:
	case err := <-createDone:
		createJoined = true
		t.Fatalf("create project before race barrier: %v", err)
	}
	promoteStarted = true
	go func() {
		promoteDone <- repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
			member, err := w.AccessMember(t.Context(), user)
			if err != nil {
				return err
			}
			member.Role, member.UpdatedAt = "admin", service.config.now().UTC()
			if err := w.UpdateAccessMember(t.Context(), member); err != nil {
				return err
			}
			return w.SyncHostedAdministratorProjectAccess(t.Context(), user, principal)
		})
	}()
	testenv.WaitUntil(t, t.Context(), func() error {
		select {
		case err := <-promoteDone:
			promoteJoined = true
			t.Fatalf("administrator promotion exited before the organization lock: %v", err)
		default:
		}
		var waiting bool
		if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FOR NO KEY UPDATE%')`).Scan(&waiting); err != nil {
			return err
		}
		if !waiting {
			return errors.New("administrator promotion has not reached the organization lock")
		}
		return nil
	})
	close(releaseCreate)
	if err := <-createDone; err != nil {
		t.Fatal(err)
	}
	createJoined = true
	if err := <-promoteDone; err != nil {
		t.Fatal(err)
	}
	promoteJoined = true
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		grant, err := r.AccessProjectGrant(t.Context(), user, projectID)
		if err != nil {
			return err
		}
		if !grant.CanWrite || !grant.ManageRunner {
			return errors.New("promoted administrator lacks the new project grant")
		}
		if _, err := r.AccessTokenGrant(t.Context(), principal, projectID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type countedProjectReads struct {
	tenantstore.Repository
	transactions int
	projects     int
	models       int
	active       int
}

func (r *countedProjectReads) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	r.transactions++
	r.active++
	defer func() { r.active-- }()
	return r.Repository.ReadTenant(ctx, org, workload, func(reader tenantstore.Reader) error { return read(countedProjectReader{Reader: reader, owner: r}) })
}

type countedProjectReader struct {
	tenantstore.Reader
	owner *countedProjectReads
}

func (r countedProjectReader) ProjectRecord(ctx context.Context, project tracker.ProjectID) (projectstore.Record, error) {
	r.owner.projects++
	return r.Reader.ProjectRecord(ctx, project)
}

func (r countedProjectReader) OrganizationModels(ctx context.Context) (policystore.OrganizationModels, error) {
	r.owner.models++
	return r.Reader.OrganizationModels(ctx)
}

func TestPostgresProjectReadAvoidsRepeatedAdmissions(t *testing.T) {
	repository := projectRecordPostgres(t)
	counted := &countedProjectReads{Repository: repository}
	accessCounted := &countedProjectReads{Repository: repository}
	now := time.Now().UTC()
	details := &memoryRunDetails{beforeRead: func(context.Context) error {
		if counted.active != 0 || accessCounted.active != 0 {
			return errors.New("archive read inside tenant transaction")
		}
		return nil
	}}
	service := &Service{echo: echo.New(), database: &database{postgres: repository}, secretRewrapReady: make(chan struct{}), config: Config{Logger: discardLogger(), TenantRepository: counted, AccessRepository: accessCounted, RunDetails: details, now: func() time.Time { return now }}}
	service.registerNativeRoutes(service.echo)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			token := string(org) + "_token"
			credential := string(org) + "_credential"
			project := tracker.ProjectID("project_" + string(org))
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now}); err != nil {
					return err
				}
				if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org)}); err != nil {
					return err
				}
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: credential, Name: "Example reader", Hash: apikey.HashToken(token), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ProjectAccess: "selected", Kind: "service"}); err != nil {
					return err
				}
				for _, id := range []tracker.ProjectID{project, "hidden_" + project} {
					if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: id, Name: string(id), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
						return err
					}
				}
				return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credential, ProjectID: project})
			}); err != nil {
				t.Fatal(err)
			}
			base := "/api/v2/organizations/" + string(org) + "/projects/"
			read := func(id tracker.ProjectID) tracker.NativeProject {
				t.Helper()
				counted.transactions, counted.projects, counted.models = 0, 0, 0
				accessCounted.transactions, accessCounted.projects, accessCounted.models = 0, 0, 0
				response := performHubAPIRequest(t, service, http.MethodGet, base+string(id), token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				transactions := counted.transactions + accessCounted.transactions
				t.Logf("project GET: tenant admissions=%d project reads=%d model reads=%d", transactions, counted.projects+accessCounted.projects, counted.models+accessCounted.models)
				projects := counted.projects + accessCounted.projects
				if transactions != 2 || projects != 1 {
					t.Errorf("project GET used %d tenant admissions and %d project reads; want 2 and 1", transactions, projects)
				}
				if accessCounted.transactions != 1 || accessCounted.projects != 1 {
					t.Errorf("project authorization owner used %d admissions and %d project reads; want 1 and 1", accessCounted.transactions, accessCounted.projects)
				}
				if models := counted.models + accessCounted.models; models != 1 {
					t.Errorf("model reads=%d, want 1", models)
				}
				var got tracker.NativeProject
				decodeHubResponse(t, response, &got)
				if got.OrganizationID != org || got.ID != project {
					t.Fatalf("project GET returned another tenant or project: %+v", got)
				}
				return got
			}
			if got := read(project); got.Name != string(project) {
				t.Fatalf("initial project name=%q", got.Name)
			}
			for revision := int64(1); revision <= 2; revision++ {
				enabled := (org == "org_first") == (revision == 1)
				raw, err := json.Marshal(config.ModelSelection{Enabled: new(enabled)})
				if err != nil {
					t.Fatal(err)
				}
				receipt, err := eventstore.NewContent(details).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org}, raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.SetOrganizationModels(t.Context(), policystore.OrganizationModels{OrganizationID: org, Revision: revision, Payload: eventstore.Reference(receipt.PayloadObjectKey, receipt.PayloadSHA256, receipt.PayloadBytes, receipt.PayloadContentBytes)}, revision-1)
				}); err != nil {
					t.Fatal(err)
				}
				got := read(project)
				var selection config.ModelSelection
				if err := json.Unmarshal(got.ModelSelection, &selection); err != nil {
					t.Fatal(err)
				}
				if selection.Enabled == nil || *selection.Enabled != enabled {
					t.Fatalf("inherited model revision %d enabled=%v, want %t", revision, selection.Enabled, enabled)
				}
			}
			overrideEnabled := org == "org_first"
			raw, err := json.Marshal(config.ModelSelection{Enabled: new(overrideEnabled)})
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := eventstore.NewContent(details).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org}, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.UpdateProjectModel(t.Context(), projectstore.Settings{OrganizationID: org, ProjectID: project, ModelRevision: 1, ModelKey: receipt.PayloadObjectKey, ModelSHA256: receipt.PayloadSHA256, ModelBytes: receipt.PayloadBytes, ModelContentBytes: receipt.PayloadContentBytes})
			}); err != nil {
				t.Fatal(err)
			}
			got := read(project)
			var overridden config.ModelSelection
			if err := json.Unmarshal(got.ModelSelection, &overridden); err != nil {
				t.Fatal(err)
			}
			if overridden.Enabled == nil || *overridden.Enabled != overrideEnabled {
				t.Fatalf("project override ignored: %+v", overridden)
			}
			originalRead := details.beforeRead
			details.beforeRead = func(context.Context) error { return errors.New("archive unavailable") }
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, base+string(project), token, nil), http.StatusInternalServerError)
			details.beforeRead = originalRead
			counted.models, accessCounted.models = 0, 0
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, base+string(project)+"/integration", token, nil), http.StatusOK)
			if counted.models+accessCounted.models != 0 {
				t.Fatal("integration route read organization models")
			}

			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, base+"hidden_"+string(project), token, nil), http.StatusForbidden)
			if counted.models+accessCounted.models != 0 {
				t.Fatal("denied project read organization models")
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				record, err := w.ProjectRecord(t.Context(), project)
				if err != nil {
					return err
				}
				record.Core.Name = "Updated " + string(org)
				_, err = w.UpdateProjectRecord(t.Context(), record)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if got := read(project); got.Name != "Updated "+string(org) {
				t.Fatalf("later request reused stale project: %+v", got)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.DeleteAccessTokenGrant(t.Context(), credential, project) }); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, base+string(project), token, nil), http.StatusForbidden)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credential, ProjectID: project}); err != nil {
					return err
				}
				return w.RevokeAccessCredential(t.Context(), credential, now)
			}); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, base+string(project), token, nil), http.StatusUnauthorized)
		})
	}
}
