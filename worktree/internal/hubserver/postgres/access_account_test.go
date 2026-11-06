package postgres_test

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAccessAccountReceiptsAndProviderBindings(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: historyAt()}); err != nil {
				return err
			}
			owner := w
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org}); err != nil {
				return err
			}
			if err := owner.BindAccessTenantProvider(t.Context(), "provider_"+string(org)); err != nil {
				return err
			}
			if err := owner.BindAccessTenantProvider(t.Context(), "provider_"+string(org)); err != nil {
				return err
			}
			if err := owner.BindAccessTenantProvider(t.Context(), "other"); err == nil {
				t.Error("provider binding overwritten")
			}
			for _, event := range []string{"pending", "succeeded"} {
				if _, err := w.RecordAccessAudit(t.Context(), accessstore.Audit{OrganizationID: org, ActualActor: "actor", Event: event, AccountKind: "account_administration", AccountRetry: "retry", RecordedAt: historyAt()}); err != nil {
					return err
				}
			}
			row, err := owner.AccessAccountAudit(t.Context(), "actor", "retry")
			if err != nil {
				return err
			}
			if row.OrganizationID != org || row.Event != "succeeded" {
				t.Errorf("receipt=%+v", row)
			}
			if _, err := owner.AccessAccountAudit(t.Context(), "another-actor", "retry"); !errors.Is(err, corestore.ErrNotFound) {
				t.Errorf("foreign actor error=%v", err)
			}

			if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: "prj_example", Name: "Example", Profile: "native"}); err != nil {
				return err
			}
			if missing, err := w.AccessMissingUsageGrant(t.Context(), "actor"); err != nil || !missing {
				t.Fatalf("missing grant=%v error=%v", missing, err)
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "actor", Name: "Actor", Hash: "actor-hash", Scope: "operator", NativeOnly: true, CreatedAt: historyAt(), UpdatedAt: historyAt(), ProjectAccess: "selected", Kind: "personal"}); err != nil {
				return err
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: "actor", Email: "actor@example.test", MembershipID: "membership", Role: "admin", Active: true, PrincipalID: "actor", CreatedAt: historyAt(), UpdatedAt: historyAt()}); err != nil {
				return err
			}
			if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: "actor", ProjectID: "prj_example"}); err != nil {
				return err
			}
			if missing, err := w.AccessMissingUsageGrant(t.Context(), "actor"); err != nil || missing {
				t.Fatalf("granted access missing=%v error=%v", missing, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
