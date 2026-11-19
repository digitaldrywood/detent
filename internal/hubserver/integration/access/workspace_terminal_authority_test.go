package access_test

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacerelay"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestRefuseTerminalGrantMatrix(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	repository := hubtest.Repository(t, hubtest.WithClock(func() time.Time { return now }))
	first := hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: "org_terminal_first", Now: now})
	second := hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: "org_terminal_second", Now: now})
	projects := []tracker.NativeProject{
		hubtest.Project(t, repository, first.OrganizationID, hubtest.ProjectOptions{Name: "First terminal grant project"}),
		hubtest.Project(t, repository, second.OrganizationID, hubtest.ProjectOptions{Name: "Second terminal grant project"}),
	}

	tests := []struct {
		name            string
		role            string
		cachedRole      string
		inactive, token bool
		canWrite        bool
		runners         bool
		isolation       string
		enabled         bool
		support         string
		want            string
	}{
		{
			name: "a member with write and the runners grant may type",
			role: "member", canWrite: true, runners: true,
			isolation: workspacesession.IsolationContainer, enabled: true,
		},
		{
			name: "a viewer never does", role: "viewer", canWrite: true, runners: true,
			isolation: workspacesession.IsolationContainer, enabled: true,
			want: workspacesession.CodeForbidden,
		},
		{
			name: "inactive member with write and runners", role: "member", canWrite: true, runners: true, inactive: true,
			isolation: workspacesession.IsolationContainer, enabled: true, want: workspacesession.CodeForbidden,
		},
		{
			name: "current viewer overrides cached owner", role: "viewer", cachedRole: "owner", canWrite: true, runners: true,
			isolation: workspacesession.IsolationUser, enabled: true, want: workspacesession.CodeForbidden,
		},
		{
			name: "operator token retains its proved scope", token: true,
			isolation: workspacesession.IsolationUser, enabled: true,
		},
		{
			name: "a member without the runners grant does not", role: "member", canWrite: true, runners: false,
			isolation: workspacesession.IsolationContainer, enabled: true,
			want: workspacesession.CodeForbidden,
		},
		{
			name: "a member with read and no write does not", role: "member", canWrite: false, runners: true,
			isolation: workspacesession.IsolationContainer, enabled: true,
			want: workspacesession.CodeForbidden,
		},
		{
			name: "the setting off refuses everybody", role: "owner", canWrite: true, runners: true,
			isolation: workspacesession.IsolationContainer, enabled: false,
			want: workspacesession.CodeForbidden,
		},
		{
			name: "user isolation refuses a member", role: "member", canWrite: true, runners: true,
			isolation: workspacesession.IsolationUser, enabled: true,
			want: workspacesession.CodeForbidden,
		},
		{
			name: "user isolation allows an admin", role: "admin", canWrite: true, runners: true,
			isolation: workspacesession.IsolationUser, enabled: true,
		},
		{
			name: "user isolation allows an owner", role: "owner", canWrite: true, runners: true,
			isolation: workspacesession.IsolationUser, enabled: true,
		},
		{
			name: "a support session is refused off container", role: "owner", canWrite: true, runners: true,
			isolation: workspacesession.IsolationUser, enabled: true, support: "ticket-9",
			want: workspacesession.CodeForbidden,
		},
		{
			name: "a support session at container is allowed", role: "admin", canWrite: true, runners: true,
			isolation: workspacesession.IsolationContainer, enabled: true, support: "ticket-9",
		},
	}

	for caseIndex, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for _, project := range projects {
				relay := &workspacerelay.HTTP{HTTPDeps: workspacerelay.HTTPDeps{
					TerminalConfig: func() workspacehub.TerminalConfig {
						return workspacehub.TerminalConfig{Enabled: tt.enabled, Isolation: tt.isolation}
					},
					Logger: slog.New(slog.DiscardHandler),
					AccessRead: func(ctx context.Context, organization tracker.OrganizationID, read func(tenantstore.Reader) error) error {
						return repository.ReadTenant(ctx, organization, corestore.Serving, read)
					},
				}}
				subject := fmt.Sprintf("user_terminal_%02d", caseIndex)
				principal := "principal_" + subject
				if !tt.token {
					if err := repository.WriteTenant(t.Context(), project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
						if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: project.OrganizationID, ID: principal, Name: principal, Hash: apikey.HashToken(string(project.OrganizationID) + principal), Scope: "operator", NativeOnly: true, Kind: "personal", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
							return err
						}
						if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: project.OrganizationID, UserID: subject, Email: subject + "@example.test", MembershipID: "membership_" + subject, Role: tt.role, Active: !tt.inactive, PrincipalID: principal, CreatedAt: now, UpdatedAt: now}); err != nil {
							return err
						}
						return w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: project.OrganizationID, UserID: subject, ProjectID: project.ID, CanWrite: tt.canWrite, ManageRunner: tt.runners})
					}); err != nil {
						t.Fatal(err)
					}
				}
				cachedRole := tt.cachedRole
				if cachedRole == "" {
					cachedRole = tt.role
				}
				record := workspacehub.Record{OrganizationID: project.OrganizationID, ProjectID: project.ID}
				connection := &workspacerelay.Connection{ID: "relayconn_test", Subject: subject, HostedRole: cachedRole, SupportReason: tt.support}
				if !tt.token {
					connection.SessionHash = "hash_" + subject
				}
				got := relay.RefuseTerminal(t.Context(), record, connection, workspacesession.Frame{Channel: workspacesession.ChannelTerminal, Type: workspacesession.TypeTerminalOpen})
				if got != tt.want {
					t.Fatalf("refuseTerminal() = %q, want %q", got, tt.want)
				}
			}
		})
	}
}
