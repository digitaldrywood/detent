package hubserver

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresOrganizationCreationWithoutLegacyDatabase(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := &Service{config: Config{AccessRepository: repository, TenantRepository: repository, SecretKeys: secretTestKeys(t, "1", "1"), now: time.Now}}
	var previous []byte
	for _, name := range []string{"First example", "Second example"} {
		organization, err := service.createNativeOrganizationFor(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		if organization.ID == "" || organization.Slug == "" {
			t.Fatalf("incomplete organization: %+v", organization)
		}
		err = repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
			stored, err := r.AccessOrganization(t.Context())
			if err == nil && (stored.Name != name || stored.Slug != organization.Slug) {
				t.Fatalf("organization=%+v", stored)
			}
			if err != nil {
				return err
			}
			secret, err := r.AccessSecret(t.Context(), "", accessstore.NativeCursorKeyKind)
			if err != nil {
				return err
			}
			key, err := accessstore.OpenSecret(service.config.SecretKeys, secret)
			if err != nil {
				return err
			}
			if len(key) != 32 || bytes.Equal(previous, key) {
				t.Fatal("organization cursor key is invalid or reused")
			}
			previous = key
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgresLocalOrganizationCursorKey(t *testing.T) {
	repository := projectRecordPostgres(t)
	cfg := Config{SecretKeys: secretTestKeys(t, "1", "1"), now: time.Now}
	org, err := bootstrapLocalOrganization(t.Context(), repository, cfg, "example-bootstrap-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		secret, err := r.AccessSecret(t.Context(), "", accessstore.NativeCursorKeyKind)
		if err != nil {
			return err
		}
		key, err := accessstore.OpenSecret(cfg.SecretKeys, secret)
		if err == nil && len(key) != 32 {
			t.Fatalf("bootstrap cursor key length=%d", len(key))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresCredentialMembershipAndKeyAudit(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now}); err != nil {
				return err
			}
			for _, id := range []string{"instance", "member"} {
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: id, Name: id, Hash: string(org) + id, Scope: "admin", Kind: "service", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
			}
			return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: "example", Email: "example@example.test", MembershipID: "membership", Role: "admin", Active: false, PrincipalID: "member", CreatedAt: now, UpdatedAt: now})
		})
		if err != nil {
			t.Fatal(err)
		}
		err = repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			for _, tc := range []struct {
				id     string
				member bool
			}{{"instance", false}, {"member", true}} {
				got, err := r.AccessCredentialHasMember(t.Context(), tc.id)
				if err != nil {
					return err
				}
				if got != tc.member {
					t.Fatalf("%s/%s member=%v", org, tc.id, got)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		service := &Service{config: Config{AccessRepository: repository, TenantRepository: repository, Hosted: &hostedplans.HostedConfig{OrganizationID: string(org)}, now: func() time.Time { return now }}}
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/keys/audit", strings.NewReader(`{"key_id":"key_example","owner":"owner_example","event":"key_revoked"}`))
		request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		recorder := httptest.NewRecorder()
		c := echo.New().NewContext(request, recorder)
		c.Set(hostedSharedClaimsKey, cloudassert.Claims{Kind: cloudassert.KindService, Audience: string(org), Path: request.URL.Path, IssuedAt: now, ExpiresAt: now.Add(time.Hour)})
		if err := service.entryKeyAudit(c); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
		}
		var owner, event, key string
		err = admin.QueryRowContext(t.Context(), `SELECT key_owner,event,key_id FROM hosted_audit WHERE organization_id=$1`, org).Scan(&owner, &event, &key)
		if err != nil {
			t.Fatal(err)
		}
		if owner != "owner_example" || event != "key_revoked" || key != "key_example" {
			t.Fatalf("audit=%q %q %q", owner, event, key)
		}
	}
}
