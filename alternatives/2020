package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (p *hostedSecurityProvider) InvitationByID(ctx context.Context, id string) (auth.Invitation, error) {
	return p.Invitation(ctx, id)
}
func (p *hostedSecurityProvider) AcceptInvitationByID(ctx context.Context, id, user string) error {
	return p.AcceptInvitation(ctx, id, user)
}

func TestNativeCredentialAdministration(t *testing.T) {
	f := newNativeFixture(t, nil, "", "administration")
	ctxs := make(chan context.Context, 1)
	f.service.echo.POST("/api/v2/organizations/:organization/administration-test", func(c echo.Context) error {
		ctxs <- operatortool.BindConnection(c.Request().Context(), "native-admin", "fixture")
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/administration-test", testHubAdminToken, nil), http.StatusOK)
	ctx := <-ctxs
	e := f.service.administration
	dispatch := hostedOperatorExecutor{f.service}
	if err := dispatch.OpenConnection(ctx); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(operatoradmin.Input{RequestID: "native-key", Name: "native scoped key", Scopes: []string{"write"}, ProjectIDs: []string{string(f.project.ID)}})
	if _, err := dispatch.Execute(ctx, operatortool.Call{Name: operatortool.CredentialCreate, Arguments: args}); !errors.Is(err, operatoradmin.ErrUnavailable) {
		t.Fatalf("missing browser approver=%v", err)
	}
	// The existing native API still uses the extracted application commands.
	token, err := f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "native scoped key", Scope: apiScopeOperator})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.grantNativeTokenFor(t.Context(), token.ID, string(f.project.OrganizationID), string(f.project.ID)); err != nil {
		t.Fatal(err)
	}
	view, err := f.service.tokenMetadataByID(t.Context(), token.ID)
	if err != nil || !view.NativeOnly || len(view.Grants) != 1 || view.Grants[0].ProjectID != string(f.project.ID) || view.Token != "" {
		t.Fatalf("grant metadata=%+v %v", view, err)
	}
	// The newly scoped token has write powers on its project, no administration.
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/administration-test", token.Token, nil), http.StatusOK)
	ctx = <-ctxs
	if err := e.App.Authorize(ctx, operatortool.CredentialCreate, operatoradmin.Input{}, ""); !errors.Is(err, operatortool.ErrAccessDenied) {
		t.Fatalf("scoped admin=%v", err)
	}
	if err := f.service.revokeAPITokenFor(t.Context(), token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatch.Execute(ctx, operatortool.Call{Name: operatortool.OrganizationSession, Arguments: json.RawMessage(`{}`)}); !errors.Is(err, operatortool.ErrAccessDenied) {
		t.Fatalf("revoked token read=%v", err)
	}
}
