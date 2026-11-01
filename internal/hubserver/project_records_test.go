package hubserver

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/lib/pq"
	"github.com/pressly/goose/v3"

	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func projectRecordPostgres(t *testing.T) *postgres.Repository {
	t.Helper()
	template := pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
		roles, err := os.ReadFile("postgres/roles.sql")
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, string(roles)); err != nil {
			return err
		}
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, "SET ROLE detent_migration"); err != nil {
			return err
		}
		provider, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS("postgres/migrations"), goose.WithDisableGlobalRegistry(true))
		if err != nil {
			return err
		}
		_, err = provider.Up(ctx)
		return err
	})
	t.Cleanup(func() {
		deadline, ok := t.Deadline()
		if !ok {
			t.Error("missing test deadline")
			return
		}
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if err := template.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	admin := template.Database(t)
	name, password := newNativeID("example"), newNativeID("password")
	quoted := pq.QuoteIdentifier(name)
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pq.QuoteLiteral(password)); err != nil {
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
	if _, err := admin.ExecContext(t.Context(), "GRANT detent_runtime TO "+quoted); err != nil {
		t.Fatal(err)
	}
	pool := template.OpenRole(t, admin, name, password)
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 2, ServingConnections: 1, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	return repository
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
	t.Helper()
	fixture := newHostedSecurityFixture(t)
	owner := fixture.user(t, "owner", "owner", "owner@example.test", "write", "")
	fixture.service.config.TenantRepository = repository
	fixture.service.config.RunDetails = &memoryRunDetails{}
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
