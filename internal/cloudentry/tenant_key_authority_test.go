package cloudentry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
)

func TestTenantKeyAuthorityStoreFailure(t *testing.T) {
	registry := openTestRegistry(t)
	s := &Service{registry: registry, auth: &authStore{store: registry.store}, config: Config{Issuer: "entry", now: time.Now}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.resolveTenantKey(ctx, Organization{ID: "org_example", Generation: 1}, entryauthority.Origin{Issuer: "entry", Generation: 1, KeyID: "key_example", Subject: "owner", CredentialBinding: strings.Repeat("a", 64)})
	if !errors.Is(err, context.Canceled) || errors.Is(err, cloudassert.ErrInvalid) {
		t.Fatalf("store outage classified as denial: %v", err)
	}
}

func TestTenantKeyAuthorityConcurrentClaims(t *testing.T) {
	registry := openTestRegistry(t)
	provider := newFakeProvider()
	s := &Service{registry: registry, auth: &authStore{store: registry.store}, config: Config{Issuer: "entry", Provider: provider, now: time.Now}}
	var allocations []Organization
	var origins []entryauthority.Origin
	for _, suffix := range []string{"first", "second"} {
		org := Organization{ID: "org_" + suffix, ProviderID: "provider_" + suffix, Name: "Example", State: "ready", Endpoint: "unix:/example/" + suffix + ".sock", Generation: 1}
		if _, err := registry.Register(t.Context(), org); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET managed=true WHERE id=?", org.ID); err != nil {
			t.Fatal(err)
		}
		provider.member("owner", org.ProviderID, "member")
		for _, kind := range []string{"service", "personal"} {
			k := accessKey{ID: "key_" + kind + "_" + suffix, Hash: "hash_" + kind + "_" + suffix, Name: "Example", Kind: kind, Owner: "owner", Permission: apikey.ScopeWrite, AccessContext: "selected", Organizations: []keyOrganization{{OrganizationID: org.ID, ProjectContext: apikey.ProjectContext{Access: "all"}}}}
			subject := "owner"
			if kind == "service" {
				k.ServiceOrganization = org.ID
				subject = "service:" + k.ID
			}
			if err := s.auth.postgresCreateKey(t.Context(), k); err != nil {
				t.Fatal(err)
			}
			allocations = append(allocations, org)
			origins = append(origins, entryauthority.Origin{KeyID: k.ID, CredentialBinding: k.credentialBinding(), Subject: subject, Issuer: "entry", Generation: 1})
		}
	}
	var group sync.WaitGroup
	started := time.Now()
	for i := range 64 {
		group.Go(func() {
			index := i % len(origins)
			claims, err := s.resolveTenantKey(t.Context(), allocations[index], origins[index])
			if err != nil {
				t.Errorf("resolve claims: %v", err)
				return
			}
			if claims.Audience != allocations[index].ID || claims.Subject != origins[index].Subject || claims.Key.ID != origins[index].KeyID {
				t.Error("mixed tenant authority")
			}
			role := "admin"
			if claims.Key.Kind == "personal" {
				role = "member"
			}
			if claims.Role != role {
				t.Errorf("role=%s, want %s", claims.Role, role)
			}
		})
	}
	group.Wait()
	t.Logf("64 full authority resolutions / 2 tenants / service and personal keys: %s", time.Since(started))
}
