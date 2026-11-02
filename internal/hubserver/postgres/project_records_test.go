package postgres_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProjectRecordsKeepTenantRevisionsAndClaims(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	ctx := t.Context()
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}
	records := make(map[tracker.OrganizationID]projectstore.Record)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "shared", Name: "Example", Profile: "native", RequireDependencies: true}, States: states})
			if err != nil {
				return err
			}
			if record.Core.Slug != "example" || record.Settings.Revision != 1 || !reflect.DeepEqual(record.States, states) {
				t.Fatalf("created record = %+v", record)
			}
			records[org] = record
			another, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "another", Name: "Example!", Profile: "native"}, States: states})
			if err == nil && (!strings.HasPrefix(another.Core.Slug, "example-") || another.Core.Slug == record.Core.Slug) {
				t.Fatalf("collision slug=%s", another.Core.Slug)
			}
			records[tracker.OrganizationID(string(org)+"_another")] = another
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	first := records["org_first"]
	first.Core.Name = "Updated"
	first.Core.Slug = "updated"
	first.Settings.ArchiveCompletedAfterDays = new(int64(12))
	first.States[0].OperatorOnly = true
	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		first, err = w.UpdateProjectRecord(ctx, first)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if first.Settings.Revision != 2 || first.Core.ContentSettingsRevision != 0 || first.Settings.ArchiveCompletedAfterDays == nil || *first.Settings.ArchiveCompletedAfterDays != 12 {
		t.Fatalf("updated record=%+v", first)
	}
	err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.UpdateProjectRecord(ctx, records["org_first"])
		return err
	})
	if !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("stale update=%v", err)
	}
	err = repository.WriteTenant(ctx, "org_second", corestore.Serving, func(w tenantstore.Writer) error { _, err := w.UpdateProjectRecord(ctx, first); return err })
	if !errors.Is(err, corestore.ErrTenantMismatch) {
		t.Fatalf("cross-tenant update=%v", err)
	}
	if err := repository.ReadTenant(ctx, "org_second", corestore.Serving, func(r tenantstore.Reader) error {
		got, err := r.ProjectRecord(ctx, "shared")
		if err == nil && got.Core.Name != "Example" {
			t.Fatalf("other tenant changed: %+v", got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		old, err := r.ProjectBySlug(ctx, "example")
		if err != nil {
			return err
		}
		if old.Core.ID != "shared" || old.Core.Slug != "updated" {
			t.Fatalf("old slug lost=%+v", old)
		}
		another, err := r.ProjectBySlug(ctx, records["org_first_another"].Core.Slug)
		if err == nil && another.Core.ID != "another" {
			t.Fatalf("collision slug=%+v", another)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var retiredItem int64
	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		item, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: "org_first", ProjectID: "shared", NativeID: "retained", Number: 1, Title: "Retained"})
		if err != nil {
			return err
		}
		retiredItem = item.ID
		return w.RegisterMachine(ctx, leasestore.Machine{OrganizationID: "org_first", ID: "machine", Hostname: "example", Capacity: 1, Version: "test"})
	}); err != nil {
		t.Fatal(err)
	}
	var claim leasestore.Lease
	if err := repository.WriteTenant(ctx, "org_second", corestore.Serving, func(w tenantstore.Writer) error {
		item, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: "org_second", ProjectID: "shared", NativeID: "item", Number: 1, Title: "Example"})
		if err != nil {
			return err
		}
		if err := w.RegisterMachine(ctx, leasestore.Machine{OrganizationID: "org_second", ID: "machine", Hostname: "example", Capacity: 1, Version: "test"}); err != nil {
			return err
		}
		claim, err = w.Claim(ctx, leasestore.Claim{IssueID: item.ID, ExpectedRevision: 1, MachineID: "machine", LeaseID: "claim", SessionID: "session", TTL: time.Hour})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err = repository.WriteTenant(ctx, "org_second", corestore.Serving, func(w tenantstore.Writer) error { return w.RetireProject(ctx, "shared", 1, claim.AcquiredAt) })
	if !errors.Is(err, projectstore.ErrActiveWork) {
		t.Fatalf("delete claimed project=%v", err)
	}
	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error { return w.RetireProject(ctx, "shared", 2, claim.AcquiredAt) }); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.ProjectRecord(ctx, "shared")
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("deleted project=%v", err)
		}
		_, err = r.ProjectBySlug(ctx, "example")
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("deleted alias=%v", err)
		}
		owner, err := r.ProjectSlugOwner(ctx, "example")
		if err == nil && owner != "shared" {
			t.Fatalf("slug reservation=%s", owner)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err = repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.Claim(ctx, leasestore.Claim{IssueID: retiredItem, ExpectedRevision: 1, MachineID: "machine", LeaseID: "retired-claim", SessionID: "retired-session", TTL: time.Hour})
		return err
	})
	if !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("claim retired project=%v", err)
	}
	if err := repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		item, err := r.WorkItem(ctx, "shared", "retained")
		if err == nil && item.Title != "Retained" {
			t.Fatalf("retained work item=%+v", item)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(ctx, "org_second", corestore.Serving, func(r tenantstore.Reader) error {
		got, err := r.Lease(ctx, "claim")
		if err == nil && (got.ReleasedAt != nil || got.FencingToken != claim.FencingToken) {
			t.Fatalf("other claim changed=%+v", got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProjectAccessPageFiltersSelectedAndBlockedProjectsBeforeLimit(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	ctx := t.Context()
	seedTenantAccess(t, repository, "org_first")
	ids := []tracker.ProjectID{"project_a", "project_b", "project_c"}
	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		for _, id := range ids {
			if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: "org_first", ID: id, Name: string(id), Profile: "native"}, States: []tracker.NativeState{{Name: "Todo"}}}); err != nil {
				return err
			}
			if err := w.InitializeProjectAccess(ctx, id, "first"); err != nil {
				return err
			}
		}
		now := historyAt()
		if err := w.CreateAccessCredential(ctx, accessstore.Credential{OrganizationID: "org_first", ID: "selected_key", Name: "Selected key", Hash: "selected_hash", Scope: "operator", NativeOnly: true, UserID: "first", MembershipID: "membership_first", KeyScope: "read", ProjectAccess: "selected", Kind: "personal", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		for _, id := range ids[1:] {
			if err := w.SetAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: "org_first", TokenID: "selected_key", ProjectID: id}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	selected := accessstore.ProjectAccessFilter{UserID: "first", PrincipalID: "first", CredentialID: "selected_key", ProjectAccess: "selected", RequireCredentialGrant: true, Limit: 1}
	var selectedFirst, selectedNext []accessstore.ProjectAccess
	if err := repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(accessstore.ProjectAccessPageReader)
		if !ok {
			t.Fatal("tenant reader does not expose typed project access pages")
		}
		var err error
		selectedFirst, err = reader.ProjectAccessPage(ctx, selected)
		if err != nil {
			return err
		}
		selected.After = selectedFirst[0].ProjectID
		selectedNext, err = reader.ProjectAccessPage(ctx, selected)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(selectedFirst) != 2 || selectedFirst[0].ProjectID != "project_b" || len(selectedNext) != 1 || selectedNext[0].ProjectID != "project_c" {
		t.Fatalf("selected pages = %+v then %+v", selectedFirst, selectedNext)
	}
	blocked := accessstore.ProjectAccessFilter{UserID: "first", PrincipalID: "first", KeyKind: "personal", ProjectAccess: "selected", Projects: ids, BlockedProjects: []tracker.ProjectID{"project_b"}, Limit: 1}
	var blockedFirst, blockedNext []accessstore.ProjectAccess
	if err := repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		reader := r.(accessstore.ProjectAccessPageReader)
		var err error
		blockedFirst, err = reader.ProjectAccessPage(ctx, blocked)
		if err != nil {
			return err
		}
		blocked.After = blockedFirst[0].ProjectID
		blockedNext, err = reader.ProjectAccessPage(ctx, blocked)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(blockedFirst) != 2 || blockedFirst[0].ProjectID != "project_a" || len(blockedNext) != 1 || blockedNext[0].ProjectID != "project_c" {
		t.Fatalf("blocked pages = %+v then %+v", blockedFirst, blockedNext)
	}
	only := blocked
	only.After, only.Only, only.Limit = "", "project_b", 1
	if err := repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		rows, err := r.(accessstore.ProjectAccessPageReader).ProjectAccessPage(ctx, only)
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			t.Fatalf("blocked exact project appeared: %+v", rows)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProjectRecordMutationRollsBackRelationships(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	ctx := t.Context()
	var record projectstore.Record
	if err := repository.WriteTenant(ctx, "org", corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		record, err = w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: "org", ID: "project", Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo"}, {Name: "Done", Terminal: true}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("rejected")
	err := repository.WriteTenant(ctx, "org", corestore.Serving, func(w tenantstore.Writer) error {
		record.Core.Name = "Rejected"
		record.Core.Slug = "rejected"
		if _, err := w.UpdateProjectRecord(ctx, record); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(ctx, "org", corestore.Serving, func(r tenantstore.Reader) error {
		got, err := r.ProjectRecord(ctx, "project")
		if err != nil {
			return err
		}
		if got.Core.Name != "Example" || got.Core.Slug != "example" || got.Settings.Revision != 1 {
			t.Fatalf("failed command persisted=%+v", got)
		}
		_, err = r.ProjectSlugOwner(ctx, "rejected")
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("failed slug persisted=%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProjectWorkflowKeepsOccupiedStates(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	ctx := t.Context()
	var record projectstore.Record
	if err := repository.WriteTenant(ctx, "org", corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		record, err = w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: "org", ID: "project", Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Done", Terminal: true}}})
		if err != nil {
			return err
		}
		_, err = w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: "org", ProjectID: "project", NativeID: "item", Number: 1, Title: "Example", WorkflowStateName: new("Todo")})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	removed := record
	removed.States = []tracker.NativeState{{Name: "Ready", Dispatchable: true}, {Name: "Done", Terminal: true}}
	err := repository.WriteTenant(ctx, "org", corestore.Serving, func(w tenantstore.Writer) error { _, err := w.UpdateProjectRecord(ctx, removed); return err })
	if !errors.Is(err, projectstore.ErrStateInUse) {
		t.Fatalf("remove occupied state=%v", err)
	}
	record.States = []tracker.NativeState{{Name: "Done", Terminal: true}, {Name: "Todo", Dispatchable: true}}
	if err := repository.WriteTenant(ctx, "org", corestore.Serving, func(w tenantstore.Writer) error { _, err := w.UpdateProjectRecord(ctx, record); return err }); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(ctx, "org", corestore.Serving, func(r tenantstore.Reader) error {
		got, err := r.ProjectRecord(ctx, "project")
		if err != nil {
			return err
		}
		if got.Settings.Revision != 2 || len(got.States) != 2 || got.States[0].Name != "Done" {
			t.Fatalf("reordered workflow=%+v", got)
		}
		item, err := r.WorkItem(ctx, "project", "item")
		if err == nil && (item.WorkflowStateName == nil || *item.WorkflowStateName != "Todo") {
			t.Fatalf("occupied state changed=%+v", item)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProjectRecordMigrationKeepsExistingTenants(t *testing.T) {
	admin := template.Database(t)
	admin.SetMaxOpenConns(1)
	provider, err := migrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(t.Context(), 20261101001936); err != nil {
		t.Fatal(err)
	}
	for _, org := range []string{"org_first", "org_second"} {
		if _, err := admin.ExecContext(t.Context(), "INSERT INTO projects(organization_id,id,name,slug,profile,content_settings_revision) VALUES($1,'shared','Example','example','native',3)", org); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.ExecContext(t.Context(), "SET ROLE detent_migration_v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "RESET ROLE"); err != nil {
		t.Fatal(err)
	}
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			record, err := r.ProjectBySlug(t.Context(), "example")
			if err == nil && (record.Core.OrganizationID != org || record.Core.ID != "shared" || record.Core.ContentSettingsRevision != 3 || record.Settings.Revision != 1) {
				t.Fatalf("migrated project=%+v", record)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	var forced int
	if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM pg_class WHERE relname IN ('projects','project_settings','project_slugs','project_workflow_states','project_workflow_transitions') AND relrowsecurity AND relforcerowsecurity").Scan(&forced); err != nil {
		t.Fatal(err)
	}
	if forced != 5 {
		t.Fatalf("forced tenant policies=%d, want 5", forced)
	}
}
