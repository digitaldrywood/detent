package accessmembers

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

func TestCredentialMaintenanceAccess(t *testing.T) {
	t.Parallel()
	t.Run("member preparation", func(t *testing.T) {
		for _, scenario := range []string{"concurrent principal", "existing principal", "revoked principal", "missing preparation", "wrong preparation"} {
			t.Run(scenario, func(t *testing.T) {
				now := time.Now().UTC().Truncate(time.Microsecond)
				admin := hubtest.Database(t)
				repository := hubtest.RepositoryOn(t, admin)
				organization := hubtest.Organization(t, repository, hubtest.OrganizationOptions{Now: now}).OrganizationID
				identity := auth.Identity{Subject: "user_prepared_invitee", Email: "prepared-invitee@example.test", EmailVerified: true, Hosted: &auth.HostedIdentity{Subject: "user_prepared_invitee", OrganizationID: "provider_" + string(organization)}}
				originalGenerator := apikey.GenerateToken
				generated := 0
				generationFailure := errors.New("fixture random unavailable")
				s := New(Deps{Repository: repository, Organization: organization, Now: func() time.Time { return now }, GenerateToken: func() (string, error) { generated++; return "", generationFailure }})
				principal := "hosted_" + apikey.HashToken(identity.Subject)[:32]
				membership := auth.Membership{ID: "membership_prepared_invitee", UserID: identity.Subject, OrganizationID: identity.Hosted.OrganizationID, Status: "active"}
				membership.Role.Slug = "member"
				var err error
				prepared := Preparation{}
				if scenario == "concurrent principal" {
					s.deps.GenerateToken = originalGenerator
					prepared, err = s.Prepare(t.Context(), identity)
					if err != nil {
						t.Fatal(err)
					}
				}
				var stored accessstore.Credential
				if scenario == "existing principal" || scenario == "revoked principal" || scenario == "concurrent principal" {
					stored = seedCredential(t, s, principal, "existing_principal_secret")
					if scenario == "revoked principal" {
						if err := repository.WriteTenant(t.Context(), s.deps.Organization, corestore.Serving, func(w tenantstore.Writer) error {
							return w.RevokeAccessCredential(t.Context(), principal, s.deps.Now())
						}); err != nil {
							t.Fatal(err)
						}
						stored = readCredential(t, s, principal)
					}
					if scenario != "concurrent principal" {
						prepared, err = s.Prepare(t.Context(), identity)
						if err != nil || generated != 0 {
							t.Fatalf("existing preparation error=%v generated=%d", err, generated)
						}
					}
				}
				if scenario == "wrong preparation" {
					prepared = Preparation{organization: "org_foreign", subject: identity.Subject, principal: principal, hash: "untrusted", fingerprint: "untrusted"}
				}
				err = repository.WriteTenant(t.Context(), s.deps.Organization, corestore.Serving, func(w tenantstore.Writer) error {
					return s.Store(t.Context(), w, identity, membership, prepared)
				})
				if scenario == "missing preparation" || scenario == "wrong preparation" {
					if !errors.Is(err, auth.ErrHostedIdentity) {
						t.Fatalf("invalid construction accepted: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				current := readCredential(t, s, principal)
				if !reflect.DeepEqual(stored, current) {
					t.Fatal("member publication replaced or revived existing principal")
				}
				var count int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_members WHERE organization_id=$1 AND user_id=$2 AND principal_id=$3", s.deps.Organization, identity.Subject, principal).Scan(&count); err != nil || count != 1 {
					t.Fatalf("canonical publication count=%d error=%v", count, err)
				}
			})
		}
	})
}

func seedCredential(t *testing.T, s *Service, id, secret string) accessstore.Credential {
	t.Helper()
	now := s.deps.Now().UTC().Truncate(time.Microsecond)
	row := accessstore.Credential{OrganizationID: s.deps.Organization, ID: id, Name: id, Hash: apikey.HashToken(secret), Fingerprint: fingerprint(apikey.HashToken(secret)), Scope: "operator", Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}
	if err := s.deps.Repository.WriteTenant(t.Context(), row.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error { return w.CreateAccessCredential(t.Context(), row) }); err != nil {
		t.Fatal(err)
	}
	return readCredential(t, s, id)
}

func readCredential(t *testing.T, s *Service, id string) accessstore.Credential {
	t.Helper()
	var row accessstore.Credential
	if err := s.read(t.Context(), func(r tenantstore.Reader) error {
		var err error
		row, err = r.AccessCredential(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return row
}
