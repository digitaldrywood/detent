package accessrecords_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProjectRecordsKeepTenantRevisionsAndClaims(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	ctx := t.Context()
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}
	records := make(map[tracker.OrganizationID]projectstore.Record)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := pgstoretest.SeedCoreOrganization(ctx, w, org); err != nil {
				return err
			}
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
	at := claim.AcquiredAt
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		exec := func(query string, args ...any) {
			t.Helper()
			if _, err := admin.ExecContext(ctx, query, args...); err != nil {
				t.Fatal(err)
			}
		}
		exec(`INSERT INTO api_tokens(organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at) VALUES($1,'retained-runner-token','retained-runner',$2,'retained','worker',TRUE,$3,$3)`, org, strings.Repeat("a", 64), at)
		exec(`INSERT INTO runner_enrollments(organization_id,id,token_hash,scope,operations,created_at,expires_at,created_by) VALUES($1,'retained-enrollment',$2,'projects',ARRAY['claim'],$3,$4,'retained-runner-token')`, org, strings.Repeat("b", 64), at, at.Add(time.Hour))
		exec(`INSERT INTO runner_identities(organization_id,runner_id,machine_id,token_id,enrollment_id,scope,operations,capacity_limit,reported_capacity,created_at,expires_at,last_heartbeat_at) VALUES($1,'retained-runner','machine','retained-runner-token','retained-enrollment','projects',ARRAY['claim'],1,1,$2,$3,$2)`, org, at, at.Add(time.Hour))
		exec(`INSERT INTO runner_repositories(organization_id,runner_id,owner,name,private,listed_at) VALUES($1,'retained-runner','example-org','example-project',FALSE,$2)`, org, at)
		for _, project := range []tracker.ProjectID{"shared", "another"} {
			exec(`UPDATE project_settings SET repository='example-org/example-project',checkout_repository='example-org/example-project' WHERE organization_id=$1 AND project_id=$2`, org, project)
			exec(`INSERT INTO runner_enrollment_projects(organization_id,enrollment_id,project_id) VALUES($1,'retained-enrollment',$2)`, org, project)
			exec(`INSERT INTO project_secrets(organization_id,project_id,kind,organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at) VALUES($1,$2,'fly_sprites_token','example-org',$3,$4,$5,1,$6)`, org, project, []byte(string(org)+"/"+string(project)), []byte{2}, []byte{3}, at)
		}
	}
	type retirementResources struct {
		Projects     map[tracker.ProjectID]projectstore.Record
		Secrets      map[tracker.ProjectID]accessstore.Secret
		Enrollment   runnerstore.Enrollment
		Runner       runnerstore.Identity
		Repositories []runnerstore.Repository
	}
	readResources := func(org tracker.OrganizationID, retired bool) retirementResources {
		t.Helper()
		got := retirementResources{Projects: make(map[tracker.ProjectID]projectstore.Record), Secrets: make(map[tracker.ProjectID]accessstore.Secret)}
		if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
			for _, project := range []tracker.ProjectID{"shared", "another"} {
				record, err := r.ProjectRecord(ctx, project)
				if retired && project == "shared" {
					if !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("retired project still visible: %v", err)
					}
					if _, err := r.AccessSecret(ctx, project, "fly_sprites_token"); !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("retired project retained secret: %v", err)
					}
					continue
				}
				if err != nil {
					return err
				}
				got.Projects[project] = record
				secret, err := r.AccessSecret(ctx, project, "fly_sprites_token")
				if err != nil {
					return err
				}
				got.Secrets[project] = secret
			}
			var err error
			got.Enrollment, err = r.RunnerEnrollment(ctx, "retained-enrollment")
			if err != nil {
				return err
			}
			got.Runner, err = r.RunnerIdentity(ctx, "retained-runner")
			if err != nil {
				return err
			}
			got.Repositories, err = r.RunnerRepositories(ctx, "retained-runner", 100)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(got.Repositories) != 1 || got.Repositories[0].Owner != "example-org" || got.Repositories[0].Name != "example-project" {
			t.Fatal("shared repository fixture is missing")
		}
		for _, project := range got.Projects {
			if project.Settings.Repository != "example-org/example-project" || project.Settings.CheckoutRepository != "example-org/example-project" {
				t.Fatal("project no longer references the retained shared repository")
			}
		}

		return got
	}
	beforeFirst, beforeSecond := readResources("org_first", false), readResources("org_second", false)

	err = repository.WriteTenant(ctx, "org_second", corestore.Serving, func(w tenantstore.Writer) error { return w.RetireProject(ctx, "shared", 1, claim.AcquiredAt) })
	if !errors.Is(err, projectstore.ErrActiveWork) {
		t.Fatalf("delete claimed project=%v", err)
	}
	if got := readResources("org_second", false); !reflect.DeepEqual(got, beforeSecond) {
		t.Fatal("refused retirement changed project resources")
	}

	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error { return w.RetireProject(ctx, "shared", 2, claim.AcquiredAt) }); err != nil {
		t.Fatal(err)
	}
	delete(beforeFirst.Projects, "shared")
	delete(beforeFirst.Secrets, "shared")
	beforeFirst.Enrollment.ProjectIDs = []tracker.ProjectID{"another"}
	if got := readResources("org_first", true); !reflect.DeepEqual(got, beforeFirst) {
		t.Fatal("retirement changed sibling project or shared runner resources")
	}
	if got := readResources("org_second", false); !reflect.DeepEqual(got, beforeSecond) {
		t.Fatal("retirement changed another tenant's project resources")
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
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	ctx := t.Context()
	ids := []tracker.ProjectID{"project_a", "project_b", "project_c"}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedTenantAccess(t, repository, organization)
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			for _, id := range ids {
				if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: id, Name: string(id), Profile: "native"}, States: []tracker.NativeState{{Name: "Todo"}}}); err != nil {
					return err
				}
				if err := w.InitializeProjectAccess(ctx, id, "first"); err != nil {
					return err
				}
			}
			now := pgstoretest.HistoryAt()
			if err := w.CreateAccessCredential(ctx, accessstore.Credential{OrganizationID: organization, ID: "selected_key", Name: "Selected key", Hash: "selected_hash", Scope: "operator", NativeOnly: true, UserID: "first", MembershipID: "membership_first", KeyScope: "read", ProjectAccess: "selected", Kind: "personal", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			for _, id := range ids[1:] {
				if err := w.SetAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: organization, TokenID: "selected_key", ProjectID: id}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	selected := accessstore.ProjectAccessFilter{UserID: "first", Role: "admin", PrincipalID: "first", CredentialID: "selected_key", ProjectAccess: "selected", RequireCredentialGrant: true, Limit: 1}
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
	blocked := accessstore.ProjectAccessFilter{UserID: "first", Role: "admin", PrincipalID: "first", KeyKind: "personal", ProjectAccess: "selected", Projects: ids, BlockedProjects: []tracker.ProjectID{"project_b"}, Limit: 1}
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
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(organization)+"/array membership", func(t *testing.T) {
			for _, test := range []struct {
				name                    string
				projects, blocked, want []tracker.ProjectID
			}{
				{name: "two selected projects", projects: []tracker.ProjectID{"project_b", "project_c"}, want: []tracker.ProjectID{"project_b", "project_c"}},
				{name: "nil restriction retains credential grants", want: []tracker.ProjectID{"project_b", "project_c"}},
				{name: "empty restriction retains credential grants", projects: []tracker.ProjectID{}, want: []tracker.ProjectID{"project_b", "project_c"}},
				{name: "nil exclusions", projects: []tracker.ProjectID{"project_b", "project_c"}, want: []tracker.ProjectID{"project_b", "project_c"}},
				{name: "empty exclusions", projects: []tracker.ProjectID{"project_b", "project_c"}, blocked: []tracker.ProjectID{}, want: []tracker.ProjectID{"project_b", "project_c"}},
				{name: "exclusion with unrelated project", blocked: []tracker.ProjectID{"project_c", "outside"}, want: []tracker.ProjectID{"project_b"}},
				{name: "complete exclusions", blocked: []tracker.ProjectID{"project_b", "project_c"}},
				{name: "duplicates retain membership semantics", projects: []tracker.ProjectID{"project_b", "project_b"}, want: []tracker.ProjectID{"project_b"}},
				{name: "quoted input cannot escape scope", projects: []tracker.ProjectID{"project_b') OR 1=1 --"}},
			} {
				t.Run(test.name, func(t *testing.T) {
					query := selected
					query.After = ""
					query.Limit = 10
					query.Projects = test.projects
					query.BlockedProjects = test.blocked
					var got []tracker.ProjectID
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						rows, err := r.(accessstore.ProjectAccessPageReader).ProjectAccessPage(t.Context(), query)
						for _, row := range rows {
							got = append(got, row.ProjectID)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, test.want) {
						t.Fatalf("project membership=%v want=%v", got, test.want)
					}
				})
			}
		})
	}

	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(organization)+"/selected service credential", func(t *testing.T) {
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				for _, id := range []string{"service_selected", "service_empty"} {
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: id, Name: id, Hash: "hash_" + id, Scope: "operator", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: pgstoretest.HistoryAt(), UpdatedAt: pgstoretest.HistoryAt()}); err != nil {
						return err
					}
				}
				for _, project := range []tracker.ProjectID{"project_b", "project_c"} {
					if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: "service_selected", ProjectID: project}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name, credential string
				requireGrant     bool
				after            tracker.ProjectID
				want             []tracker.ProjectID
			}{
				{name: "selected first page excludes ungranted prefix", credential: "service_selected", requireGrant: true, want: []tracker.ProjectID{"project_b", "project_c"}},
				{name: "selected continuation", credential: "service_selected", requireGrant: true, after: "project_b", want: []tracker.ProjectID{"project_c"}},
				{name: "selected without grants", credential: "service_empty", requireGrant: true},
				{name: "bootstrap without required grants", credential: "service_empty", want: []tracker.ProjectID{"project_a", "project_b"}},
			} {
				t.Run(test.name, func(t *testing.T) {
					var got []tracker.ProjectID
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						rows, err := r.(accessstore.ProjectAccessPageReader).ProjectAccessPage(t.Context(), accessstore.ProjectAccessFilter{KeyKind: "service", CredentialID: test.credential, ProjectAccess: "selected", RequireCredentialGrant: test.requireGrant, After: test.after, Limit: 1})
						for _, row := range rows {
							got = append(got, row.ProjectID)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, test.want) {
						t.Fatalf("service project page=%v want=%v", got, test.want)
					}
				})
			}
		})
	}

}

func TestProjectRecordMutationRollsBackRelationships(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	ctx := t.Context()
	pgstoretest.SeedTenantAccess(t, repository, "org")
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
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	ctx := t.Context()
	pgstoretest.SeedTenantAccess(t, repository, "org")
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
	admin, fixture := pgstoretest.MigrationDatabaseAtVersion(t, 20261101001936)
	admin.SetMaxOpenConns(1)
	provider, err := pgstoretest.MigrationProvider(admin)
	if err != nil {
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
	pool := pgstoretest.RolePoolFromTemplate(t, fixture, admin, "detent_runtime_v2", "")
	repository, err := postgres.NewRepository(t.Context(), pool, pgstoretest.SmallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
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

func TestProjectIDByNameStaysInsideTheOrganization(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	ctx := t.Context()
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := pgstoretest.SeedCoreOrganization(ctx, w, org); err != nil {
				return err
			}
			for _, project := range []corestore.Project{{ID: tracker.ProjectID("shared_" + org), Name: "Example Project"}, {ID: tracker.ProjectID("own_" + org), Name: "Only " + string(org)}, {ID: tracker.ProjectID("retired_" + org), Name: "Retired Project"}} {
				project.OrganizationID, project.Profile = org, "native"
				if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: project, States: states}); err != nil {
					return err
				}
			}
			record, err := w.ProjectRecord(ctx, tracker.ProjectID("retired_"+org))
			if err != nil {
				return err
			}
			return w.RetireProject(ctx, record.Core.ID, record.Settings.Revision, time.Now().UTC())
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		other := tracker.OrganizationID("org_second")
		if org == other {
			other = "org_first"
		}
		for _, test := range []struct {
			name string
			want tracker.ProjectID
		}{
			{"example project", tracker.ProjectID("shared_" + org)},
			{"EXAMPLE PROJECT", tracker.ProjectID("shared_" + org)},
			{"Only " + string(org), tracker.ProjectID("own_" + org)},
			{"Only " + string(other), ""},
			{"Retired Project", ""},
			{"Example", ""},
		} {
			if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				got, err := r.ProjectIDByName(ctx, test.name)
				if test.want == "" {
					if !errors.Is(err, corestore.ErrNotFound) {
						t.Errorf("%s ProjectIDByName(%q) = %q, %v; want not found", org, test.name, got, err)
					}
					return nil
				}
				if err != nil || got != test.want {
					t.Errorf("%s ProjectIDByName(%q) = %q, %v; want %q", org, test.name, got, err, test.want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
