package hubserver

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func hostedStorageConfig(t *testing.T, configure ...func(*Config)) Config {
	t.Helper()
	cfg := Config{Logger: discardLogger(), ListenAddress: "127.0.0.1:0", InitialAdminToken: []byte(testHubAdminToken), RunnerReleaseClient: &runnerReleaseFixture{}, Content: (&changeObjectFixture{}).call, RunDetails: &memoryRunDetails{}, Hosted: &hostedplans.HostedConfig{OrganizationID: "org_hosted", WorkOSOrganizationID: "org_provider", PublicURL: "https://tenant.example.test", Provider: newHostedSecurityProvider()}}
	for _, apply := range configure {
		apply(&cfg)
	}
	return hostedPostgresConfig(t, cfg)
}

func openHostedStorage(t *testing.T, cfg Config) *Service {
	t.Helper()
	started := time.Now()
	defer func() { t.Logf("hub_fixture_open_seconds=%.6f", time.Since(started).Seconds()) }()
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	return service
}

func hostedStorageProject(t *testing.T, service *Service, id tracker.ProjectID) projectstore.Record {
	t.Helper()
	var record projectstore.Record
	if err := service.config.TenantRepository.WriteTenant(t.Context(), service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		record, err = w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: service.accessOrganization(), ID: id, Name: "private-project-sentinel", Profile: "native"}, States: nativeFixtureStates()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestHostedDatabaseBindingRestart(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                  string
		change                func(*Config)
		wantError, shapeError bool
	}{
		{name: "same binding"},
		{name: "other organization", change: func(c *Config) { c.Hosted.OrganizationID = "org_other" }, wantError: true},
		{name: "other provider", change: func(c *Config) { c.Hosted.WorkOSOrganizationID = "org_other_provider" }, wantError: true},
		{name: "missing provider", change: func(c *Config) { c.Hosted.WorkOSOrganizationID = "" }, wantError: true, shapeError: true},
		{name: "other host", change: func(c *Config) { c.Hosted.PublicURL = "https://other.example.test" }, wantError: true},
		{name: "other bootstrap subject", change: func(c *Config) { c.Hosted.BootstrapSubject = "user_other" }, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := hostedStorageConfig(t)
			service := openHostedStorage(t, cfg)
			record := hostedStorageProject(t, service, "prj_binding")
			if err := service.Close(); err != nil {
				t.Fatal(err)
			}
			if test.change != nil {
				test.change(&cfg)
			}
			reopened, err := Open(t.Context(), cfg)
			if test.wantError {
				if err == nil || !test.shapeError && !errors.Is(err, ErrHostedDatabaseBinding) {
					t.Fatalf("reopen error = %v", err)
				}
				if reopened != nil {
					reopened.Close()
					t.Fatal("rejected binding returned a Service")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.config.TenantRepository.ReadTenant(t.Context(), record.Core.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				got, err := r.ProjectRecord(t.Context(), record.Core.ID)
				if err == nil && (got.Core.OrganizationID != tracker.OrganizationID(cfg.Hosted.OrganizationID) || reopened.database.hostedOrganization != got.Core.OrganizationID) {
					t.Fatalf("project organization = %q, bound organization = %q", got.Core.OrganizationID, reopened.database.hostedOrganization)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHostedDatabaseCloudAliases(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, from, to         string
		mutate                 func(*Config)
		originBound, wantError bool
	}{
		{name: "production", from: "https://hub.detent.build", to: "https://cloud.detent.build"},
		{name: "staging", from: "https://staging.hub.detent.build", to: "https://staging.cloud.detent.build"},
		{name: "production rollback", from: "https://cloud.detent.build", to: "https://hub.detent.build"},
		{name: "staging rollback", from: "https://staging.cloud.detent.build", to: "https://staging.hub.detent.build"},
		{name: "cross environment", from: "https://hub.detent.build", to: "https://staging.cloud.detent.build", wantError: true},
		{name: "unapproved origin", from: "https://tenant.example.test", to: "https://cloud.detent.build", wantError: true},
		{name: "origin-bound tenant", from: "https://hub.detent.build", to: "https://cloud.detent.build", originBound: true, wantError: true},
		{name: "changed generation", from: "https://hub.detent.build", to: "https://cloud.detent.build", mutate: func(c *Config) { c.Hosted.SharedEntry.Generation++ }, wantError: true},
		{name: "changed organization", from: "https://hub.detent.build", to: "https://cloud.detent.build", mutate: func(c *Config) { c.Hosted.OrganizationID = "org_other" }, wantError: true},
		{name: "changed provider", from: "https://hub.detent.build", to: "https://cloud.detent.build", mutate: func(c *Config) { c.Hosted.WorkOSOrganizationID = "org_other" }, wantError: true},
		{name: "changed bootstrap", from: "https://hub.detent.build", to: "https://cloud.detent.build", mutate: func(c *Config) { c.Hosted.BootstrapSubject = "user_other" }, wantError: true},
		{name: "removed shared entry", from: "https://hub.detent.build", to: "https://cloud.detent.build", mutate: func(c *Config) { c.Hosted.SharedEntry = nil }, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := hostedStorageConfig(t, func(cfg *Config) {
				cfg.Hosted.PublicURL = test.from
				if !test.originBound {
					cfg.Hosted.SharedEntry = &hostedplans.HostedSharedEntry{Issuer: "entry", Generation: 1, PublicKeys: []ed25519.PublicKey{hostedSharedKey(71).Public().(ed25519.PublicKey)}}
				}
			})
			service := openHostedStorage(t, cfg)
			hostedStorageProject(t, service, "prj_alias")
			if err := service.Close(); err != nil {
				t.Fatal(err)
			}
			cfg.Hosted.PublicURL = test.to
			if test.mutate != nil {
				test.mutate(&cfg)
			}
			reopened, err := Open(t.Context(), cfg)
			if test.wantError {
				if !errors.Is(err, ErrHostedDatabaseBinding) || reopened != nil {
					if reopened != nil {
						reopened.Close()
					}
					t.Fatalf("reopen = %v, %v; want binding rejection", reopened, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			var stored string
			var generation, projects, migrations int
			if err := hostedPostgresAdmin(t, reopened).QueryRowContext(t.Context(), `SELECT public_url,allocation_generation,(SELECT count(*) FROM projects WHERE organization_id=$1),(SELECT count(*) FROM hosted_binding_migrations WHERE organization_id=$1) FROM hosted_tenant WHERE organization_id=$1`, reopened.accessOrganization()).Scan(&stored, &generation, &projects, &migrations); err != nil {
				t.Fatal(err)
			}
			if stored != test.from || generation != 1 || projects != 1 || migrations != 0 {
				t.Fatalf("stored = %q, generation = %d, projects = %d, migrations = %d", stored, generation, projects, migrations)
			}
		})
	}
}

func TestHostedDatabaseBootstrapBinding(t *testing.T) {
	t.Parallel()
	cfg := hostedStorageConfig(t, func(cfg *Config) { cfg.Hosted.WorkOSOrganizationID = ""; cfg.Hosted.BootstrapSubject = "user_creator" })
	service := openHostedStorage(t, cfg)
	organization := service.accessOrganization()
	for range 2 {
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error { return w.BindAccessTenantProvider(t.Context(), "org_created") }); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error { return w.BindAccessTenantProvider(t.Context(), "org_replacement") }); err == nil {
		t.Fatal("provider rebinding was accepted")
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	service = openHostedStorage(t, cfg)
	report, err := service.readHostedMetadata(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.ProviderOrganizationID != "org_created" {
		t.Fatalf("provider organization = %q", report.ProviderOrganizationID)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*Config)
	}{
		{"wrong bootstrap", func(c *Config) { c.Hosted.BootstrapSubject = "user_other" }},
		{"missing bootstrap", func(c *Config) { c.Hosted.BootstrapSubject = "" }},
		{"maintenance requires provider", func(c *Config) { c.CredentialMaintenance = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := cfg
			hosted := *cfg.Hosted
			changed.Hosted = &hosted
			test.change(&changed)
			reopened, err := Open(t.Context(), changed)
			if err == nil || reopened != nil {
				if reopened != nil {
					reopened.Close()
				}
				t.Fatalf("untrusted bootstrap reopen = %v, %v", reopened, err)
			}
		})
	}
}

func TestHostedDatabaseRejectsInvalidBinding(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"organization", "public URL", "provider without bootstrap"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			cfg := hostedStorageConfig(t)
			switch field {
			case "organization":
				cfg.Hosted.OrganizationID = ""
			case "public URL":
				cfg.Hosted.PublicURL = ""
			case "provider without bootstrap":
				cfg.Hosted.WorkOSOrganizationID = ""
			}
			service, err := Open(t.Context(), cfg)
			if err == nil || service != nil {
				if service != nil {
					service.Close()
				}
				t.Fatalf("Service = %v, error = %v", service, err)
			}
		})
	}
}

func TestHostedDatabaseConstrainsBackgroundWrites(t *testing.T) {
	t.Parallel()
	service := openHostedStorage(t, hostedStorageConfig(t))
	first := service.accessOrganization()
	second := tracker.OrganizationID("org_other")
	repository := service.config.TenantRepository
	now := service.config.Clock()
	if err := repository.WriteTenant(t.Context(), second, corestore.Background, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: second, Name: "Other", CreatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: second, ProviderID: "org_other_provider", Deployment: "origin", PublicURL: "https://other.example.test"})
	}); err != nil {
		t.Fatal(err)
	}
	for _, organization := range []tracker.OrganizationID{first, second} {
		if err := repository.WriteTenant(t.Context(), organization, corestore.Background, func(w tenantstore.Writer) error {
			record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "prj_shared", Name: "Example", Profile: "native"}, States: nativeFixtureStates()})
			if err != nil {
				return err
			}
			if organization == first {
				record.Settings.Repository, record.Settings.RepositoryEnabled = "owner/repository", true
				if _, err := w.UpdateProjectRecord(t.Context(), record); err != nil {
					return err
				}
			}
			state := "Todo"
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: record.Core.ID, NativeID: "wi_shared", Number: 1, Revision: 1, Title: "Example", WorkflowStateName: &state, CreatedAt: now, UpdatedAt: now, NativeCreatedAt: now, NativeUpdatedAt: now}); err != nil {
				return err
			}
			return w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: "machine_shared", Hostname: "example", DisplayName: "Example", Capacity: 1, Version: "test"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name  string
		write func(tenantstore.Writer) error
	}{
		{"foreign organization", func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: second, Name: "Changed", CreatedAt: now})
		}},
		{"tenant reassignment", func(w tenantstore.Writer) error {
			return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: second})
		}},
		{"foreign project", func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: second, ID: "prj_other", Name: "Changed", Profile: "native"}})
			return err
		}},
		{"project reassignment", func(w tenantstore.Writer) error {
			record, err := w.ProjectRecord(t.Context(), "prj_shared")
			if err != nil {
				return err
			}
			record.Core.OrganizationID = second
			_, err = w.UpdateProjectRecord(t.Context(), record)
			return err
		}},
		{"issue reassignment", func(w tenantstore.Writer) error {
			row, err := w.WorkItem(t.Context(), "prj_shared", "wi_shared")
			if err != nil {
				return err
			}
			row.OrganizationID = second
			_, err = w.UpdateWorkItem(t.Context(), row)
			return err
		}},
		{"foreign machine", func(w tenantstore.Writer) error {
			return w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: second, ID: "machine_other", Hostname: "other", Capacity: 1, Version: "test"})
		}},
		{"unscoped machine", func(w tenantstore.Writer) error {
			return w.RegisterMachine(t.Context(), leasestore.Machine{ID: "machine_unscoped", Hostname: "other", Capacity: 1, Version: "test"})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := repository.WriteTenant(t.Context(), first, corestore.Background, test.write); !errors.Is(err, corestore.ErrTenantMismatch) {
				t.Fatalf("cross-organization write = %v", err)
			}
		})
	}
	for _, organization := range []tracker.OrganizationID{first, second} {
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			tenant, err := r.AccessTenant(t.Context())
			if err != nil {
				return err
			}
			if tenant.OrganizationID != organization {
				t.Fatal("tenant association changed")
			}
			record, err := r.ProjectRecord(t.Context(), "prj_shared")
			if err != nil {
				return err
			}
			if record.Core.OrganizationID != organization || record.Core.Name != "Example" {
				t.Fatalf("project changed: %+v", record.Core)
			}
			issue, err := r.WorkItem(t.Context(), "prj_shared", "wi_shared")
			if err != nil {
				return err
			}
			if issue.OrganizationID != organization || issue.Title != "Example" {
				t.Fatalf("issue changed: %+v", issue)
			}
			machine, err := r.Machine(t.Context(), "machine_shared")
			if err != nil {
				return err
			}
			if machine.OrganizationID != organization || machine.Hostname != "example" {
				t.Fatalf("machine changed: %+v", machine)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	targets, err := service.database.reconcileTargets(t.Context())
	if err != nil || len(targets) != 1 {
		t.Fatalf("background reconciliation targets = %d, error = %v", len(targets), err)
	}
}

func TestHostedMetadataExcludesCustomerContent(t *testing.T) {
	t.Parallel()
	expectedActivity := time.Date(2026, 9, 1, 12, 0, 0, 500_000_000, time.UTC)
	cfg := hostedStorageConfig(t, func(cfg *Config) { cfg.Clock = func() time.Time { return expectedActivity } })
	service := openHostedStorage(t, cfg)
	project := hostedStorageProject(t, service, "prj_metadata")
	const sentinel = "private-content-credential-sentinel"
	organization := service.accessOrganization()
	at := expectedActivity.Add(-500 * time.Millisecond)
	issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: project.Core.ID, WorkItemID: "wi_metadata", Number: 1, Revision: 1}, State: "Todo", Title: sentinel, Body: sentinel, WebURL: "https://private.example.test/" + sentinel, CreatedAt: at, UpdatedAt: at, LastActivityAt: at}
	fields, err := service.prepareWorkItemFields(t.Context(), organization, hubrecords.WorkItemRecordPlan{Issue: issue, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(service.runDetailStore())
	version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: project.Core.ID, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: 1}, historyTestJSON(t, issue))
	if err != nil {
		t.Fatal(err)
	}
	event, err := content.PrepareEvent(t.Context(), organization, historyTestJSON(t, tracker.CollaborationEvent{ID: "event_metadata", OrganizationID: organization, ProjectID: project.Core.ID, AggregateType: "issue", AggregateID: issue.WorkItemID, AggregateSequence: 1, Type: "issue.created", SchemaVersion: 1, Actor: tracker.Actor{Kind: "human", PrincipalID: "test_hub_admin", DisplayName: sentinel}, Data: tracker.CollaborationData{Reason: sentinel, Revision: 1}, RecordedAt: at}))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		project.Settings.Repository = "owner/" + sentinel
		if _, err := w.UpdateProjectRecord(t.Context(), project); err != nil {
			return err
		}
		if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project.Core.ID, NativeID: issue.WorkItemID, Number: 1, Revision: 1, Title: issue.Title, BodyObjectKey: &fields.Body, WorkflowStateName: &issue.State, CreatedAt: at, UpdatedAt: at, NativeCreatedAt: at, NativeUpdatedAt: at, LastActivityAt: at}); err != nil {
			return err
		}
		if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
			return err
		}
		if _, err := w.AppendEvent(t.Context(), event); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "metadata_credential", Name: sentinel, Fingerprint: sentinel, Hash: apikey.HashToken(sentinel), Scope: "operator", Kind: "service", ProjectAccess: "all", CreatedAt: at, UpdatedAt: at, LastUsedAt: &at}); err != nil {
			return err
		}
		if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: "user_metadata", Email: sentinel + "@example.test", MembershipID: "membership_metadata", PrincipalID: "test_hub_admin", Role: "viewer", Active: true, CreatedAt: at, UpdatedAt: at}); err != nil {
			return err
		}
		return w.CreateAccessSession(t.Context(), accessstore.Session{OrganizationID: organization, Hash: apikey.HashToken("session" + sentinel), Email: sentinel + "@example.test", Subject: "user_metadata", ProviderOrganizationID: "org_provider", SessionID: sentinel, IdentityCreatedAt: at, IdentityExpiresAt: expectedActivity.Add(time.Hour), CreatedAt: expectedActivity, ExpiresAt: expectedActivity.Add(time.Hour)})
	}); err != nil {
		t.Fatal(err)
	}
	report, err := service.readHostedMetadata(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.OrganizationID != "org_hosted" || report.ProviderOrganizationID != "org_provider" || report.Members == nil || *report.Members != 1 || report.Projects == nil || *report.Projects != 1 || report.Events == nil || *report.Events != 1 || report.Runners == nil || *report.Runners != 0 || report.LastActivity == nil || !report.LastActivity.Equal(expectedActivity) || !report.Healthy {
		t.Fatalf("report = %#v", report)
	}
	if report.DatabaseBytes != nil || report.CostDrivers.DatabaseBytes != nil || report.CostDrivers.WALBytes != nil || !slices.Contains(report.UnavailableUsage, "database_bytes") || !slices.Contains(report.UnavailableUsage, "wal_bytes") {
		t.Fatalf("physical storage was fabricated: %#v", report)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), sentinel) || strings.Contains(string(encoded), testHubAdminToken) {
		t.Fatal("report contains customer content or credentials")
	}
	var fieldsByName map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fieldsByName); err != nil {
		t.Fatal(err)
	}
	for name := range fieldsByName {
		if !slices.Contains([]string{"organization_id", "provider_organization_id", "members", "projects", "runners", "events", "last_activity", "database_bytes", "healthy", "unavailable_usage", "entitlement", "plan_id", "storage_quota_bytes", "event_quota", "cost_drivers"}, name) {
			t.Fatalf("unapproved report field %q", name)
		}
	}
}

func TestHostedMetadataFailsWithoutExposingErrors(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"local database", "closed database", "not ready"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			service := openHostedStorage(t, hostedStorageConfig(t))
			readService := &Service{database: service.database, config: service.config}
			switch name {
			case "local database":
				readService.config.Hosted = nil
			case "closed database":
				if err := service.Close(); err != nil {
					t.Fatal(err)
				}
				if err := service.database.postgres.Ping(t.Context()); err == nil {
					t.Fatal("closed PostgreSQL pool remained available")
				}
				readService.ready.Store(true)
			}
			report, err := readService.readHostedMetadata(t.Context())
			if err != ErrHostedMetadataUnavailable || !reflect.DeepEqual(report, hostedMetadataResponse{}) || strings.Contains(err.Error(), "private-path-sentinel") {
				t.Fatalf("report = %#v, error = %v", report, err)
			}
		})
	}
}
