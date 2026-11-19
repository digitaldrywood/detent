package platformstore

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPlatformKeyReadsPreserveScopeAndFailClosed(t *testing.T) {
	db := template.Database(t)
	initial := rolePool(t, db, postgres.PlatformRoleName, "")
	config := initial.Config()
	initial.Close()
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	r, err := New(t.Context(), pool, testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	seedOrganizations(t, r)
	if _, err := db.ExecContext(t.Context(), `UPDATE platform.organizations SET managed=true`); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, size := range []int{2, 100} {
			id := fmt.Sprintf("key_%s_%d", org, size)
			projects := make([]tracker.ProjectID, size)
			for i := range projects {
				projects[i] = tracker.ProjectID(fmt.Sprintf("prj_%03d", i))
			}
			key := Key{ID: id, Hash: "hash_" + id, Name: "Example", Kind: "service", OwnerSubject: "owner", Permission: "read", AccessContext: "selected", ServiceOrganizationID: &org, Organizations: []KeyOrganization{{OrganizationID: org, ProjectAccess: "selected", Projects: projects}}}
			if err := r.CreateKey(t.Context(), key); err != nil {
				t.Fatal(err)
			}
			for _, read := range []struct {
				name string
				run  func() error
			}{
				{"credential", func() error {
					stored, err := r.Key(t.Context(), key.Hash)
					if err != nil {
						return err
					}
					if len(stored.Organizations) != 1 || len(stored.Organizations[0].Projects) != size {
						return errors.New("stored scope was truncated")
					}
					return nil
				}},
				{"authority", func() error {
					_, err := r.KeyAuthority(t.Context(), key.Hash, org, projects[size-1])
					return err
				}},
				{"re-resolution", func() error {
					snapshot, err := r.TenantKey(t.Context(), key.ID, org)
					if err != nil {
						return err
					}
					if !snapshot.Authority.Allows(string(projects[size-1])) {
						return errors.New("authority scope was truncated")
					}
					return nil
				}},
			} {
				t.Run(fmt.Sprintf("%s/%d/%s", org, size, read.name), func(t *testing.T) {
					if err := read.run(); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE platform.access_key_projects RENAME TO unavailable_key_projects`); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func() error{
		func() error { _, err := r.Key(t.Context(), "hash_key_org_first_2"); return err },
		func() error { _, err := r.TenantKey(t.Context(), "key_org_first_2", "org_first"); return err },
	} {
		err := read()
		if !errors.Is(err, tenantauth.ErrUnavailable) || errors.Is(err, tenantauth.ErrDenied) || errors.Is(err, ErrInvalid) {
			t.Errorf("query error became a denial: %v", err)
		}
	}
}
