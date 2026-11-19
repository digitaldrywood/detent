package hubfixture

import (
	"context"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type conversationHostedPeerProvider struct {
	*HostedProvider
	organization tracker.OrganizationID
}

func (p conversationHostedPeerProvider) Organization(_ context.Context, id string) (auth.Organization, error) {
	return auth.Organization{ID: id, ExternalID: string(p.organization), Name: "Example organization"}, nil
}

func OpenConversationHostedPeer(t *testing.T, parent *Hosted, organization tracker.OrganizationID) *Hosted {
	t.Helper()
	cfg := parent.Config
	settings := *cfg.Hosted
	provider := NewHostedProvider()
	settings.OrganizationID = string(organization)
	settings.WorkOSOrganizationID = "provider_" + string(organization)
	settings.BootstrapSubject = hubapi.NewID("owner")
	settings.Provider = conversationHostedPeerProvider{HostedProvider: provider, organization: organization}
	cfg.Hosted = &settings
	cfg.InitialAdminToken = []byte(hubapi.NewID("token"))
	now := parent.Now()
	slug := strings.ReplaceAll(string(organization), "_", "-")
	deployment, generation := settings.Deployment()
	if err := parent.Repository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := writer.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: string(organization), Slug: slug, CreatedAt: now}); err != nil {
			return err
		}
		if err := writer.AddAccessOrganizationSlug(t.Context(), slug); err != nil {
			return err
		}
		return writer.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: settings.WorkOSOrganizationID, BootstrapSubject: settings.BootstrapSubject, Deployment: deployment, PublicURL: settings.PublicURL, AllocationGeneration: generation})
	}); err != nil {
		t.Fatalf("seed hosted peer %s: %v", organization, err)
	}
	SeedAdmin(t, parent.Repository, organization, string(cfg.InitialAdminToken), now)
	hub := open(t, &Hub{Admin: parent.Admin, Primary: organization, owner: t}, cfg, parent.Repository)
	return &Hosted{Hub: hub, Provider: provider}
}
