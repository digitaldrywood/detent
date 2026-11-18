package platformstore

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"

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
	t.Run("readable organization slugs", func(t *testing.T) {
		db := template.Database(t)
		r := repository(t, db, testConfig(t))
		create := func(id, name, want string) {
			t.Helper()
			input := Organization{ID: tracker.OrganizationID(id), ProviderID: "provider_" + id, Name: name, Endpoint: "unix:/example/" + id + ".sock", Generation: 1}
			if changed, err := r.Register(t.Context(), input); err != nil || !changed {
				t.Fatalf("register %s changed=%v error=%v", id, changed, err)
			}
			stored, err := r.Organization(t.Context(), input.ID)
			if err != nil || stored.Slug != want {
				t.Fatalf("organization %s slug=%q want=%q error=%v", id, stored.Slug, want, err)
			}
		}
		create("org_initial_first", "Example Organization", "example-organization")
		create("org_initial_second", "Example Organization", "example-organization-2")
		for _, route := range []string{"work", "settings", "login", "projects", "organizations", "organization", "api", "app", "auth", "chat", "reports", "activity", "diagnostics", "fleet", "support", "invite", "logout", "health", "metrics", "static", "webhooks", "design-system", "setup", "usage", "billing", "account", "platform"} {
			t.Run(route, func(t *testing.T) {
				create("org_reserved_"+route, route, route+"-organization")
				create("org_repeated_"+route, route, route+"-organization-2")
			})
		}
		create("org_final_example", "Example Organization", "example-organization-3")
		create("org_empty_name", "!!!", "organization-organization-3")
	})

}

func TestConcurrentRegistryRegistrationIsIdempotent(t *testing.T) {
	cases := []struct {
		name     string
		change   func(*Organization)
		conflict bool
	}{
		{"identical", func(*Organization) {}, false},
		{"different organization", func(o *Organization) { o.ID = "org_other" }, true},
		{"different provider", func(o *Organization) { o.ProviderID = "provider_other" }, true},
		{"different endpoint", func(o *Organization) { o.Endpoint = "unix:/example/other.sock" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := template.Database(t)
			cfg := testConfig(t)
			first := repository(t, db, cfg)
			second := repository(t, db, cfg)
			o := Organization{ID: "org_example", ProviderID: "provider_example", Name: "Example", Endpoint: "unix:/example/tenant.sock", Generation: 1}
			type outcome struct {
				organization Organization
				changed      bool
				err          error
			}
			replicas := []*Repository{first, second}
			results := make(chan outcome, len(replicas))
			start := make(chan struct{})
			var workers sync.WaitGroup
			other := o
			tc.change(&other)
			for i, r := range replicas {
				input := o
				if i == 1 {
					input = other
				}
				workers.Go(func() {
					<-start
					changed, err := r.Register(t.Context(), input)
					results <- outcome{input, changed, err}
				})
			}
			close(start)
			workers.Wait()
			close(results)
			changed := 0
			conflicts := 0
			var winner Organization
			for result := range results {
				if errors.Is(result.err, ErrOrganizationConflict) && tc.conflict && !result.changed {
					conflicts++
				} else if result.err != nil {
					t.Fatal(result.err)
				}
				if result.changed {
					changed++
					winner = result.organization
				}
			}
			if changed != 1 {
				t.Fatalf("changed registrations=%d", changed)
			}
			if (conflicts == 1) != tc.conflict {
				t.Fatalf("conflicts=%d want conflict=%v", conflicts, tc.conflict)
			}
			stored, err := first.ByProvider(t.Context(), winner.ProviderID)
			if err != nil || stored.ID != winner.ID || stored.Endpoint != winner.Endpoint || stored.Generation != winner.Generation {
				t.Fatalf("stored registration: %+v err=%v", stored, err)
			}
			var events int
			if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM platform.organization_events`).Scan(&events); err != nil || events != 1 {
				t.Fatalf("events=%d err=%v", events, err)
			}
		})
	}
}

func TestOrganizationReadsUseOneStatement(t *testing.T) {
	db := template.Database(t)
	var calls atomic.Int64
	pool := rolePool(t, db, postgres.PlatformRoleName, "", countedTracer{calls: &calls})
	native := pool.Config()
	pool.Close()
	native.MaxConns = 3
	pool, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	r, err := New(t.Context(), pool, testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	seedOrganizations(t, r)
	for _, id := range []tracker.OrganizationID{"org_first", "org_second"} {
		before := calls.Load()
		organization, err := r.Organization(t.Context(), id)
		if err != nil || organization.ID != id {
			t.Fatalf("organization=%s err=%v", organization.ID, err)
		}
		if count := calls.Load() - before; count != 1 {
			t.Fatalf("organization read used %d statements", count)
		}
		before = calls.Load()
		organization, err = r.ByProvider(t.Context(), organization.ProviderID)
		if err != nil || organization.ID != id {
			t.Fatalf("provider organization=%s err=%v", organization.ID, err)
		}
		if count := calls.Load() - before; count != 1 {
			t.Fatalf("provider read used %d statements", count)
		}
	}
}

func TestDueAllocations(t *testing.T) {
	db := template.Database(t)
	var calls atomic.Int64
	pool := rolePool(t, db, postgres.PlatformRoleName, "", countedTracer{calls: &calls})
	native := pool.Config()
	pool.Close()
	native.MaxConns = 3
	pool, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	r, err := New(t.Context(), pool, testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	seedOrganizations(t, r)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := db.ExecContext(t.Context(), "UPDATE platform.organizations SET state='allocating',managed=true,next_attempt_at=$1,created_at=$2", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	check := func(at time.Time, after AllocationCandidate, want ...string) []AllocationCandidate {
		t.Helper()
		before := calls.Load()
		rows, _, err := r.AllocationSchedule(t.Context(), after, at)
		if err != nil {
			t.Fatal(err)
		}
		if count := calls.Load() - before; count != 1 {
			t.Fatalf("allocator used %d statements", count)
		}
		var ids []string
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("selected %v, want %v", ids, want)
		}
		return rows
	}
	rows, deadline, err := r.AllocationSchedule(t.Context(), AllocationCandidate{}, now)
	if err != nil || len(rows) != 0 || !deadline.Equal(now.Add(time.Minute)) {
		t.Fatalf("future retry rows=%d deadline=%v err=%v", len(rows), deadline, err)
	}
	check(now, AllocationCandidate{})
	if _, err := db.ExecContext(t.Context(), "UPDATE platform.organizations SET state='deleting' WHERE id='org_second'"); err != nil {
		t.Fatal(err)
	}
	due := check(now, AllocationCandidate{}, "org_second")
	if !due[0].Deleting {
		t.Fatal("deletion returned as provisioning")
	}
	due = check(now.Add(time.Minute), AllocationCandidate{}, "org_second", "org_first")
	if due[1].Deleting {
		t.Fatal("provisioning returned as deletion")
	}
	check(now.Add(time.Minute), due[0], "org_first")
	if _, err := db.ExecContext(t.Context(), `INSERT INTO platform.organizations(id,name,state,managed,endpoint,generation,created_at,updated_at)
SELECT 'org_delete_'||lpad(n::text,2,'0'),'Example','deleting',true,'unix:/example/allocator.sock',1,$1,$1 FROM generate_series(1,$2::integer) n`, now, AllocationPageSize); err != nil {
		t.Fatal(err)
	}
	first, deadline, err := r.AllocationSchedule(t.Context(), AllocationCandidate{}, now)
	if err != nil || len(first) != AllocationPageSize || !deadline.Equal(now.Add(time.Minute)) {
		t.Fatalf("first deletion page=%d retry=%v err=%v", len(first), deadline, err)
	}
	check(now, first[len(first)-1], "org_second")
}
