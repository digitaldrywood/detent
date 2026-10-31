package platformstore

import (
	"errors"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRegistryMappingAndGenerations(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	for _, id := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(id), func(t *testing.T) {
			original, err := r.Organization(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			mapped, err := r.ByProvider(t.Context(), original.ProviderID)
			if err != nil || mapped.ID != id {
				t.Fatalf("mapping: %+v %v", mapped, err)
			}
			cases := []struct {
				name    string
				change  func(*Organization)
				changed bool
				want    error
			}{
				{"idempotent", func(*Organization) {}, false, nil},
				{"endpoint requires generation", func(o *Organization) { o.Endpoint = "unix:/example/replaced.sock" }, false, ErrOrganizationConflict},
				{"provider cannot be reassigned", func(o *Organization) { o.ProviderID = "provider_other" }, false, ErrOrganizationConflict},
				{"generation advances", func(o *Organization) { o.Generation++; o.Endpoint = "unix:/example/new.sock" }, true, nil},
				{"old generation refused", func(*Organization) {}, false, ErrOrganizationConflict},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					o := original
					tc.change(&o)
					changed, err := r.Register(t.Context(), o)
					if changed != tc.changed || !errors.Is(err, tc.want) {
						t.Fatalf("changed=%v err=%v", changed, err)
					}
				})
			}
		})
	}
	page, err := r.Organizations(t.Context(), "", 1)
	if err != nil || len(page) != 1 || page[0].ID != "org_first" {
		t.Fatalf("first page: %+v %v", page, err)
	}
	page, err = r.Organizations(t.Context(), page[0].ID, 1)
	if err != nil || len(page) != 1 || page[0].ID != "org_second" {
		t.Fatalf("second page: %+v %v", page, err)
	}
	if _, err := r.ByProvider(t.Context(), "provider_missing"); !errors.Is(err, corestore.ErrNotFound) {
		t.Fatal(err)
	}
	var events int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM platform.organization_events`).Scan(&events); err != nil || events != 4 {
		t.Fatalf("events=%d err=%v", events, err)
	}
}

func TestConcurrentRegistryRegistrationIsIdempotent(t *testing.T) {
	db := template.Database(t)
	cfg := testConfig(t)
	first := repository(t, db, cfg)
	second := replicaRepository(t, first, cfg)
	o := Organization{ID: "org_example", ProviderID: "provider_example", Name: "Example", Endpoint: "unix:/example/tenant.sock", Generation: 1}
	type outcome struct {
		changed bool
		err     error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for _, r := range []*Repository{first, second} {
		workers.Go(func() { <-start; changed, err := r.Register(t.Context(), o); results <- outcome{changed, err} })
	}
	close(start)
	workers.Wait()
	close(results)
	changed := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.changed {
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("changed registrations=%d", changed)
	}
	var events int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM platform.organization_events WHERE organization_id=$1`, o.ID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("events=%d err=%v", events, err)
	}
}
