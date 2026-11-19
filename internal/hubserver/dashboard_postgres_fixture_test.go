package hubserver

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func controlledPostgresDelivery(t *testing.T, cfg hubdeps.DashboardDelivery, configure ...func(*pgxpool.Config)) (hostedSecurityFixture, hostedSecurityUser, *dashboardDelivery) {
	t.Helper()
	repository, _ := projectRecordPostgresWithAdmin(t, 3, configure...)
	f := accountPreferencesPostgresFixture(t, repository)
	f.project = "prj_security"
	f.base = "/api/v2/organizations/org_security/projects/prj_security"
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, org := range []tracker.OrganizationID{"org_security", "org_delivery_other"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "org_provider"}); err != nil {
				return err
			}
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: f.project, Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	user := f.user(t, "stream-member", "member", "stream@example.test", "", "")
	dashboardPostgresGrant(t, f, user)
	if cfg.NewTimer == nil {
		cfg.NewTimer = func(time.Duration) hubdeps.Timer {
			return hubdeps.Timer{C: make(chan time.Time), Reset: func(time.Duration) {}, Stop: func() {}}
		}
	}
	f.service.config.Deps.DashboardDelivery = &cfg
	f.service.database.dashboardCommitted = boardCommitChannel(repository)
	d := newDashboardDelivery(f.service)
	f.service.dashboard = d
	t.Cleanup(d.stop)
	admin := usagePostgresAdmin(t, f.service)
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_board_sequences(organization_id,project_id,sequence,replay_floor) VALUES('org_delivery_other',$1,100,0)`, f.project); err != nil {
		t.Fatal(err)
	}
	return f, user, d
}

func dashboardPostgresGrant(t *testing.T, f hostedSecurityFixture, user hostedSecurityUser) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), user.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_security", UserID: member.UserID, ProjectID: f.project, CanWrite: true}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_security", TokenID: member.PrincipalID, ProjectID: f.project})
	}); err != nil {
		t.Fatal(err)
	}
}

func dashboardPostgresProject(t *testing.T, f hostedSecurityFixture, user hostedSecurityUser) hostedSecurityFixture {
	t.Helper()
	other := f
	other.project = "prj_stream_other"
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_security", ID: other.project, Name: "Other", Slug: "other", Profile: "native"}, States: nativeFixtureStates()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dashboardPostgresGrant(t, other, user)
	return other
}

func dashboardPostgresHead(t *testing.T, f hostedSecurityFixture) int64 {
	t.Helper()
	var head int64
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.BoardSequence(t.Context(), f.project)
		head = row.Sequence
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return head
}

func dashboardPostgresAppend(t *testing.T, f hostedSecurityFixture, name string) int64 {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		number, err := w.NextWorkItemNumber(t.Context(), f.project)
		if err != nil {
			return err
		}
		_, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{
			OrganizationID: "org_security", ProjectID: f.project, NativeID: tracker.NativeWorkItemID("wi_" + name),
			Number: number, Title: name, WorkflowStateName: new("Todo"), CreatedAt: f.service.config.Clock(),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return dashboardPostgresHead(t, f)
}

func dashboardPostgresLane(ctx context.Context, w tenantstore.Writer, project tracker.ProjectID) error {
	record, err := w.ProjectRecord(ctx, project)
	if err != nil {
		return err
	}
	record.Core.RequireDependencies = !record.Core.RequireDependencies
	_, err = w.UpdateProjectRecord(ctx, record)
	return err
}

func appendDashboardPostgresLane(t *testing.T, f hostedSecurityFixture) int64 {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		return dashboardPostgresLane(t.Context(), w, f.project)
	}); err != nil {
		t.Fatal(err)
	}
	return dashboardPostgresHead(t, f)
}

func dashboardPostgresIssue(t *testing.T, f hostedSecurityFixture, number int64) tracker.NativeWorkItemID {
	t.Helper()
	id := tracker.NativeWorkItemID(fmt.Sprintf("wi_example_%d", number))
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: "org_security", ProjectID: f.project, NativeID: id, Number: number, Title: "Example", WorkflowStateName: new("Todo"), CreatedAt: f.service.config.Clock()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}
