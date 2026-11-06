package hubserver

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresHostedProjectListingUsesCurrentAccess(t *testing.T) {
	repository := projectRecordPostgres(t)
	provider := newHostedSecurityProvider()
	details := &memoryRunDetails{}
	service := &Service{echo: echo.New(), database: &database{postgres: repository}, secretRewrapReady: make(chan struct{}), config: Config{Logger: discardLogger(), now: time.Now, generateToken: apikey.GenerateToken, AccessRepository: repository, TenantRepository: repository, RunDetails: details, Hosted: &HostedConfig{OrganizationID: "org_security", WorkOSOrganizationID: "org_provider", PublicURL: "http://127.0.0.1:7777", Provider: provider}}}
	sessions, err := auth.NewSessionService(auth.SessionConfig{SessionTTL: 24 * time.Hour, PublicURL: service.config.Hosted.PublicURL}, service)
	if err != nil {
		t.Fatal(err)
	}
	service.hostedSessions = sessions
	service.registerRoutes(service.echo)
	f := hostedSecurityFixture{service: service, provider: provider}
	now := time.Now().UTC()
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_security", Name: "Example organization", CreatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_security", ProviderID: "org_provider"})
	}); err != nil {
		t.Fatal(err)
	}
	viewer := f.user(t, "project_viewer", "viewer", "viewer@example.test", "", "")
	var member accessstore.Member
	if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(t.Context(), viewer.identity.Subject)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	revisions := map[tracker.ProjectID]policystore.Revision{}
	for _, id := range []tracker.ProjectID{"prj_first", "prj_second"} {
		descriptor := hubTestPolicy()
		descriptor.Requirements.RequiredTags = []string{"example-tag"}
		if id == "prj_second" {
			descriptor.Requirements.RequiredTags = []string{"different-tag"}
		}
		descriptor = descriptor.WithID()
		revision, err := eventstore.NewContent(details).PrepareProjectPolicy(t.Context(), "org_security", descriptor)
		if err != nil {
			t.Fatal(err)
		}
		revision.ProjectID = id
		revision.ApprovedBy = "example_operator"
		revision.ApprovedAt = now
		revisions[id] = revision
	}
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		for _, project := range []corestore.Project{
			{OrganizationID: "org_security", ID: "prj_first", Name: "First project", Slug: "first-project", Profile: "native"},
			{OrganizationID: "org_security", ID: "prj_second", Name: "Second project", Slug: "second-project", Profile: "native"},
			{OrganizationID: "org_security", ID: "prj_hidden", Name: "Hidden project", Slug: "hidden-project", Profile: "native"},
		} {
			created, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: project, States: nativeFixtureStates()})
			if err != nil {
				return err
			}
			created.Settings.Repository = "example-org/example-project"
			created.Settings.RepositoryEnabled = true
			if _, err := w.UpdateProjectRecord(t.Context(), created); err != nil {
				return err
			}
			if project.ID == "prj_hidden" {
				continue
			}
			if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_security", UserID: member.UserID, ProjectID: project.ID}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_security", TokenID: member.PrincipalID, ProjectID: project.ID}); err != nil {
				return err
			}
			if err := w.SaveProjectOnboardingProgress(t.Context(), project.ID, onboarding.Progress{Revision: 1, Repository: "existing", Doctor: true, Provider: true, Artifacts: "local", UpdatedAt: now.Format(time.RFC3339Nano)}, 0); err != nil {
				return err
			}
			if err := w.ApproveProjectPolicy(t.Context(), revisions[project.ID], ""); err != nil {
				return err
			}
			runner := "runner_" + string(project.ID)
			machine := "machine_" + string(project.ID)
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_security", ID: runner, Name: runner, Hash: apikey.HashToken("org_security_" + runner), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ProjectAccess: "selected", Kind: "service"}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_security", TokenID: runner, ProjectID: project.ID}); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_security", ID: machine, Hostname: "example-host", Capacity: 2}); err != nil {
				return err
			}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: "org_security", ID: "enrollment_" + runner, RunnerID: runner, MachineID: tracker.MachineID(machine), TokenHash: apikey.HashToken("enroll_" + runner), Scope: "projects", ProjectIDs: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: member.PrincipalID}); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: "org_security", RunnerID: runner, MachineID: tracker.MachineID(machine), TokenID: runner, EnrollmentID: "enrollment_" + runner, Scope: "projects", State: "active", DisplayName: runner, Tags: []string{"example-tag"}, Operations: []string{runnerauth.Read, runnerauth.Claim}, CapacityLimit: 2, ReportedCapacity: 2, ProjectIDs: []tracker.ProjectID{project.ID}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
				return err
			}
			if err := w.ReplaceRunnerLocalChecks(t.Context(), runner, project.ID, runnerauth.LocalChecks{Checkout: "passed", Configuration: "passed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"codex"}, ObservedAt: now}); err != nil {
				return err
			}

		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), "org_other", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_other", Name: "Other example", CreatedAt: now}); err != nil {
			return err
		}
		for _, id := range []tracker.ProjectID{"prj_first", "prj_second"} {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_other", ID: id, Name: string(id), Profile: "native"}, Settings: projectstore.Settings{Repository: "example-org/other-project"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if err := w.SaveProjectOnboardingProgress(t.Context(), id, onboarding.Progress{Revision: 1, Artifacts: "customer", UpdatedAt: now.Format(time.RFC3339Nano)}, 0); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	counted := &projectListReadRepository{Repository: repository}
	f.service.config.TenantRepository = counted
	var objectReads atomic.Int64
	details.beforeRead = func(ctx context.Context) error {
		timing := requestTimingFrom(ctx)
		if timing == nil {
			return nil
		}
		if active, ok := counted.active.Load(timing); ok && active.(*atomic.Int64).Load() != 0 {
			return errors.New("policy payload read inside tenant transaction")
		}
		objectReads.Add(1)
		return nil
	}
	if service.database.db != nil || service.database.reader != nil || service.database.authReader != nil || service.database.claimReader != nil {
		t.Fatal("Projects fixture retained a legacy database handle")
	}
	path := "/api/v2/organizations/org_security/projects"
	response := f.request(t, viewer, http.MethodGet, path, nil)
	if response.Code != http.StatusOK {
		_, failure := f.service.projectOnboardingBatchTenant(t.Context(), "org_security", []appBootstrapProject{{ID: "prj_first"}, {ID: "prj_second"}})
		t.Logf("typed onboarding failure: %v", failure)
	}
	requireNativeStatus(t, response, http.StatusOK)
	if counted.reads.Load() != 2 || objectReads.Load() != 2 {
		t.Fatalf("project list did not batch tenant/policy reads: transactions=%d objects=%d", counted.reads.Load(), objectReads.Load())
	}
	var result []hostedProjectView
	decodeHubResponse(t, response, &result)
	if len(result) != 2 {
		t.Fatalf("visible projects=%d, want two", len(result))
	}
	projects := map[string]string{}
	for _, project := range result {
		projects[project.ID] = project.Name
		if project.Onboarding.Ready != (project.ID == "prj_first") || len(project.Onboarding.Steps) != 4 {
			t.Fatalf("project readiness omitted real runners/checks/policy selection: %+v", project)
		}

		if project.CanWrite || project.Onboarding.Steps == nil {
			t.Fatal("viewer project listing lost access or onboarding facts")
		}
	}
	if projects["prj_first"] != "First project" || projects["prj_second"] != "Second project" {
		t.Fatal("project listing did not return both granted projects")
	}
	for _, binding := range []struct {
		name, repository, checkout, want string
		ready                            bool
	}{
		{name: "binding required"},
		{name: "checkout fallback", checkout: "example-org/fallback", want: "example-org/fallback", ready: true},
		{name: "primary repository", repository: "example-org/example-project", checkout: "example-org/other", want: "example-org/example-project", ready: true},
	} {
		t.Run(binding.name, func(t *testing.T) {
			if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
				record, err := w.ProjectRecord(t.Context(), "prj_first")
				if err != nil {
					return err
				}
				record.Settings.Repository = binding.repository
				record.Settings.CheckoutRepository = binding.checkout
				_, err = w.UpdateProjectRecord(t.Context(), record)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			setups, err := f.service.projectOnboardingBatch(t.Context(), "org_security", []appBootstrapProject{{ID: "prj_first"}})
			if err != nil {
				t.Fatal(err)
			}
			setup := setups["prj_first"]
			if setup.Repository != binding.want || setup.Ready != binding.ready || (setup.RepositoryBindingReason == nil) != binding.ready || !binding.ready && setup.Steps[1].State == "ready" {
				t.Fatalf("incorrect repository binding readiness: %+v", setup)
			}
		})
	}
	for _, mode := range []string{"customer", "local"} {
		revision := tracker.Revision(2)
		if mode == "local" {
			revision = 3
		}
		if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
			return w.SaveProjectOnboardingProgress(t.Context(), "prj_second", onboarding.Progress{Revision: revision, Repository: "existing", Doctor: true, Provider: true, Artifacts: mode, UpdatedAt: now.Format(time.RFC3339Nano)}, revision-1)
		}); err != nil {
			t.Fatal(err)
		}
		if mode == "customer" {
			requireNativeStatus(t, f.request(t, viewer, http.MethodGet, path, nil), http.StatusServiceUnavailable)
		}
	}
	if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		checks, err := r.(projectstore.OnboardingBatchReader).ProjectRunnerChecksForProjects(t.Context(), []tracker.ProjectID{"prj_first", "prj_second"}, 10000)
		if err != nil {
			return err
		}
		for _, id := range []tracker.ProjectID{"prj_first", "prj_second"} {
			check := checks[id]["runner_"+string(id)]
			if !check.Passed() || !slices.Equal(check.ProviderKinds, []string{"codex"}) {
				t.Errorf("full checks lost for %s: %+v", id, check)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessMember(t.Context(), viewer.identity.Subject, now.Add(time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, f.request(t, viewer, http.MethodGet, path, nil), http.StatusForbidden)
}

type projectListReadRepository struct {
	tenantstore.Repository
	reads  atomic.Int64
	active sync.Map
}

func (r *projectListReadRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	timing := requestTimingFrom(ctx)
	if timing == nil {
		return r.Repository.ReadTenant(ctx, organization, workload, read)
	}
	r.reads.Add(1)
	node, _ := r.active.LoadOrStore(timing, &atomic.Int64{})
	active := node.(*atomic.Int64)
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error { active.Add(1); defer active.Add(-1); return read(reader) })
}
