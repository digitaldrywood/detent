package postgres_test

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOrganizationSlugAliasesRetainTenantScope(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		for _, slug := range []string{"original", string(org)[4:] + "-renamed"} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.SetAccessOrganizationSlug(t.Context(), nil, slug)
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	rollback := errors.New("rollback slug")
	err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.SetAccessOrganizationSlug(t.Context(), new("first-renamed"), "rolled-back"); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	err = repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetAccessOrganizationSlug(t.Context(), new("original"), "stale-write")
	})
	if !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("stale update = %v", err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			for _, slug := range []string{"original", "first-renamed", "second-renamed", "rolled-back", "stale-write"} {
				got, err := r.AccessOrganizationSlug(t.Context(), slug)
				want := slug == "original" || slug == string(org)[4:]+"-renamed"
				if err != nil || got != want {
					t.Fatalf("organization=%s slug=%s found=%v want=%v error=%v", org, slug, got, want, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
