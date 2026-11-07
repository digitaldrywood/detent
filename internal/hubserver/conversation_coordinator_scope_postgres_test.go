package hubserver

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConversationCoordinatorRejectsRecordOutsideScope(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := &Service{config: Config{TenantRepository: repository, now: func() time.Time { return now }}}
	coordinator := &conversationTurnCoordinator{service: &conversationService{server: service}}
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	for _, organization := range organizations {
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: string(organization), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "owner", Name: "Example owner", Hash: string(organization), Scope: "operator", ProjectAccess: "all", Kind: "personal", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{"project_first", "project_second"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Slug: strings.ReplaceAll(string(project), "_", "-"), Profile: "native", CreatedAt: now}, States: nativeFixtureStates()}); err != nil {
					return err
				}
			}
			for _, id := range []string{"runner_a", "runner_b", "runner_foreign"} {
				project := tracker.ProjectID("project_first")
				if id == "runner_foreign" {
					project = "project_second"
				}
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: id, Name: id, Hash: string(organization) + id, Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: id, ProjectID: project}); err != nil {
					return err
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: id, Hostname: id, Capacity: 1, Version: "example"}); err != nil {
					return err
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: id, RunnerID: id, MachineID: tracker.MachineID(id), TokenHash: apikey.HashToken(string(organization) + id + "enrollment"), Scope: "projects", Operations: []string{runnerauth.Read}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: "owner"}); err != nil {
					return err
				}
				if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: id, MachineID: tracker.MachineID(id), TokenID: id, EnrollmentID: id, Scope: "projects", State: "active", Operations: []string{runnerauth.Read}, CapacityLimit: 1, ReportedCapacity: 1, OS: "linux", Architecture: "arm64", CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, organization := range organizations {
		scope := nativeScope{organization: organization, project: "project_first", credential: apiCredential{ID: "owner", Hash: string(organization), Scope: apiScopeOperator}}
		for _, page := range []struct {
			args coordinatorRunnerArguments
			ids  []string
		}{
			{coordinatorRunnerArguments{}, []string{"runner_a", "runner_b"}},
			{coordinatorRunnerArguments{Limit: 1, Offset: 1}, []string{"runner_b"}},
			{coordinatorRunnerArguments{RunnerID: "runner_a"}, []string{"runner_a"}},
		} {
			runners, err := service.coordinatorRunners(t.Context(), scope, page.args)
			if err != nil {
				t.Fatal(err)
			}
			if len(runners) != len(page.ids) {
				t.Fatalf("tenant %s runners=%v want=%v", organization, runners, page.ids)
			}
			for i, runner := range runners {
				if runner.OrganizationID != organization || runner.RunnerID != page.ids[i] {
					t.Fatalf("tenant %s runner=%v want=%s", organization, runner, page.ids[i])
				}
			}
		}
		if _, err := service.coordinatorRunner(t.Context(), scope, "runner_foreign"); err == nil {
			t.Fatal("runner from another project was visible")
		}
		record := conversationRecord{OrganizationID: organization, ProjectID: scope.project, Visibility: conversation.VisibilityShared}
		for _, write := range []bool{false, true} {
			if err := coordinator.authorizeRecord(t.Context(), scope, record, write); err != nil {
				t.Fatalf("same scope %s write=%t: %v", organization, write, err)
			}
			for _, other := range organizations {
				if other == organization {
					continue
				}
				foreign := record
				foreign.OrganizationID = other
				var refusal *nativeError
				if err := coordinator.authorizeRecord(t.Context(), scope, foreign, write); !errors.As(err, &refusal) || refusal.Code != "not_found" {
					t.Errorf("foreign organization %s write=%t: %v", other, write, err)
				}
			}
			foreign := record
			foreign.ProjectID = "project_second"
			var refusal *nativeError
			if err := coordinator.authorizeRecord(t.Context(), scope, foreign, write); !errors.As(err, &refusal) || refusal.Code != "not_found" {
				t.Errorf("foreign project %s write=%t: %v", organization, write, err)
			}
		}
	}
}
