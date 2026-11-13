package postgres_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSpriteContractPersistsScopedAllocationsAndLifecycleReceipts(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, repository, org)
		for _, id := range []string{"first", "second"} {
			pgstoretest.SeedRunnerIdentity(t, repository, org, id, "machine_"+id, now, true)
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
	repository, _ = pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
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

	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org)+"/current_pool_authority", func(t *testing.T) {
			for _, tc := range []struct {
				name       string
				scope      string
				nativeOnly bool
				role       string
				active     bool
				revoked    bool
				expired    bool
				future     bool
				allowed    bool
			}{
				{name: "current_admin", scope: "admin", allowed: true},
				{name: "native_only_admin", scope: "admin", nativeOnly: true},
				{name: "current_owner", scope: "operator", nativeOnly: true, role: "owner", active: true, allowed: true},
				{name: "current_member_admin", scope: "operator", nativeOnly: true, role: "admin", active: true, allowed: true},
				{name: "demoted_member", scope: "operator", nativeOnly: true, role: "member", active: true},
				{name: "inactive_owner", scope: "operator", nativeOnly: true, role: "owner"},
				{name: "revoked_admin", scope: "admin", revoked: true},
				{name: "expired_admin", scope: "admin", expired: true},
				{name: "future_admin", scope: "admin", future: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					rollback := errors.New("pool authority rollback")
					err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						credential := accessstore.Credential{OrganizationID: org, ID: "pool_authority", Name: "pool authority", Hash: apikey.HashToken(string(org) + "/pool_authority"), Scope: tc.scope, NativeOnly: tc.nativeOnly, Kind: "service", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now}
						if tc.revoked {
							credential.RevokedAt = &now
						}
						if tc.expired {
							credential.ExpiresAt = &now
						}
						if tc.future {
							credential.CreatedAt = now.Add(time.Second)
						}
						if err := w.CreateAccessCredential(t.Context(), credential); err != nil {
							return err
						}
						if tc.role != "" {
							if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: "pool_owner", Email: "owner@example.test", MembershipID: "pool_membership", Role: tc.role, Active: tc.active, PrincipalID: credential.ID, CreatedAt: now, UpdatedAt: now}); err != nil {
								return err
							}
						}
						owner := w.(postgres.SpriteWriter)
						row, err := owner.SpritePool(t.Context())
						if err != nil {
							return err
						}
						row.ConfiguredBy = credential.ID
						if _, err := owner.PutSpritePool(t.Context(), row, row.Revision); err != nil {
							return err
						}
						actor, err := owner.SpritePoolAuthority(t.Context(), now)
						if tc.allowed {
							if err != nil || actor != credential.ID {
								t.Errorf("current authority=%q error=%v", actor, err)
							}
						} else if !errors.Is(err, corestore.ErrNotFound) {
							t.Errorf("ineligible authority=%q error=%v", actor, err)
						}
						return rollback
					})
					if !errors.Is(err, rollback) {
						t.Fatal(err)
					}
				})
			}
		})
		t.Run(string(org)+"/current_and_recent_deleted_members", func(t *testing.T) {
			for index := range 21 {
				id := fmt.Sprintf("retired_%02d", index)
				pgstoretest.SeedRunnerIdentity(t, repository, org, id, "machine_"+id, now, true)
				if err := repository.WriteSprite(t.Context(), org, corestore.Serving, func(w postgres.SpriteWriter) error {
					return w.CreateRunnerSpriteMembership(t.Context(), runnerstore.SpriteMembership{OrganizationID: org, Name: id, ProviderOrganization: "example", EnrollmentID: "enrollment_" + id, State: "deleted", BootstrapLog: "log_" + id, IdleSince: now, CreatedAt: now.Add(time.Second), AllocationProjectID: "prj_first", AllocationRevision: 1})
				}); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := repository.ReadSprite(t.Context(), org, corestore.Serving, func(r postgres.SpriteReader) error {
					rows, err := r.SpritePoolMembers(t.Context())
					if err != nil {
						return err
					}
					if len(rows) != 22 {
						t.Fatalf("current and recent deleted count=%d; want22", len(rows))
					}
					for i, row := range rows {
						want := "first"
						if i == 1 {
							want = "second"
						}
						if i >= 2 {
							want = fmt.Sprintf("retired_%02d", i-1)
						}
						if row.OrganizationID != org || row.Name != want || row.RunnerID != want {
							t.Fatalf("pool member[%d]=%+v; want tenant=%s/name=%s", i, row, org, want)
						}
						if i >= 2 && (row.State != "deleted" || row.BootstrapLog != "log_"+want || !row.CreatedAt.Equal(now.Add(time.Second))) {
							t.Fatalf("retained deleted metadata=%+v", row)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}

}

func TestSpriteWakeFactsPreserveNullsAndFenceMachineBinding(t *testing.T) {
	repository, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	now := pgstoretest.HistoryAt()
	for _, org := range []tracker.OrganizationID{"org_wake_first", "org_wake_second"} {
		pgstoretest.SeedRepository(t, repository, org)
		for _, id := range []string{"first", "second"} {
			machineID := "machine_" + id
			pgstoretest.SeedRunnerIdentity(t, repository, org, id, machineID, now, true)
			err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				machine, err := w.Machine(t.Context(), machineID)
				if err != nil {
					return err
				}
				if machine.SpriteWokenAt != nil || machine.SpriteWakeFailedAt != nil {
					t.Fatalf("new machine has wake facts: %+v", machine)
				}
				machine.Hostname, machine.SpriteName = id, id
				return w.RegisterMachine(t.Context(), machine)
			})
			if err != nil {
				t.Fatal(err)
			}
			failed := now.Add(time.Second)
			woken := now.Add(2 * time.Second)
			later := now.Add(3 * time.Second)
			for _, test := range []struct {
				name                  string
				update                leasestore.SpriteWake
				wantWoken, wantFailed *time.Time
				wantError             error
			}{
				{"failed wake preserves null success", leasestore.SpriteWake{MachineID: machineID, RunnerID: id, Name: id, FailedAt: &failed, ObservedAt: failed}, nil, &failed, nil},
				{"successful wake clears failure", leasestore.SpriteWake{MachineID: machineID, RunnerID: id, Name: id, WokenAt: &woken, ObservedAt: woken}, &woken, nil, nil},
				{"later failure preserves success", leasestore.SpriteWake{MachineID: machineID, RunnerID: id, Name: id, FailedAt: &later, ObservedAt: later}, &woken, &later, nil},
				{"wrong runner cannot update facts", leasestore.SpriteWake{MachineID: machineID, RunnerID: "different", Name: id, ObservedAt: later}, &woken, &later, corestore.ErrNotFound},
				{"wrong name cannot update facts", leasestore.SpriteWake{MachineID: machineID, RunnerID: id, Name: "different", ObservedAt: later}, &woken, &later, corestore.ErrNotFound},
			} {
				t.Run(string(org)+"/"+id+"/"+test.name, func(t *testing.T) {
					err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.UpdateMachineSpriteWake(t.Context(), test.update) })
					if !errors.Is(err, test.wantError) {
						t.Fatalf("write error=%v want=%v", err, test.wantError)
					}
					err = repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						machine, err := r.Machine(t.Context(), machineID)
						if err != nil {
							return err
						}
						for _, fact := range []struct {
							name      string
							got, want *time.Time
						}{{"woken", machine.SpriteWokenAt, test.wantWoken}, {"failed", machine.SpriteWakeFailedAt, test.wantFailed}} {
							if fact.got == nil && fact.want != nil || fact.got != nil && (fact.want == nil || !fact.got.Equal(*fact.want)) {
								t.Fatalf("%s=%v want=%v", fact.name, fact.got, fact.want)
							}
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func TestSpriteLeaseUsageCountsUnassignedLeasesAndExpiryBoundary(t *testing.T) {
	repository, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_usage_first", "org_usage_second"} {
		pgstoretest.SeedLeaseFamily(t, repository, org)
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			for _, id := range []string{"machine_1", "machine_2"} {
				machine, err := w.Machine(t.Context(), id)
				if err != nil {
					return err
				}
				machine.Hostname, machine.SpriteName = id, id
				if err := w.RegisterMachine(t.Context(), machine); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		first := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(1))
		second := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(2))
		expired := first.ExpiresAt
		if second.ExpiresAt.After(expired) {
			expired = second.ExpiresAt
		}
		for _, test := range []struct {
			name    string
			at      time.Time
			joining string
			want    int
		}{
			{"unassigned active leases", second.AcquiredAt, "", 2},
			{"ordinary lease is not a landing group", second.AcquiredAt, "machine_1/prj_1/batch/", 2},
			{"exact expiry boundary", expired, "", 0},
		} {
			t.Run(string(org)+"/"+test.name, func(t *testing.T) {
				err := repository.ReadSprite(t.Context(), org, corestore.Serving, func(r postgres.SpriteReader) error {
					got, err := r.SpriteLeaseUsage(t.Context(), test.at, test.joining)
					if err != nil {
						return err
					}
					if got != test.want {
						t.Fatalf("usage=%d want=%d", got, test.want)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
