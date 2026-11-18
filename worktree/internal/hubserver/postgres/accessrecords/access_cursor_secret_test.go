package accessrecords_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOrganizationSecretCreateIfAbsent(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	keys := pgstoretest.AccessTestKeys(t, true)
	rollback := errors.New("rollback cursor key creation")
	for i, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			organization := accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: pgstoretest.HistoryAt()}
			key := bytes.Repeat([]byte{byte(i + 1)}, 32)
			envelope, err := keys.Seal(key, accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
			if err != nil {
				t.Fatal(err)
			}
			secret := accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: pgstoretest.HistoryAt()}
			audit := accessstore.SecretAudit{OrganizationID: org, Kind: secret.Kind, Actor: "example", Event: "create", KeyVersion: envelope.Version, RecordedAt: pgstoretest.HistoryAt()}
			err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), organization); err != nil {
					return err
				}
				if err := w.EnsureOrganizationSecret(t.Context(), secret, audit); err != nil {
					return err
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatalf("rollback error=%v", err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.AccessOrganization(t.Context())
				return err
			}); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("organization survived rollback: %v", err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), organization); err != nil {
					return err
				}
				return w.EnsureOrganizationSecret(t.Context(), secret, audit)
			}); err != nil {
				t.Fatal(err)
			}
			replacement := secret
			replacement.Envelope, err = keys.Seal(bytes.Repeat([]byte{0xff}, 32), accessstore.SecretAAD(org, "", secret.Kind))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.EnsureOrganizationSecret(t.Context(), replacement, audit)
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				stored, err := r.AccessSecret(t.Context(), "", secret.Kind)
				if err != nil {
					return err
				}
				value, err := accessstore.OpenSecret(keys, stored)
				if err == nil && !bytes.Equal(value, key) {
					t.Fatal("create-if-absent replaced the cursor key")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM organization_secret_audit WHERE organization_id=$1 AND kind=$2`, org, secret.Kind).Scan(&count); err != nil || count != 1 {
				t.Fatalf("creation audit count=%d, want 1: %v", count, err)
			}
			if err := repository.ReadRequest(t.Context(), org, func(ctx context.Context) error {
				foreign := secret
				foreign.OrganizationID = "org_other"
				_, err := repository.EnsureOrganizationSecret(ctx, foreign, audit)
				return err
			}); !errors.Is(err, corestore.ErrTenantMismatch) {
				t.Fatalf("foreign cursor key initialization from a read snapshot: %v", err)
			}
			for _, test := range []struct {
				name string
				row  accessstore.Secret
				want error
			}{
				{"foreign organization", accessstore.Secret{OrganizationID: "org_other"}, corestore.ErrTenantMismatch},
				{"project secret", accessstore.Secret{OrganizationID: org, ProjectID: "prj_first"}, accessstore.ErrDenied},
				{"audit mismatch", accessstore.Secret{OrganizationID: org, Kind: "other"}, accessstore.ErrDenied},
			} {
				t.Run(test.name, func(t *testing.T) {
					err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						return w.EnsureOrganizationSecret(t.Context(), test.row, audit)
					})
					if !errors.Is(err, test.want) {
						t.Fatalf("secret creation error=%v, want %v", err, test.want)
					}
				})
			}
		})
	}
}
