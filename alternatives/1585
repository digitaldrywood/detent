package hubserver

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, organization := range organizations {
		scope := nativeScope{organization: organization, project: "project_first", credential: apiCredential{ID: "owner", Hash: string(organization), Scope: apiScopeOperator}}
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
