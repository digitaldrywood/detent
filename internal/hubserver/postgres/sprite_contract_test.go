package postgres_test

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
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
			settings := postgres.SpritePool{OrganizationID: org, MaxRunners: 10, IdleSeconds: 300, ConfiguredBy: "admin", IsolationTier: "native-trusted", PlacementMode: "blended", BootstrapObjectKey: new("objects/bootstrap/" + string(org))}
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
				operation := postgres.SpriteOperation{OrganizationID: org, IdempotencyKey: id, Name: id, Operation: "provision", CreatedAt: now, NextAttemptAt: now.Add(time.Second)}
				if _, err := w.CreateSpriteOperation(t.Context(), operation); err != nil {
					return err
				}
				replay, err := w.CreateSpriteOperation(t.Context(), operation)
				if err != nil {
					return err
				}
				if replay.Attempts != 0 || replay.Outcome != "pending" || !replay.CreatedAt.Equal(now) {
					t.Fatalf("replay=%+v", replay)
				}
			}
			return w.RecordSpriteProvisionFailure(t.Context(), "prj_first", 1)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
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
		{"conflicting replay", "org_first", func(w postgres.SpriteWriter) error {
			_, err := w.CreateSpriteOperation(t.Context(), postgres.SpriteOperation{OrganizationID: "org_first", IdempotencyKey: "first", Name: "second", Operation: "provision", CreatedAt: now, NextAttemptAt: now.Add(time.Second)})
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
			if settings.OrganizationID != org || settings.BootstrapObjectKey == nil || *settings.BootstrapObjectKey != "objects/bootstrap/"+string(org) {
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
				if row.OrganizationID != org || row.DesiredCount != int32(i+1) {
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
}
