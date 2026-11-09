package postgres_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSpriteContractPersistsScopedAllocationsAndLifecycleReceipts(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		for _, id := range []string{"first", "second"} {
			seedRunnerIdentity(t, repository, org, id, "machine_"+id, now, true)
		}
		err := repository.WriteSprite(t.Context(), org, corestore.Serving, func(w postgres.SpriteWriter) error {
			settings := postgres.SpritePool{OrganizationID: org, MaxRunners: 101, IdleSeconds: 300, ConfiguredBy: "admin", IsolationTier: "native-trusted", PlacementMode: "blended", BootstrapObjectKey: new("objects/bootstrap/" + string(org))}
			got, err := w.PutSpritePool(t.Context(), settings, 0)
			if err != nil {
				return err
			}
			if got.Revision != 1 {
				t.Fatalf("initial revision=%d", got.Revision)
			}
			for i, id := range []string{"first", "second"} {
				allocation := postgres.SpriteAllocation{OrganizationID: org, ProjectID: tracker.ProjectID("prj_" + id), DesiredCount: int32(i + 1), RequestedAt: now.Add(time.Duration(i) * time.Second), ConfiguredBy: "admin"}
				got, err := w.PutSpriteAllocation(t.Context(), allocation, 0)
				if err != nil {
					return err
				}
				if got.Revision != 1 {
					t.Fatal("allocation revision missing")
				}
				member := runnerstore.SpriteMembership{OrganizationID: org, Name: id, ProviderOrganization: "example", EnrollmentID: "enrollment_" + id, State: "bootstrapping", IdleSince: now, CreatedAt: now, AllocationProjectID: allocation.ProjectID, AllocationRevision: 1}
				if err := w.CreateRunnerSpriteMembership(t.Context(), member); err != nil {
					return err
				}
				operation := postgres.SpriteOperation{OrganizationID: org, IdempotencyKey: id, Name: id, Operation: "provision", RequestObjectKey: new("objects/request"), CreatedAt: now, NextAttemptAt: now.Add(time.Second)}
				if _, err := w.CreateSpriteOperation(t.Context(), operation); err != nil {
					return err
				}
				replay, err := w.CreateSpriteOperation(t.Context(), operation)
				if err != nil {
					return err
				}
				if replay.Attempts != 0 || replay.Outcome != "pending" || !replay.CreatedAt.Equal(now) || replay.RequestObjectKey == nil || *replay.RequestObjectKey != "objects/request" || replay.TargetEnrollmentID != "enrollment_"+id || replay.ProjectID != allocation.ProjectID {
					t.Fatalf("replay=%+v", replay)
				}
			}
			return w.RecordSpriteProvisionFailure(t.Context(), "prj_first", 1)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteSprite(t.Context(), org, corestore.Serving, func(w postgres.SpriteWriter) error {
			tenant, ok := w.(tenantstore.Writer)
			if !ok {
				t.Fatal("Sprite writes do not share the tenant transaction")
			}
			for i := range 22 {
				name := fmt.Sprintf("zz_history_%02d", i)
				enrollment := runnerstore.Enrollment{OrganizationID: org, ID: name, TokenHash: apikey.HashToken(string(org) + name), Scope: "organization", Operations: []string{"read"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: "admin"}
				if err := tenant.CreateRunnerEnrollment(t.Context(), enrollment); err != nil {
					return err
				}
				if err := w.CreateRunnerSpriteMembership(t.Context(), runnerstore.SpriteMembership{OrganizationID: org, Name: name, EnrollmentID: name, ProviderOrganization: "example", State: "deleted", CreatedAt: now.Add(time.Duration(i+1) * time.Minute), IdleSince: now, AllocationProjectID: "prj_first"}); err != nil {
					return err
				}
			}
			row, err := w.SpriteAllocation(t.Context(), "prj_first")
			if err != nil {
				return err
			}
			row.DesiredCount, row.RequestedAt = 3, now.Add(5*time.Second)
			row, err = w.PutSpriteAllocation(t.Context(), row, row.Revision)
			if err != nil {
				return err
			}
			row.DesiredCount, row.RequestedAt = 4, now.Add(10*time.Second)
			row, err = w.PutSpriteAllocation(t.Context(), row, row.Revision)
			if err != nil {
				return err
			}
			if !row.RequestedAt.Equal(now.Add(5 * time.Second)) {
				t.Fatalf("shortfall lost queue position: %+v", row)
			}
			member, err := w.SpriteMember(t.Context(), "second")
			if err != nil {
				return err
			}
			return w.UpdateRunnerSpriteMembership(t.Context(), runnerstore.SpriteMembership{OrganizationID: org, Name: member.Name, EnrollmentID: member.EnrollmentID, ProviderOrganization: member.ProviderOrganization, State: "deleting", CreatedAt: member.CreatedAt, IdleSince: member.IdleSince, AllocationProjectID: tracker.ProjectID(member.AllocationProjectID), AllocationRevision: member.AllocationRevision})
		}); err != nil {
			t.Fatal(err)
		}
	}
	rollbackCleanup := errors.New("rollback cleanup fixture")
	cases := []struct {
		name string
		org  tracker.OrganizationID
		run  func(postgres.SpriteWriter) error
		want error
	}{
		{"foreign settings", "org_first", func(w postgres.SpriteWriter) error {
			_, err := w.PutSpritePool(t.Context(), postgres.SpritePool{OrganizationID: "org_second"}, 1)
			return err
		}, corestore.ErrTenantMismatch},
		{"stale pool", "org_first", func(w postgres.SpriteWriter) error {
			row, err := w.SpritePool(t.Context())
			if err != nil {
				return err
			}
			_, err = w.PutSpritePool(t.Context(), row, 4)
			return err
		}, corestore.ErrRevisionConflict},
		{"stale allocation", "org_first", func(w postgres.SpriteWriter) error {
			row, err := w.SpriteAllocation(t.Context(), "prj_first")
			if err != nil {
				return err
			}
			_, err = w.PutSpriteAllocation(t.Context(), row, 4)
			return err
		}, corestore.ErrRevisionConflict},
		{"deleted project source", "org_first", func(w postgres.SpriteWriter) error {
			writer, ok := w.(tenantstore.Writer)
			if !ok {
				return accessstore.ErrTransactionRequired
			}
			project, err := writer.Project(t.Context(), "prj_first")
			if err != nil {
				return err
			}
			project.DeletedAt = &now
			if _, err := writer.UpdateProject(t.Context(), project); err != nil {
				return err
			}
			_, err = w.CreateSpriteOperation(t.Context(), postgres.SpriteOperation{OrganizationID: "org_first", IdempotencyKey: "deleted-project", Name: "first", Operation: "bootstrap", RequestObjectKey: new("objects/request"), CreatedAt: now, NextAttemptAt: now})
			return err
		}, corestore.ErrNotFound},
		{"exec session cannot bind a provision receipt", "org_first", func(w postgres.SpriteWriter) error {
			row, err := w.SpriteOperation(t.Context(), "first")
			if err != nil {
				return err
			}
			row.Outcome, row.UpdatedAt, row.NextAttemptAt = "uncertain", now, now
			if err := w.UpdateSpriteOperation(t.Context(), row, row.Attempts); err != nil {
				return err
			}
			row.Attempts++
			row.Operation = "bootstrap"
			return w.SetSpriteExecSession(t.Context(), row, "session-example")
		}, corestore.ErrRevisionConflict},
		{"cleanup survives retired project", "org_first", func(w postgres.SpriteWriter) error {
			writer := w.(tenantstore.Writer)
			project, err := writer.Project(t.Context(), "prj_first")
			if err != nil {
				return err
			}
			project.DeletedAt = &now
			if _, err := writer.UpdateProject(t.Context(), project); err != nil {
				return err
			}
			if _, err := w.CreateSpriteOperation(t.Context(), postgres.SpriteOperation{OrganizationID: "org_first", IdempotencyKey: "cleanup-retired-project", Name: "first", Operation: "delete", RequestObjectKey: new("objects/request"), CreatedAt: now, NextAttemptAt: now}); err != nil {
				return err
			}
			return rollbackCleanup
		}, rollbackCleanup},
		{"missing request payload", "org_first", func(w postgres.SpriteWriter) error {
			_, err := w.CreateSpriteOperation(t.Context(), postgres.SpriteOperation{OrganizationID: "org_first", IdempotencyKey: "missing-request", Name: "first", Operation: "provision", CreatedAt: now, NextAttemptAt: now})
			return err
		}, corestore.ErrInvalidPage},
		{"foreign enrollment binding", "org_first", func(w postgres.SpriteWriter) error {
			_, err := w.CreateSpriteOperation(t.Context(), postgres.SpriteOperation{OrganizationID: "org_first", IdempotencyKey: "foreign-binding", Name: "first", Operation: "provision", TargetEnrollmentID: "enrollment_second", ProjectID: "prj_first", RequestObjectKey: new("objects/request"), CreatedAt: now, NextAttemptAt: now})
			return err
		}, corestore.ErrNotFound},
		{"conflicting replay", "org_first", func(w postgres.SpriteWriter) error {
			_, err := w.CreateSpriteOperation(t.Context(), postgres.SpriteOperation{OrganizationID: "org_first", IdempotencyKey: "first", Name: "second", Operation: "provision", RequestObjectKey: new("objects/request"), CreatedAt: now, NextAttemptAt: now.Add(time.Second)})
			return err
		}, corestore.ErrUniqueViolation},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := repository.WriteSprite(t.Context(), test.org, corestore.Serving, test.run)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}
	if err := repository.WriteSprite(t.Context(), "org_first", corestore.Serving, func(w postgres.SpriteWriter) error {
		row, err := w.SpriteOperation(t.Context(), "first")
		if err != nil {
			return err
		}
		row.Outcome = "uncertain"
		row.UpdatedAt = now
		row.NextAttemptAt = now.Add(time.Minute)
		return w.UpdateSpriteOperation(t.Context(), row, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteSprite(t.Context(), "org_first", corestore.Serving, func(w postgres.SpriteWriter) error {
		row, err := w.SpriteOperation(t.Context(), "first")
		if err != nil {
			return err
		}
		row.Outcome = "committed"
		row.ReceiptObjectKey = new("objects/provider-receipt")
		row.UpdatedAt = now.Add(time.Minute)
		return w.UpdateSpriteOperation(t.Context(), row, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteSprite(t.Context(), "org_first", corestore.Serving, func(w postgres.SpriteWriter) error {
		row, err := w.CreateSpriteOperation(t.Context(), postgres.SpriteOperation{OrganizationID: "org_first", IdempotencyKey: "failed-source", Name: "first", Operation: "bootstrap", Attempts: 7, SourceRevision: "current-source", ProviderResourceID: "owned-resource", RequestObjectKey: new("objects/request"), CreatedAt: now, NextAttemptAt: now})
		if err != nil {
			return err
		}
		if row.Attempts != 7 || row.SourceRevision != "current-source" || row.ProviderResourceID != "owned-resource" {
			t.Fatalf("replacement source was lost: %+v", row)
		}
		row.Outcome, row.ReceiptObjectKey, row.UpdatedAt, row.NextAttemptAt = "rejected", new("objects/failure-receipt"), now, now.Add(time.Minute)
		return w.UpdateSpriteOperation(t.Context(), row, 7)
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteSprite(t.Context(), "org_first", corestore.Serving, func(w postgres.SpriteWriter) error {
		row, err := w.CreateSpriteOperation(t.Context(), postgres.SpriteOperation{OrganizationID: "org_first", IdempotencyKey: "no-delay-source", Name: "first", Operation: "provision", Attempts: 90, SourceRevision: "current-source", RequestObjectKey: new("objects/request"), CreatedAt: now.Add(time.Second), NextAttemptAt: now.Add(time.Second)})
		if err != nil {
			return err
		}
		row.Outcome, row.ReceiptObjectKey, row.UpdatedAt = "rejected", new("objects/no-delay"), now.Add(time.Second)
		return w.UpdateSpriteOperation(t.Context(), row, row.Attempts)
	}); err != nil {
		t.Fatal(err)
	}
	var unbound int
	if err := pool.QueryRow(pgtest.UnboundProbe(t.Context()), `SELECT count(*) FROM organization_sprite_pools`).Scan(&unbound); err != nil || unbound != 0 {
		t.Fatalf("unbound rows=%d err=%v", unbound, err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, _ = repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := repository.ReadSprite(t.Context(), org, corestore.Serving, func(r postgres.SpriteReader) error {
			settings, err := r.SpritePool(t.Context())
			if err != nil {
				return err
			}
			if settings.MaxRunners != 101 || settings.OrganizationID != org || settings.BootstrapObjectKey == nil || *settings.BootstrapObjectKey != "objects/bootstrap/"+string(org) {
				t.Fatalf("pool=%+v", settings)
			}
			allocations, err := r.SpriteAllocations(t.Context(), "", 2)
			if err != nil {
				return err
			}
			if len(allocations) != 2 {
				t.Fatalf("allocations=%+v", allocations)
			}
			for i, row := range allocations {
				want := int32(i + 1)
				if i == 0 {
					want = 4
				}
				if row.OrganizationID != org || row.DesiredCount != want {
					t.Fatalf("allocation=%+v", row)
				}
			}
			members, err := r.SpriteMembers(t.Context(), "", 2)
			if err != nil {
				return err
			}
			if len(members) != 2 {
				t.Fatalf("members=%+v", members)
			}
			for _, member := range members {
				if member.OrganizationID != org || member.AllocationProjectID == "" {
					t.Fatalf("member=%+v", member)
				}
			}
			for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
				count, err := r.SpriteActiveMemberCount(t.Context(), project)
				if err != nil {
					return err
				}
				if count != 1 {
					t.Fatalf("%s active count=%d", project, count)
				}
			}
			activeCount, err := r.SpritePoolMemberCount(t.Context())
			if err != nil {
				return err
			}
			if activeCount != 2 {
				t.Fatalf("pool count=%d", activeCount)
			}
			retained, err := r.SpriteRetainedPoolCount(t.Context())
			if err != nil || retained != 1 {
				t.Fatalf("retained pool count=%d err=%v", retained, err)
			}
			for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
				retained, err := r.SpriteRetainedMemberCount(t.Context(), project)
				want := int64(1)
				if project == "prj_second" {
					want = 0
				}
				if err != nil || retained != want {
					t.Fatalf("%s retained count=%d want=%d err=%v", project, retained, want, err)
				}
			}
			queue, err := r.SpriteAllocationQueue(t.Context(), postgres.SpriteAllocationCursor{}, 1)
			if err != nil {
				return err
			}
			if len(queue) != 1 || queue[0].ProjectID != "prj_second" {
				t.Fatalf("first queue page=%+v", queue)
			}
			queue, err = r.SpriteAllocationQueue(t.Context(), postgres.SpriteAllocationCursor{RequestedAt: queue[0].RequestedAt, ProjectID: queue[0].ProjectID}, 1)
			if err != nil {
				return err
			}
			if len(queue) != 1 || queue[0].ProjectID != "prj_first" {
				t.Fatalf("second queue page=%+v", queue)
			}
			seen := map[string]bool{}
			cursor := postgres.SpriteMemberCursor{}
			for range 4 {
				page, err := r.SpritePoolMemberPage(t.Context(), cursor, 7)
				if err != nil {
					return err
				}
				for _, view := range page {
					m := view.RunnerSpriteMembership
					if m.OrganizationID != org || seen[m.Name] || m.Name == "zz_history_00" || m.Name == "zz_history_01" {
						t.Fatalf("invalid pool page member=%+v", view)
					}
					if m.State != "deleted" && view.RunnerID != m.Name {
						t.Fatalf("runner binding=%+v", view)
					}
					seen[m.Name] = true
					cursor = postgres.SpriteMemberCursor{CreatedAt: m.CreatedAt, Name: m.Name}
				}
			}
			if len(seen) != 22 {
				t.Fatalf("active and recent deleted members=%d", len(seen))
			}
			failure, failureErr := r.SpriteProvisionSourceFailure(t.Context(), "prj_first", "current-source")
			if org == "org_first" {
				if failureErr != nil || failure.Attempts != 8 || !failure.NextAttemptAt.Equal(now.Add(time.Minute)) || failure.ProviderResourceID != "owned-resource" {
					t.Fatalf("failure source after reopen=%+v err=%v", failure, failureErr)
				}
			} else if !errors.Is(failureErr, corestore.ErrNotFound) {
				t.Fatalf("foreign failure source=%+v err=%v", failure, failureErr)
			}
			for _, source := range []struct {
				project  tracker.ProjectID
				revision string
			}{{"prj_second", "current-source"}, {"prj_first", "changed-source"}} {
				if _, err := r.SpriteProvisionSourceFailure(t.Context(), source.project, source.revision); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("source/project boundary error=%v", err)
				}
			}
			operation, err := r.SpriteOperation(t.Context(), "first")
			if err != nil {
				return err
			}
			want := "pending"
			if org == "org_first" {
				want = "committed"
			}
			if operation.Outcome != want {
				t.Fatalf("operation=%+v", operation)
			}
			pending, err := r.PendingSpriteOperations(t.Context(), now.Add(time.Second), 2)
			if err != nil {
				return err
			}
			count := 2
			if org == "org_first" {
				count = 1
			}
			if len(pending) != count {
				t.Fatalf("pending=%+v", pending)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRunnerIdentity(t, repository, org, string(org)+"_filled", "machine_filled", now, true)
		if err := repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
			owner := w.(postgres.SpriteWriter)
			for i := range 101 {
				id := tracker.ProjectID(fmt.Sprintf("completed-%03d", i))
				if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: id, Slug: string(id), Name: string(id), Profile: "native"}); err != nil {
					return err
				}
				desired := int32(0)
				if i == 100 {
					desired = 1
				}
				if _, err := owner.PutSpriteAllocation(t.Context(), postgres.SpriteAllocation{OrganizationID: org, ProjectID: id, DesiredCount: desired, ConfiguredBy: "admin", RequestedAt: now.Add(-time.Duration(i+1) * time.Minute)}, 0); err != nil {
					return err
				}
				if desired == 1 {
					if err := owner.CreateRunnerSpriteMembership(t.Context(), runnerstore.SpriteMembership{OrganizationID: org, Name: "filled", ProviderOrganization: "example", EnrollmentID: "enrollment_" + string(org) + "_filled", AllocationProjectID: id, AllocationRevision: 1, State: "bootstrapping", IdleSince: now, CreatedAt: now}); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadSprite(t.Context(), org, corestore.Serving, func(r postgres.SpriteReader) error {
			cursor := postgres.SpriteAllocationCursor{}
			for _, project := range []tracker.ProjectID{"prj_second", "prj_first"} {
				page, err := r.SpriteProvisionQueue(t.Context(), cursor, 1)
				if err != nil {
					return err
				}
				if len(page) != 1 || page[0].OrganizationID != org || page[0].ProjectID != project {
					t.Fatalf("pending allocation queue for %s: %+v", org, page)
				}
				cursor = postgres.SpriteAllocationCursor{RequestedAt: page[0].RequestedAt, ProjectID: page[0].ProjectID}
			}
			page, err := r.SpriteProvisionQueue(t.Context(), cursor, 1)
			if err != nil {
				return err
			}
			if len(page) != 0 {
				t.Fatalf("fulfilled allocation leaked into provisioning: %+v", page)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteSprite(t.Context(), org, corestore.Serving, func(w postgres.SpriteWriter) error {
			row, err := w.SpriteAllocation(t.Context(), "completed-100")
			if err != nil {
				return err
			}
			row.DesiredCount = 0
			_, err = w.PutSpriteAllocation(t.Context(), row, row.Revision)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadSprite(t.Context(), org, corestore.Serving, func(r postgres.SpriteReader) error {
			projects, err := r.SpriteRetirementProjects(t.Context(), []string{"second", "filled", "first", "filled", "absent"})
			if err != nil {
				return err
			}
			want := []tracker.ProjectID{"completed-100", "prj_first", "prj_second"}
			if len(projects) != len(want) {
				t.Fatalf("retirement projects=%v, want %v", projects, want)
			}
			for i, project := range want {
				if projects[i] != project {
					t.Fatalf("retirement projects=%v, want %v", projects, want)
				}
			}
			projects, err = r.SpriteRetirementProjects(t.Context(), []string{"first"})
			if err != nil {
				return err
			}
			if len(projects) != 1 || projects[0] != "prj_first" {
				t.Fatalf("retirement ignored member scope: %v", projects)
			}
			projects, err = r.SpriteRetirementProjects(t.Context(), []string{"absent"})
			if err != nil {
				return err
			}
			if len(projects) != 0 {
				t.Fatalf("retirement returned unselected members: %v", projects)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		var busy postgres.SpriteOperation
		if err := repository.WriteSprite(t.Context(), org, corestore.Serving, func(w postgres.SpriteWriter) error {
			for _, name := range []string{"first", "second"} {
				row, err := w.CreateSpriteOperation(t.Context(), postgres.SpriteOperation{OrganizationID: org, IdempotencyKey: "delete:" + name, Name: name, Operation: "delete", RequestObjectKey: new("objects/request"), ProviderResourceID: "resource-" + name, CreatedAt: now, NextAttemptAt: now})
				if err != nil {
					return err
				}
				if name == "first" {
					busy = row
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		checked := now.Add(500 * time.Millisecond)
		if err := repository.WriteSprite(t.Context(), org, corestore.Serving, func(w postgres.SpriteWriter) error {
			return w.RescheduleBusySpriteOperation(t.Context(), busy, checked)
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadSprite(t.Context(), org, corestore.Serving, func(r postgres.SpriteReader) error {
			pending, err := r.PendingSpriteOperations(t.Context(), checked, 1)
			if err != nil {
				return err
			}
			if len(pending) != 1 || pending[0].IdempotencyKey != "delete:second" || pending[0].OrganizationID != org {
				t.Fatalf("busy operation kept the bounded queue head: %+v", pending)
			}
			current, err := r.SpriteOperation(t.Context(), busy.IdempotencyKey)
			if err != nil {
				return err
			}
			if current.Attempts != 0 || current.Outcome != "pending" || !current.NextAttemptAt.Equal(checked) || current.TargetEnrollmentID != "enrollment_first" || current.ProviderResourceID != "resource-first" || current.RequestObjectKey == nil || *current.RequestObjectKey != "objects/request" {
				t.Fatalf("queue reschedule changed execution authority: %+v", current)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		stale := busy
		stale.Attempts++
		if err := repository.WriteSprite(t.Context(), org, corestore.Serving, func(w postgres.SpriteWriter) error {
			return w.RescheduleBusySpriteOperation(t.Context(), stale, checked.Add(time.Second))
		}); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("stale operation changed queue position: %v", err)
		}
		if err := repository.WriteSprite(t.Context(), org, corestore.Serving, func(w postgres.SpriteWriter) error {
			return w.RescheduleBusySpriteOperation(t.Context(), busy, now.Add(-time.Second))
		}); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("backdated operation changed queue position: %v", err)
		}
		claims := map[string]leasestore.Lease{}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			for _, name := range []string{"first", "second", "local"} {
				machine := "machine_" + name
				if name == "local" {
					if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: machine, Hostname: "local", Capacity: 1, Version: "example"}); err != nil {
						return err
					}
				} else {
					profile, err := w.Machine(t.Context(), machine)
					if err != nil {
						return err
					}
					profile.Hostname, profile.SpriteName = name, name
					if err := w.RegisterMachine(t.Context(), profile); err != nil {
						return err
					}
				}
				project, item := tracker.ProjectID("prj_"+name), tracker.NativeWorkItemID("wi_"+name+"_1")
				if name == "local" {
					project, item = "prj_first", "wi_first_2"
				}
				row, err := w.WorkItem(t.Context(), project, item)
				if err != nil {
					return err
				}
				claim := leasestore.Claim{IssueID: row.ID, ExpectedRevision: row.Revision, MachineID: machine, SessionID: "slot-session-" + name, LeaseID: "slot-lease-" + name, TTL: 2 * time.Hour}
				if name == "first" {
					claim.LandingCapacity, claim.TTL = 1, time.Hour
				}
				claims[name], err = w.Claim(t.Context(), claim)
				if err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadSprite(t.Context(), org, corestore.Serving, func(r postgres.SpriteReader) error {
			for _, test := range []struct {
				name    string
				at      time.Time
				limit   int64
				joining postgres.SpriteSlotGroup
				want    int64
			}{
				{name: "active Sprite jobs span projects and exclude local jobs", at: claims["second"].AcquiredAt, limit: 3, want: 2},
				{name: "bounded capacity result", at: claims["second"].AcquiredAt, limit: 1, want: 1},
				{name: "joining landing group reuses its slot", at: claims["second"].AcquiredAt, limit: 3, joining: postgres.SpriteSlotGroup{MachineID: "machine_first", ProjectID: "prj_first", SessionID: "slot-session-first"}, want: 1},
				{name: "another project cannot reuse a landing slot", at: claims["second"].AcquiredAt, limit: 3, joining: postgres.SpriteSlotGroup{MachineID: "machine_first", ProjectID: "prj_second", SessionID: "slot-session-first"}, want: 2},
				{name: "another machine cannot reuse a landing slot", at: claims["second"].AcquiredAt, limit: 3, joining: postgres.SpriteSlotGroup{MachineID: "machine_second", ProjectID: "prj_first", SessionID: "slot-session-first"}, want: 2},
				{name: "ordinary jobs retain separate slots", at: claims["second"].AcquiredAt, limit: 3, joining: postgres.SpriteSlotGroup{MachineID: "machine_second", ProjectID: "prj_second", SessionID: "slot-session-second"}, want: 2},
				{name: "microsecond before expiry remains active", at: claims["first"].ExpiresAt.Add(-time.Microsecond), limit: 3, want: 2},
				{name: "exact expiry releases its slot", at: claims["first"].ExpiresAt, limit: 3, want: 1},
				{name: "last expiry empties capacity", at: claims["second"].ExpiresAt, limit: 3, want: 0},
			} {
				count, err := r.SpriteJobSlotCount(t.Context(), test.at, test.limit, test.joining)
				if err != nil || count != test.want {
					t.Fatalf("%s: count=%d want=%d error=%v", test.name, count, test.want, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: claims["first"].LeaseID, FencingToken: claims["first"].FencingToken}, "completed")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadSprite(t.Context(), org, corestore.Serving, func(r postgres.SpriteReader) error {
			count, err := r.SpriteJobSlotCount(t.Context(), claims["second"].AcquiredAt, 3, postgres.SpriteSlotGroup{})
			if err != nil || count != 1 {
				t.Fatalf("released lease retained a Sprite slot: count=%d error=%v", count, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSpriteRunnerSelectionHonorsAllocationAndSharing(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	for _, organization := range organizations {
		seedTenantAccess(t, repository, organization)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			for _, name := range []string{"first", "second", "unmanaged"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: tracker.ProjectID("prj_" + name), Name: name, Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"first", "second"} {
			id := string(organization) + "_" + name
			seedRunnerIdentity(t, repository, organization, id, "machine_"+name, now, true)
			seedRunnerIdentity(t, repository, organization, "unowned_"+id, "unowned_machine_"+name, now, true)
		}
		if err := repository.WriteSprite(t.Context(), organization, corestore.Serving, func(w postgres.SpriteWriter) error {
			if _, err := w.PutSpritePool(t.Context(), postgres.SpritePool{OrganizationID: organization, MaxRunners: 2, IdleSeconds: 300, ConfiguredBy: "admin", IsolationTier: "native-trusted", PlacementMode: "blended"}, 0); err != nil {
				return err
			}
			for _, name := range []string{"first", "second"} {
				id := string(organization) + "_" + name
				project := tracker.ProjectID("prj_" + name)
				if _, err := w.PutSpriteAllocation(t.Context(), postgres.SpriteAllocation{OrganizationID: organization, ProjectID: project, DesiredCount: 1, RequestedAt: now, ConfiguredBy: "admin"}, 0); err != nil {
					return err
				}
				if err := w.CreateRunnerSpriteMembership(t.Context(), runnerstore.SpriteMembership{OrganizationID: organization, Name: name, EnrollmentID: "enrollment_" + id, ProviderOrganization: "example", State: "enrolled", AllocationProjectID: project, AllocationRevision: 1, IdleSince: now, CreatedAt: now}); err != nil {
					return err
				}
				writer := w.(tenantstore.Writer)
				for _, target := range []tracker.ProjectID{"prj_first", "prj_second"} {
					if err := writer.ReplaceRunnerLocalChecks(t.Context(), id, target, runnerauth.LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"codex"}, ObservedAt: now}); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	rollback := errors.New("rollback selection fixture")
	for _, organization := range organizations {
		for _, name := range []string{"first", "second"} {
			for _, test := range []struct {
				name, change string
				other, want  bool
			}{
				{name: "own ready member", want: true},
				{name: "unmanaged local obeys Sprites-only", change: "unowned local unmanaged"},
				{name: "managed local obeys Sprites-only", change: "unowned local managed"},
				{name: "unmanaged manual Sprite remains eligible", change: "unowned sprite unmanaged", want: true},
				{name: "managed manual Sprite remains eligible", change: "unowned sprite managed", want: true},
				{name: "unmanaged blended local remains eligible", change: "unowned local blended", want: true},
				{name: "organization member", change: "unallocated", want: true},
				{name: "disabled organization pool", change: "disabled"},
				{name: "sharing requires consent", other: true},
				{name: "shared ready member", change: "sharing", other: true, want: true},
				{name: "shared target desired zero", change: "target zero", other: true},
				{name: "owner desired zero", change: "owner zero"},
				{name: "retiring member", change: "retiring"},
				{name: "runner draining", change: "draining"},
				{name: "revoked runner token", change: "revoked"},
				{name: "missing runner grant", change: "grant"},
				{name: "failed target checkout", change: "checks"},
				{name: "foreign tenant identity", change: "foreign"},
			} {
				t.Run(string(organization)+"/"+name+"/"+test.name, func(t *testing.T) {
					err := repository.WriteSprite(t.Context(), organization, corestore.Serving, func(w postgres.SpriteWriter) error {
						writer := w.(tenantstore.Writer)
						id := string(organization) + "_" + name
						project := tracker.ProjectID("prj_" + name)
						target := project
						if test.other {
							target = "prj_first"
							if name == "first" {
								target = "prj_second"
							}
						}
						switch test.change {
						case "unowned local unmanaged", "unowned local managed", "unowned sprite unmanaged", "unowned sprite managed", "unowned local blended":
							id = "unowned_" + id
							if strings.HasSuffix(test.change, "unmanaged") || strings.HasSuffix(test.change, "blended") {
								target = "prj_unmanaged"
							}
							if strings.Contains(test.change, "sprite") {
								identity, err := writer.RunnerIdentity(t.Context(), id)
								if err != nil {
									return err
								}
								machine, err := writer.Machine(t.Context(), string(identity.MachineID))
								if err != nil {
									return err
								}
								machine.SpriteName = machine.Hostname
								if err := writer.HeartbeatMachine(t.Context(), machine, now); err != nil {
									return err
								}
							}
							if !strings.HasSuffix(test.change, "blended") {
								pool, err := w.SpritePool(t.Context())
								if err != nil {
									return err
								}
								pool.PlacementMode = "sprites_only"
								if _, err := w.PutSpritePool(t.Context(), pool, pool.Revision); err != nil {
									return err
								}
							}
						case "unallocated", "disabled":
							member, err := writer.RunnerSpriteMembershipByEnrollment(t.Context(), "enrollment_"+id)
							if err != nil {
								return err
							}
							member.AllocationProjectID, member.AllocationRevision = "", 0
							if err := writer.UpdateRunnerSpriteMembership(t.Context(), member); err != nil {
								return err
							}
							target = "prj_unmanaged"
							if test.change == "disabled" {
								pool, err := w.SpritePool(t.Context())
								if err != nil {
									return err
								}
								pool.MaxRunners = 0
								if _, err := w.PutSpritePool(t.Context(), pool, pool.Revision); err != nil {
									return err
								}
							}
						case "sharing", "target zero":
							pool, err := w.SpritePool(t.Context())
							if err != nil {
								return err
							}
							pool.ShareIdleCapacity = true
							if _, err := w.PutSpritePool(t.Context(), pool, pool.Revision); err != nil {
								return err
							}
						case "retiring":
							member, err := w.SpriteMember(t.Context(), name)
							if err != nil {
								return err
							}
							if err := writer.UpdateRunnerSpriteMembership(t.Context(), runnerstore.SpriteMembership{OrganizationID: organization, Name: member.Name, EnrollmentID: member.EnrollmentID, ProviderOrganization: member.ProviderOrganization, State: "deleting", CreatedAt: member.CreatedAt, IdleSince: member.IdleSince, AllocationProjectID: project, AllocationRevision: 1}); err != nil {
								return err
							}
						case "draining", "grant":
							identity, err := writer.RunnerIdentity(t.Context(), id)
							if err != nil {
								return err
							}
							if test.change == "draining" {
								identity.State = "draining"
							} else {
								identity.Scope = "projects"
							}
							if err := writer.UpdateRunnerRouting(t.Context(), identity.Revision, identity); err != nil {
								return err
							}
						case "revoked":
							if err := writer.RevokeAccessCredential(t.Context(), id, now); err != nil {
								return err
							}
						case "checks":
							if err := writer.ReplaceRunnerLocalChecks(t.Context(), id, target, runnerauth.LocalChecks{Checkout: "failed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"codex"}, ObservedAt: now}); err != nil {
								return err
							}
						case "foreign":
							foreign := organizations[0]
							if foreign == organization {
								foreign = organizations[1]
							}
							id = string(foreign) + "_" + name
						}
						if test.change == "target zero" || test.change == "owner zero" {
							allocation, err := w.SpriteAllocation(t.Context(), target)
							if err != nil {
								return err
							}
							allocation.DesiredCount = 0
							if _, err := w.PutSpriteAllocation(t.Context(), allocation, allocation.Revision); err != nil {
								return err
							}
						}
						allowed, err := w.SpriteRunnerAllowed(t.Context(), id, target, now)
						if err != nil {
							return err
						}
						if allowed != test.want {
							t.Errorf("allowed=%t want=%t", allowed, test.want)
						}
						return rollback
					})
					if !errors.Is(err, rollback) {
						t.Fatal(err)
					}
				})
			}
		}
	}
}
