package hubserver

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func operatorNativeTokenFixture(t *testing.T, service *Service, organization tracker.OrganizationID, scope hubapi.APIScope, name string) hubapi.TokenResponse {
	t.Helper()
	token, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	now := service.config.Clock().UTC().Truncate(time.Microsecond)
	response := hubapi.TokenResponse{ID: hubapi.NewID("credential"), Token: token, Name: name, Scope: scope}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: response.ID, Name: name, Hash: apikey.HashToken(token), Scope: string(scope), Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	return response
}
