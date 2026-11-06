package postgres_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOrganizationSchedulingAdoptionPreservesSourceRevision(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	ctx := t.Context()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			for i, id := range []tracker.ProjectID{"first", "second"} {
				if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: id, Name: string(id), Profile: "native", SchedulingRank: int64(i + 7)}, States: []tracker.NativeState{{Name: "Todo"}}}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	request := projectstore.OrganizationSchedulingAdoption{OrganizationID: "org_first", ManifestSHA256: [32]byte{1}, SourceSHA256: [32]byte{2}, ExpectedRevision: 1, SourceRevision: 38, Projects: []projectstore.OrganizationSchedulingRank{{ProjectID: "first", Rank: 7}, {ProjectID: "second", Rank: 8}}}
	adopt := func(org tracker.OrganizationID, row projectstore.OrganizationSchedulingAdoption) error {
		return repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			importer, ok := w.(projectstore.OrganizationSchedulingImporter)
			if !ok {
				return errors.New("scheduling revision adoption owner missing")
			}
			return importer.AdoptOrganizationSchedulingRevision(ctx, row)
		})
	}
	check := func(org tracker.OrganizationID, revision int64, first tracker.ProjectID, rank int64) {
		t.Helper()
		if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
			reader := r.(projectstore.OrganizationReader)
			got, err := reader.OrganizationSchedulingRevision(ctx)
			if err != nil {
				return err
			}
			if got != revision {
				t.Errorf("revision=%d want %d", got, revision)
			}
			rows, err := reader.OrganizationRankProjects(ctx, 201)
			if err != nil {
				return err
			}
			if len(rows) != 2 || rows[0].ID != first || rows[0].SchedulingRank != rank {
				t.Errorf("unexpected project ranks: %+v", rows)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := adopt("org_first", request); err != nil {
		t.Fatal(err)
	}
	check("org_first", 38, "first", 7)
	check("org_second", 1, "first", 7)
	if err := adopt("org_first", request); err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	second := request
	second.OrganizationID = "org_second"
	second.SourceRevision = 2
	if err := adopt("org_second", second); err != nil {
		t.Fatal(err)
	}
	check("org_second", 2, "first", 7)
	for _, tc := range []struct {
		name   string
		change func(*projectstore.OrganizationSchedulingAdoption)
		want   error
	}{
		{"other tenant", func(r *projectstore.OrganizationSchedulingAdoption) { r.OrganizationID = "org_second" }, corestore.ErrInvalidPage},
		{"missing manifest proof", func(r *projectstore.OrganizationSchedulingAdoption) { r.ManifestSHA256 = [32]byte{} }, corestore.ErrInvalidPage},
		{"missing source proof", func(r *projectstore.OrganizationSchedulingAdoption) { r.SourceSHA256 = [32]byte{} }, corestore.ErrInvalidPage},
		{"noninitial expected revision", func(r *projectstore.OrganizationSchedulingAdoption) { r.ExpectedRevision = 38 }, corestore.ErrInvalidPage},
		{"invalid source revision", func(r *projectstore.OrganizationSchedulingAdoption) { r.SourceRevision = 0 }, corestore.ErrInvalidPage},
		{"duplicate project", func(r *projectstore.OrganizationSchedulingAdoption) {
			r.Projects = []projectstore.OrganizationSchedulingRank{r.Projects[0], r.Projects[0]}
		}, corestore.ErrInvalidPage},
		{"oversized snapshot", func(r *projectstore.OrganizationSchedulingAdoption) {
			r.Projects = make([]projectstore.OrganizationSchedulingRank, 201)
		}, corestore.ErrInvalidPage},
		{"changed rank", func(r *projectstore.OrganizationSchedulingAdoption) {
			r.Projects = append([]projectstore.OrganizationSchedulingRank(nil), r.Projects...)
			r.Projects[0].Rank = 6
		}, corestore.ErrRevisionConflict},
		{"missing project", func(r *projectstore.OrganizationSchedulingAdoption) { r.Projects = r.Projects[:1] }, corestore.ErrRevisionConflict},
		{"older source", func(r *projectstore.OrganizationSchedulingAdoption) { r.SourceRevision = 37 }, corestore.ErrRevisionConflict},
		{"newer source over current", func(r *projectstore.OrganizationSchedulingAdoption) { r.SourceRevision = 39 }, corestore.ErrRevisionConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := request
			tc.change(&row)
			if err := adopt("org_first", row); !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want %v", err, tc.want)
			}
		})
	}
	check("org_first", 38, "first", 7)
	if _, err := admin.ExecContext(ctx, `UPDATE organizations SET scheduling_revision=1 WHERE organization_id=$1`, "org_second"); err != nil {
		t.Fatal(err)
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	rankDone := make(chan error, 1)
	go func() {
		rankDone <- repository.WriteTenant(ctx, "org_second", corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.(projectstore.OrganizationWriter).SetOrganizationProjectRanks(ctx, 1, []tracker.ProjectID{"second", "first"}); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-rankDone:
		t.Fatalf("rank writer before lock: %v", err)
	}
	adoptionStarted := make(chan struct{})
	adoptionDone := make(chan error, 1)
	go func() {
		close(adoptionStarted)
		second.SourceRevision = 10
		adoptionDone <- adopt("org_second", second)
	}()
	<-adoptionStarted
	close(release)
	rankErr := <-rankDone
	adoptionErr := <-adoptionDone
	if rankErr != nil {
		t.Fatal(rankErr)
	}
	if !errors.Is(adoptionErr, corestore.ErrRevisionConflict) {
		t.Fatalf("adoption overwrote rank writer: %v", adoptionErr)
	}
	check("org_second", 2, "second", 0)
	if err := adopt("org_second", second); !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("newer current overwrite: %v", err)
	}
	check("org_first", 38, "first", 7)
}

func TestOrganizationSchedulingRollbackRetainsAdoptedStateAcrossTenantRLS(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	ctx := t.Context()
	tenants := []struct {
		org      tracker.OrganizationID
		revision int64
		ranks    []projectstore.OrganizationSchedulingRank
	}{
		{"org_first", 38, []projectstore.OrganizationSchedulingRank{{ProjectID: "first", Rank: 7}, {ProjectID: "second", Rank: 8}}},
		{"org_second", 10, []projectstore.OrganizationSchedulingRank{{ProjectID: "first", Rank: 12}, {ProjectID: "second", Rank: 19}}},
	}
	for _, tenant := range tenants {
		seedTenantAccess(t, repository, tenant.org)
		if err := repository.WriteTenant(ctx, tenant.org, corestore.Serving, func(w tenantstore.Writer) error {
			for _, project := range tenant.ranks {
				if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: tenant.org, ID: project.ProjectID, Name: string(project.ProjectID), Profile: "native", SchedulingRank: project.Rank}, States: []tracker.NativeState{{Name: "Todo"}}}); err != nil {
					return err
				}
			}
			return w.(projectstore.OrganizationSchedulingImporter).AdoptOrganizationSchedulingRevision(ctx, projectstore.OrganizationSchedulingAdoption{OrganizationID: tenant.org, ManifestSHA256: [32]byte{1}, SourceSHA256: [32]byte{2}, ExpectedRevision: 1, SourceRevision: tenant.revision, Projects: tenant.ranks})
		}); err != nil {
			t.Fatal(err)
		}
	}
	admin.SetMaxOpenConns(1)
	admin.SetMaxIdleConns(1)
	if _, err := admin.ExecContext(ctx, `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(ctx, `RESET ROLE`); err != nil {
			t.Error(err)
		}
	}()
	var hidden int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM organizations`).Scan(&hidden); err != nil || hidden != 0 {
		t.Fatalf("unscoped migration role did not exercise tenant RLS: count=%d error=%v", hidden, err)
	}
	provider, err := migrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ApplyVersion(ctx, 20261104012900, false); err == nil || !strings.Contains(err.Error(), "organization scheduling revisions require an explicit retained-data migration") {
		t.Errorf("rollback returned %v, want scheduling revision retained-data refusal", err)
	}
	if got, err := provider.GetDBVersion(ctx); err != nil || got != version {
		t.Errorf("schema version after rollback=%d want %d error=%v", got, version, err)
	}
	for _, tenant := range tenants {
		if err := repository.ReadTenant(ctx, tenant.org, corestore.Serving, func(r tenantstore.Reader) error {
			reader := r.(projectstore.OrganizationReader)
			revision, err := reader.OrganizationSchedulingRevision(ctx)
			if err != nil {
				return err
			}
			if revision != tenant.revision {
				t.Errorf("tenant %s revision=%d want %d", tenant.org, revision, tenant.revision)
			}
			rows, err := reader.OrganizationRankProjects(ctx, 201)
			if err != nil {
				return err
			}
			if len(rows) != len(tenant.ranks) {
				t.Errorf("tenant %s projects=%d want %d", tenant.org, len(rows), len(tenant.ranks))
				return nil
			}
			for i, want := range tenant.ranks {
				if rows[i].ID != want.ProjectID || rows[i].SchedulingRank != want.Rank {
					t.Errorf("tenant %s project %d=%s rank %d want %s rank %d", tenant.org, i, rows[i].ID, rows[i].SchedulingRank, want.ProjectID, want.Rank)
				}
			}
			return nil
		}); err != nil {
			t.Errorf("tenant %s retained state: %v", tenant.org, err)
		}
	}
}
