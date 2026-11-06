package hubserver

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/emailtemplate"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDailySummaryUsesTenantOrganizationWithoutLegacyDatabase(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, id := range []tracker.OrganizationID{"org_summary_first", "org_summary_second"} {
		if err := repository.WriteTenant(t.Context(), id, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: id, Name: string(id), Slug: string(id), CreatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{config: Config{TenantRepository: repository, Hosted: &HostedConfig{PublicURL: "https://example.test"}}}
	for _, id := range []string{"org_summary_first", "org_summary_second"} {
		data, quiet, err := service.buildDailySummary(t.Context(), summaryRecipient{organization: id, user: "example", role: "member", zone: time.UTC, end: now})
		if err != nil {
			t.Fatal(err)
		}
		if data.Organization != id || !quiet || data.BoardURL != "https://example.test/"+id+"/work" {
			t.Fatalf("summary=%+v quiet=%t", data, quiet)
		}
	}
}

func TestDailySummaryFiltersProjectsAndReporterAttention(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_summary_first", "org_summary_second"} {
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "principal_example", Name: "Example", Hash: "example_hash", Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ProjectAccess: "selected", Kind: "personal"}); err != nil {
				return err
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: "example", Email: "example@example.test", MembershipID: string(org) + "_member", Role: "member", Active: true, PrincipalID: "principal_example", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			for _, name := range []string{"visible", "private-project"} {
				project := tracker.ProjectID(string(org) + "_" + name)
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: name, Slug: name, Profile: "native"}, States: []tracker.NativeState{{Name: "Human Review"}, {Name: "In Progress"}}}); err != nil {
					return err
				}
				if name == "visible" {
					if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: "example", ProjectID: project}); err != nil {
						return err
					}
				}
				for i, actor := range []string{"principal_example", "principal_other"} {
					kind, state := "human", "Human Review"
					if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: tracker.NativeWorkItemID(string(project) + actor), Number: int64(i + 1), Title: name + " " + actor, ActorKind: &kind, ActorPrincipalID: &actor, WorkflowStateName: &state, NativeCreatedAt: now.Add(-time.Hour), CreatedAt: now.Add(-time.Hour), NativeUpdatedAt: now}); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{config: Config{TenantRepository: repository, Hosted: &HostedConfig{PublicURL: "https://example.test"}}}
	for _, org := range []string{"org_summary_first", "org_summary_second"} {
		for _, test := range []struct {
			role string
			want int
		}{{"owner", 2}, {"member", 2}, {"reporter", 1}} {
			data, quiet, err := service.buildDailySummary(t.Context(), summaryRecipient{organization: org, user: "example", principal: "principal_example", role: test.role, zone: time.UTC, end: now})
			if err != nil {
				t.Fatal(err)
			}
			if quiet || len(data.Needs) != test.want || len(data.Projects) != 1 || data.Projects[0].Name != "visible" {
				t.Fatalf("summary=%+v quiet=%t", data, quiet)
			}
			message, err := emailtemplate.RenderDailySummary(data)
			if err != nil {
				t.Fatal(err)
			}
			for _, part := range []string{message.Body, message.HTMLBody} {
				if strings.Contains(part, "private-project") || test.role == "reporter" && (strings.Contains(part, "principal_other") || strings.Contains(part, "$")) {
					t.Fatalf("summary disclosed hidden data: %s", part)
				}
			}
		}
	}
}
