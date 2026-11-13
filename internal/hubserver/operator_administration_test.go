package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (p *hostedSecurityProvider) InvitationByID(ctx context.Context, id string) (auth.Invitation, error) {
	return p.Invitation(ctx, id)
}
func (p *hostedSecurityProvider) AcceptInvitationByID(ctx context.Context, id, user string) error {
	return p.AcceptInvitation(ctx, id, user)
}

func TestNativeCredentialAdministration(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.November, 12, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	f := newNativeFixture(t, newClockedPostgresNativeService(t, Config{Clock: func() time.Time { return time.Unix(0, clock.Load()).UTC() }}), "", "administration")
	ctxs := make(chan context.Context, 1)
	f.service.echo.POST("/api/v2/organizations/:organization/administration-test", func(c echo.Context) error {
		ctxs <- operatortool.BindConnection(c.Request().Context(), "native-admin-"+c.Param("organization"), "fixture")
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/administration-test", testHubAdminToken, nil), http.StatusOK)
	ctx := <-ctxs
	e := f.service.administration
	dispatch := hostedOperatorExecutor{f.service}
	if err := dispatch.OpenConnection(ctx); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name             string
		expired, revoked bool
	}{{name: "current credential"}, {name: "expiry boundary", expired: true}, {name: "revoked credential", revoked: true}} {
		t.Run(test.name, func(t *testing.T) {
			clock.Store(now.UnixNano())
			t.Cleanup(func() { clock.Store(now.UnixNano()) })
			token, err := f.service.createAPITokenFor(t.Context(), hubapi.TokenRequest{Name: "Example administration " + test.name, Scope: hubapi.APIScopeAdmin, ExpiresAt: new(now.Add(time.Hour))})
			if err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/administration-test", token.Token, nil), http.StatusOK)
			original := <-ctxs
			connection := operatortool.CurrentConnection(original)
			authority, err := connection.Resolve(original)
			if err != nil || authority.Identity != connection.Identity {
				t.Fatalf("original connection authority did not match: error=%v", err)
			}
			resolveCalls := 0
			connection.Resolve = func(context.Context) (operatortool.Authority, error) {
				resolveCalls++
				return authority, nil
			}
			bound := operatortool.WithConnection(context.WithValue(original, hubOperatorResolverKey{}, nil), connection)
			if test.expired {
				clock.Store(now.Add(time.Hour).UnixNano())
			}
			if test.revoked {
				if err := f.service.revokeAPITokenFor(t.Context(), token.ID); err != nil {
					t.Fatal(err)
				}
			}
			current, err := (hubAdministration{f.service}).credential(bound)
			if resolveCalls != 1 {
				t.Fatalf("administration did not revalidate after the bound authority: calls=%d", resolveCalls)
			}
			for _, connection := range []context.Context{bound, original} {
				_, listErr := e.App.Read(connection, operatortool.CredentialList, operatoradmin.Input{Limit: 1})
				if test.expired || test.revoked {
					if !errors.Is(listErr, operatortool.ErrAccessDenied) {
						t.Fatalf("invalid original credential read metadata: %v", listErr)
					}
				} else if listErr != nil {
					t.Fatalf("current original credential could not read metadata: %v", listErr)
				}
			}
			if test.expired || test.revoked {
				if !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("invalid original credential resolved: id=%s error=%v", current.ID, err)
				}
				if _, err := e.App.Read(bound, operatortool.OrganizationSession, operatoradmin.Input{}); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("invalid credential read administration session: %v", err)
				}
				if _, err := (hubAdministration{f.service}).credential(original); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("invalid credential retained original connection authority: %v", err)
				}
				if _, err := e.App.Read(original, operatortool.OrganizationSession, operatoradmin.Input{}); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("invalid original connection read administration session: %v", err)
				}
				return
			}
			if err != nil || current.ID != token.ID || current.Scope != hubapi.APIScopeAdmin || current.NativeOnly {
				t.Fatalf("original binding was not revalidated: id=%s scope=%s native=%v error=%v", current.ID, current.Scope, current.NativeOnly, err)
			}
			value, err := e.App.Read(bound, operatortool.OrganizationSession, operatoradmin.Input{})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var session struct {
				Principal    string `json:"principal_id"`
				Organization string `json:"organization_id"`
			}
			if err := json.Unmarshal(raw, &session); err != nil || session.Principal != token.ID || session.Organization != string(f.project.OrganizationID) {
				t.Fatalf("administration session lost bound identity: %+v error=%v", session, err)
			}
		})
	}
	args, _ := json.Marshal(operatoradmin.Input{RequestID: "native-key", Name: "native scoped key", Scopes: []string{"write"}, ProjectIDs: []string{string(f.project.ID)}})
	if _, err := dispatch.Execute(ctx, operatortool.Call{Name: operatortool.CredentialCreate, Arguments: args}); !errors.Is(err, operatoradmin.ErrUnavailable) {
		t.Fatalf("missing browser approver=%v", err)
	}
	// The existing native API still uses the extracted application commands.
	token, err := f.service.createAPITokenFor(t.Context(), hubapi.TokenRequest{Name: "native scoped key", Scope: hubapi.APIScopeOperator})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.grantNativeTokenFor(t.Context(), token.ID, string(f.project.OrganizationID), string(f.project.ID)); err != nil {
		t.Fatal(err)
	}
	if err := f.service.accessRead(t.Context(), f.project.OrganizationID, func(r tenantstore.Reader) error {
		row, err := r.AccessCredential(t.Context(), token.ID)
		if err != nil {
			return err
		}
		if row.OrganizationID != f.project.OrganizationID || row.ID != token.ID || !row.NativeOnly || row.Scope != string(hubapi.APIScopeOperator) {
			t.Fatalf("native grant did not persist credential scope: id=%s native=%v scope=%s", row.ID, row.NativeOnly, row.Scope)
		}
		grant, err := r.AccessTokenGrant(t.Context(), token.ID, f.project.ID)
		if err != nil {
			return err
		}
		if grant.OrganizationID != f.project.OrganizationID || grant.TokenID != token.ID || grant.ProjectID != f.project.ID {
			t.Fatalf("native grant lost tenant/token/project identity: %+v", grant)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	view, err := f.service.tokenMetadataByID(t.Context(), token.ID)
	if err != nil || !view.NativeOnly || len(view.Grants) != 1 || view.Grants[0].ProjectID != string(f.project.ID) || view.Token != "" {
		t.Fatalf("grant metadata=%+v %v", view, err)
	}
	t.Run("credential metadata grant pages", func(t *testing.T) {
		scopes := []struct {
			name         string
			organization tracker.OrganizationID
			context      context.Context
		}{
			{name: "first", organization: f.project.OrganizationID},
			{name: "second", organization: f.project.OrganizationID + "_second"},
		}
		for index := range scopes {
			organization := scopes[index].organization
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(organization)+"/administration-test", testHubAdminToken, nil), http.StatusOK)
			scopes[index].context = <-ctxs
			connection := operatortool.CurrentConnection(scopes[index].context)
			if connection.Identity.OrganizationID != string(organization) || connection.ID != "native-admin-"+string(organization) {
				t.Fatalf("administration connection lost tenant: organization=%s connection=%s", connection.Identity.OrganizationID, connection.ID)
			}
		}
		for _, scope := range scopes {
			if err := f.service.accessWrite(t.Context(), scope.organization, func(w tenantstore.Writer) error {
				for _, id := range []string{"metadata_empty", "metadata_paged"} {
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: scope.organization, ID: id, Name: id + " " + string(scope.organization), Hash: id + "_" + string(scope.organization), Fingerprint: "fingerprint_" + string(scope.organization), Scope: "operator", CreatedAt: now, UpdatedAt: now, ProjectAccess: "selected", Kind: "service"}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, test := range []struct {
			name, id                string
			from, to                int
			firstCount, secondCount int
		}{
			{name: "empty", id: "metadata_empty"},
			{name: "full page", id: "metadata_paged", to: 200, firstCount: 200, secondCount: 1},
			{name: "next page", id: "metadata_paged", from: 200, to: 201, firstCount: 201, secondCount: 1},
		} {
			t.Run(test.name, func(t *testing.T) {
				for scopeIndex, scope := range scopes {
					from, to, wantCount := test.from, test.to, test.firstCount
					if scopeIndex == 1 {
						from, to, wantCount = min(from, 1), min(to, 1), test.secondCount
					}
					if err := f.service.accessWrite(t.Context(), scope.organization, func(w tenantstore.Writer) error {
						for index := from; index < to; index++ {
							project := tracker.ProjectID(fmt.Sprintf("prj_metadata_%03d", index))
							if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: scope.organization, ID: project, Name: fmt.Sprintf("Metadata project %03d", index), Slug: fmt.Sprintf("metadata-%03d", index), Profile: "native"}); err != nil {
								return err
							}
							if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: scope.organization, TokenID: test.id, ProjectID: project}); err != nil {
								return err
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if err := e.App.Authorize(scope.context, operatortool.CredentialList, operatoradmin.Input{CredentialID: test.id}, ""); err != nil {
						t.Fatalf("%s tenant credential target authorization=%v", scope.name, err)
					}
					preview, err := e.App.Preview(scope.context, operatortool.CredentialGrant, operatoradmin.Input{CredentialID: test.id})
					if err != nil || preview.ResourceID != test.id {
						t.Fatalf("%s tenant credential preview: resource=%s error=%v", scope.name, preview.ResourceID, err)
					}
					encoded, err := json.Marshal(preview.Current)
					if err != nil {
						t.Fatal(err)
					}
					var current struct {
						Credential hubapi.TokenResponse `json:"credential"`
					}
					if err := json.Unmarshal(encoded, &current); err != nil {
						t.Fatal(err)
					}
					view := current.Credential
					if err != nil || view.ID != test.id || view.Name != test.id+" "+string(scope.organization) || view.Fingerprint != "fingerprint_"+string(scope.organization) || view.Scope != hubapi.APIScopeOperator || view.Kind != "service" || view.NativeOnly || view.Token != "" || !view.CreatedAt.Equal(now) || view.Grants == nil || view.Projects == nil || len(view.Grants) != wantCount || len(view.Projects) != wantCount {
						t.Fatalf("%s tenant metadata: id=%s grants=%d projects=%d error=%v", scope.name, view.ID, len(view.Grants), len(view.Projects), err)
					}
					for index := range wantCount {
						project := fmt.Sprintf("prj_metadata_%03d", index)
						if view.Grants[index].OrganizationID != string(scope.organization) || view.Grants[index].ProjectID != project || view.Projects[index] != project {
							t.Fatalf("%s metadata lost ordered tenant grant %d: grant=%+v project=%s", scope.name, index, view.Grants[index], view.Projects[index])
						}
					}
				}
			})
		}
		for scopeIndex, scope := range scopes {
			fixture := newNativeFixture(t, f.service, scope.organization, "metadata-list")
			runner := prepareRunner(t, fixture, runnerauth.Read, runnerauth.Heartbeat)
			runner.enroll(t)
			var runnerCredential string
			if err := f.service.accessRead(t.Context(), scope.organization, func(r tenantstore.Reader) error {
				identity, err := r.RunnerIdentity(t.Context(), runner.binding.RunnerID)
				runnerCredential = identity.TokenID
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.service.accessWrite(t.Context(), scope.organization, func(w tenantstore.Writer) error {
				for _, id := range []string{"000_metadata_a", "000_metadata_b", "000_metadata_c"} {
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: scope.organization, ID: id, Name: id + " " + string(scope.organization), Hash: "secret_" + id + "_" + string(scope.organization), Fingerprint: "listed_" + string(scope.organization), Scope: "operator", CreatedAt: now, UpdatedAt: now, ProjectAccess: "selected", Kind: "service"}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name                string
				offset, limit, next int
				ids                 []string
			}{
				{name: "first credential page", limit: 2, ids: []string{"000_metadata_a", "000_metadata_b"}, next: 2},
				{name: "credential page continuation", offset: 2, limit: 1, ids: []string{"000_metadata_c"}, next: 3},
				{name: "credential offset past end", offset: 100000, limit: 200},
			} {
				t.Run(scope.name+"/"+test.name, func(t *testing.T) {
					data, err := e.App.Read(scope.context, operatortool.CredentialList, operatoradmin.Input{Offset: test.offset, Limit: test.limit})
					if err != nil {
						t.Fatal(err)
					}
					page, ok := data.(operatoradmin.PageResult[hubapi.TokenResponse])
					if !ok || len(page.Items) != len(test.ids) || (page.NextOffset == nil) != (test.next == 0) || page.NextOffset != nil && *page.NextOffset != test.next {
						t.Fatalf("credential continuation=%+v want ids=%v next=%d", data, test.ids, test.next)
					}
					for index, item := range page.Items {
						if item.ID != test.ids[index] || item.Name != item.ID+" "+string(scope.organization) || item.Fingerprint != "listed_"+string(scope.organization) || item.Token != "" {
							t.Fatalf("credential list escaped connection tenant or exposed token: %+v", item)
						}
					}
				})
			}
			data, err := e.App.Read(scope.context, operatortool.CredentialList, operatoradmin.Input{Limit: 200})
			if err != nil {
				t.Fatal(err)
			}
			page, ok := data.(operatoradmin.PageResult[hubapi.TokenResponse])
			if !ok || page.NextOffset != nil {
				t.Fatalf("bounded credential fixture unexpectedly continued: %+v", data)
			}
			found := false
			for _, item := range page.Items {
				if item.ID == runnerCredential {
					t.Fatal("runner credential appeared in administration metadata")
				}
				if item.ID == "metadata_paged" {
					found = true
					want := []int{201, 1}[scopeIndex]
					if item.Name != "metadata_paged "+string(scope.organization) || item.Fingerprint != "fingerprint_"+string(scope.organization) || len(item.Grants) != want || len(item.Projects) != want || item.Token != "" {
						t.Fatalf("credential list lost complete tenant metadata: %+v", item)
					}
					for index, grant := range item.Grants {
						project := fmt.Sprintf("prj_metadata_%03d", index)
						if grant.OrganizationID != string(scope.organization) || grant.ProjectID != project || item.Projects[index] != project {
							t.Fatalf("credential list lost ordered tenant grant %d: %+v", index, grant)
						}
					}
				}
			}
			if !found {
				t.Fatal("credential list omitted grant metadata fixture")
			}
			encoded, err := json.Marshal(page)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{runnerCredential, runner.redemption.Credential, "secret_000_metadata_a_" + string(scope.organization), "metadata_paged_" + string(scope.organization), "token_hash"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("credential metadata exposed secret field %q", secret)
				}
			}
		}
		configured, err := f.service.tokenMetadataByID(t.Context(), "metadata_paged")
		if err != nil || configured.Name != "metadata_paged "+string(f.project.OrganizationID) || configured.Fingerprint != "fingerprint_"+string(f.project.OrganizationID) || len(configured.Grants) != 201 || len(configured.Projects) != 201 {
			t.Fatalf("configured credential metadata changed: id=%s grants=%d projects=%d error=%v", configured.ID, len(configured.Grants), len(configured.Projects), err)
		}
		for _, scope := range scopes {
			if _, err := e.App.Preview(scope.context, operatortool.CredentialGrant, operatoradmin.Input{CredentialID: "metadata_missing"}); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("%s tenant missing metadata=%v", scope.name, err)
			}
		}
	})
	// The newly scoped token has write powers on its project, no administration.
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/administration-test", token.Token, nil), http.StatusOK)
	ctx = <-ctxs
	if err := e.App.Authorize(ctx, operatortool.CredentialCreate, operatoradmin.Input{}, ""); !errors.Is(err, operatortool.ErrAccessDenied) {
		t.Fatalf("scoped admin=%v", err)
	}
	if _, err := e.App.Read(ctx, operatortool.CredentialList, operatoradmin.Input{Limit: 1}); !errors.Is(err, operatortool.ErrAccessDenied) {
		t.Fatalf("native-only credential read administration metadata: %v", err)
	}
	t.Run("organization pages preserve instance and native scope", func(t *testing.T) {
		created := f.service.config.Clock().UTC().Add(-time.Minute)
		const pageToken = "example_instance_organization_page_token"
		if err := f.service.accessWrite(t.Context(), f.project.OrganizationID, func(w tenantstore.Writer) error {
			return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: "organization_page_admin", Name: "Example instance admin", Hash: apikey.HashToken(pageToken), Scope: "admin", CreatedAt: created, UpdatedAt: created, ProjectAccess: "all", Kind: "service"})
		}); err != nil {
			t.Fatal(err)
		}
		for index, organization := range []tracker.OrganizationID{"000_org_a", "000_org_b", "000_org_c"} {
			if err := f.service.accessWrite(t.Context(), organization, func(w tenantstore.Writer) error {
				return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: fmt.Sprintf("Example organization %d", index), CreatedAt: created})
			}); err != nil {
				t.Fatal(err)
			}
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/administration-test", pageToken, nil), http.StatusOK)
		instanceContext := <-ctxs
		for _, test := range []struct {
			name                string
			context             context.Context
			offset, limit, next int
			ids                 []tracker.OrganizationID
		}{
			{name: "instance first page", context: instanceContext, limit: 2, next: 2, ids: []tracker.OrganizationID{"000_org_a", "000_org_b"}},
			{name: "instance continuation", context: instanceContext, offset: 2, limit: 1, next: 3, ids: []tracker.OrganizationID{"000_org_c"}},
			{name: "instance past end", context: instanceContext, offset: 100000, limit: 200},
			{name: "native own organization", context: ctx, limit: 1, ids: []tracker.OrganizationID{f.project.OrganizationID}},
			{name: "native past own organization", context: ctx, offset: 1, limit: 1},
		} {
			t.Run(test.name, func(t *testing.T) {
				if err := e.App.Authorize(test.context, operatortool.OrganizationList, operatoradmin.Input{}, ""); err != nil {
					t.Fatal(err)
				}
				data, err := e.App.Read(test.context, operatortool.OrganizationList, operatoradmin.Input{Offset: test.offset, Limit: test.limit})
				if err != nil {
					t.Fatal(err)
				}
				page, ok := data.(operatoradmin.PageResult[nativeOrganization])
				if !ok || len(page.Items) != len(test.ids) || (page.NextOffset == nil) != (test.next == 0) || page.NextOffset != nil && *page.NextOffset != test.next {
					t.Fatalf("organization page=%+v want=%v next=%d", data, test.ids, test.next)
				}
				for index, item := range page.Items {
					if item.ID != test.ids[index] || item.Name == "" {
						t.Fatalf("organization visibility/order changed: %+v", item)
					}
					if strings.HasPrefix(string(item.ID), "000_org_") && (item.Name != fmt.Sprintf("Example organization %d", test.offset+index) || item.Local) {
						t.Fatalf("organization metadata changed: %+v", item)
					}
				}
			})
		}
	})
	if err := f.service.revokeAPITokenFor(t.Context(), token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatch.Execute(ctx, operatortool.Call{Name: operatortool.OrganizationSession, Arguments: json.RawMessage(`{}`)}); !errors.Is(err, operatortool.ErrAccessDenied) {
		t.Fatalf("revoked token read=%v", err)
	}
}
