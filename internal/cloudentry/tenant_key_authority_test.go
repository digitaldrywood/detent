package cloudentry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/tenantauth"
)

func TestKeyAuthorityOutagesRemainRetryable(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{"missing key", sql.ErrNoRows, http.StatusUnauthorized},
		{"missing session", errNoSession, http.StatusUnauthorized},
		{"typed denial", tenantauth.ErrDenied, http.StatusForbidden},
		{"corrupt key", errors.Join(tenantauth.ErrDenied, &apikey.Refusal{Code: "key_context_invalid"}), http.StatusForbidden},
		{"cancelled query", context.Canceled, http.StatusServiceUnavailable},
		{"storage outage", tenantauth.ErrUnavailable, http.StatusServiceUnavailable},
		{"audit outage", errors.Join(tenantauth.ErrUnavailable, &apikey.Refusal{Code: "access_denied"}), http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/mcp", nil), recorder)
			if err := keyError(c, test.err); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != test.status {
				t.Fatalf("status=%d want=%d", recorder.Code, test.status)
			}
		})
	}
	f := newEntryFixture(t)
	org, err := f.service.readyOrganization(t.Context(), "org_alpha")
	if err != nil {
		t.Fatal(err)
	}
	key := accessKey{ID: "key_example", Hash: "hash_example", Name: "Example", Kind: "service", Owner: "owner", Permission: apikey.ScopeRead, AccessContext: "selected", ServiceOrganization: org.ID, Organizations: []keyOrganization{{OrganizationID: org.ID, ProjectContext: apikey.ProjectContext{Access: "all"}}}}
	if err := f.service.auth.postgresCreateKey(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	f.service.config.transport = func(Organization) (http.RoundTripper, error) {
		return handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })}, nil
	}
	_, err = f.service.keyTenantCall(t.Context(), key, org, "/internal/v1/keys/catalog", struct{}{})
	var refusal *apikey.Refusal
	if !errors.Is(err, tenantauth.ErrUnavailable) || errors.As(err, &refusal) {
		t.Fatalf("tenant outage became a denial: %v", err)
	}
}

func TestTenantKeyAuthorityDenialsAreAudited(t *testing.T) {
	f := newEntryFixture(t)
	for _, orgID := range []string{"org_alpha", "org_beta"} {
		if _, err := f.service.auth.store.db.ExecContext(t.Context(), `UPDATE platform.organizations SET managed=true WHERE id=$1`, orgID); err != nil {
			t.Fatal(err)
		}
		org, err := f.service.readyOrganization(t.Context(), orgID)
		if err != nil {
			t.Fatal(err)
		}
		for index, test := range []struct{ name, event, query string }{
			{"proof changed", "access_denied", ""},
			{"issuer changed", "access_denied", ""},
			{"subject changed", "access_denied", ""},
			{"missing subject", "access_denied", ""},
			{"long subject", "access_denied", ""},
			{"generation changed", "access_denied", ""},
			{"membership removed", "membership_denied", ""},
			{"corrupt context", "key_context_invalid", `UPDATE platform.access_keys SET name='' WHERE id=$1`},
			{"revoked", "key_inactive", `UPDATE platform.access_keys SET revoked_at=now() WHERE id=$1`},
			{"scope narrowed", "key_organization_denied", `UPDATE platform.access_key_organizations SET selected=false WHERE key_id=$1`},
		} {
			t.Run(orgID+"/"+test.name, func(t *testing.T) {
				key := accessKey{ID: fmt.Sprintf("key_%s_%d", orgID, index), Hash: fmt.Sprintf("hash_%s_%d", orgID, index), Name: "Example", Kind: "service", Owner: "owner", Permission: apikey.ScopeRead, AccessContext: "selected", ServiceOrganization: orgID, Organizations: []keyOrganization{{OrganizationID: orgID, ProjectContext: apikey.ProjectContext{Access: "all"}}}}
				if test.name == "membership removed" {
					key.Kind, key.Owner, key.ServiceOrganization = "personal", "removed_owner", ""
				}
				if err := f.service.auth.postgresCreateKey(t.Context(), key); err != nil {
					t.Fatal(err)
				}
				origin := entryauthority.Origin{KeyID: key.ID, CredentialBinding: key.credentialBinding(), Subject: "service:" + key.ID, Issuer: f.service.config.Issuer, Generation: org.Generation}
				switch test.name {
				case "proof changed":
					origin.CredentialBinding = strings.Repeat("a", 64)
				case "issuer changed":
					origin.Issuer = "other_entry"
				case "subject changed":
					origin.Subject = "other_subject"
				case "missing subject":
					origin.Subject = ""
				case "long subject":
					origin.Subject = strings.Repeat("x", 257)
				case "generation changed":
					origin.Generation++
				case "membership removed":
					origin.Subject = key.Owner
				default:
					if _, err := f.service.auth.store.db.ExecContext(t.Context(), test.query, key.ID); err != nil {
						t.Fatal(err)
					}
				}
				_, err := f.service.resolveTenantKey(t.Context(), org, origin)
				if !errors.Is(err, cloudassert.ErrInvalid) || errors.Is(err, tenantauth.ErrUnavailable) {
					t.Fatalf("refused re-resolution must remain a denial: %v", err)
				}
				var count int
				if err := f.fixtures[orgID].database.QueryRowContext(t.Context(), `SELECT count(*) FROM public.hosted_audit WHERE organization_id=$1 AND key_id=$2 AND event=$3`, orgID, key.ID, test.event).Scan(&count); err != nil || count != 1 {
					t.Fatalf("refusal audit count=%d err=%v", count, err)
				}
				if test.event == "key_context_invalid" {
					_, err := f.service.activeAccessKey(t.Context(), key.Hash)
					recorder := httptest.NewRecorder()
					if err := keyError(f.service.echo.NewContext(httptest.NewRequest(http.MethodGet, "/mcp", nil), recorder), err); err != nil {
						t.Fatal(err)
					}
					if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "key_context_invalid") {
						t.Fatalf("corrupt key is retried: status=%d body=%s", recorder.Code, recorder.Body.String())
					}
				}
			})
		}
	}
}

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
		if _, err := registry.store.db.ExecContext(t.Context(), "UPDATE platform.organizations SET managed=true WHERE id=$1", org.ID); err != nil {
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
