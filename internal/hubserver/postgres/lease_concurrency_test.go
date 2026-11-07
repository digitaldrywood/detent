package postgres_test

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLeaseCompetingLifecycleTransactions(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
		expired             bool
		want                error
		status              string
		usage               int64
	}{
		{"renew before claim", "renew", "claim", false, tracker.ErrLeaseConflict, "running", 1},
		{"renew before expiry", "renew", "expire", false, tracker.ErrStaleFencingToken, "running", 1},
		{"renew before completion", "renew", "finish", false, nil, "succeeded", 1},
		{"completion before renewal", "finish", "renew", false, nil, "succeeded", 1},
		{"release before renewal", "release", "renew", false, tracker.ErrStaleFencingToken, "cancelled", 0},
		{"release before completion", "release", "finish", false, tracker.ErrStaleFencingToken, "cancelled", 0},
		{"expiry before completion", "expire", "finish", true, tracker.ErrStaleFencingToken, "cancelled", 0},
		{"expiry before claim", "expire", "claim", true, nil, "cancelled", 1},
		{"takeover before renewal", "claim", "renew", true, tracker.ErrStaleFencingToken, "cancelled", 1},
		{"takeover before completion", "claim", "finish", true, tracker.ErrStaleFencingToken, "cancelled", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admin := template.Database(t)
			r, _ := repositoryFor(t, admin, smallPool())
			seedLeaseFamily(t, r, "org_first")
			lease := claimLease(t, r, "org_first", leaseClaim(1))
			event := attemptEvent(lease)
			recordLeaseAttempt(t, r, "org_first", event)
			if tc.expired {
				expireLeaseFixture(t, admin, "org_first", lease.LeaseID)
			}
			operate := func(ctx context.Context, w leasestore.Writer, operation string) error {
				switch operation {
				case "claim":
					request := leaseClaim(1)
					request.LeaseID, request.SessionID = "replacement", "replacement"
					got, err := w.Claim(ctx, request)
					if err == nil && got.FencingToken <= lease.FencingToken {
						t.Error("takeover did not advance the fence")
					}
					return err
				case "renew":
					_, err := w.Renew(ctx, leaseFence(lease), time.Hour)
					return err
				case "expire":
					_, err := w.Expire(ctx, leaseFence(lease))
					return err
				case "release":
					_, err := w.Release(ctx, leaseFence(lease), "cancelled")
					return err
				default:
					finish := event
					finish.Sequence, finish.Status = 2, "succeeded"
					_, err := w.RecordAttempt(ctx, finish)
					return err
				}
			}
			var second *repositoryTask
			err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error {
				if err := operate(t.Context(), w, tc.first); err != nil {
					return err
				}
				second = startRepositoryTask(t, func(ctx context.Context) error {
					return r.WriteLeases(ctx, "org_first", corestore.Background, func(other leasestore.Writer) error {
						return operate(ctx, other, tc.second)
					})
				})
				waitLeaseLock(t, admin, 1, second)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := second.wait(t); !errors.Is(err, tc.want) {
				t.Fatalf("competing %s error = %v, want %v", tc.second, err, tc.want)
			}
			if err := r.ReadLeases(t.Context(), "org_first", corestore.Serving, func(reader leasestore.Reader) error {
				got, err := reader.Attempt(t.Context(), event.ID)
				if err == nil && got.Status != tc.status {
					t.Errorf("attempt status = %s, want %s", got.Status, tc.status)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			requireLeaseUsage(t, r, "org_first", lease.MachineID, tc.usage)
		})
	}
}

func TestLeaseUnrelatedClaimsDoNotShareWriterLock(t *testing.T) {
	for _, sameTenant := range []bool{false, true} {
		t.Run(map[bool]string{false: "other tenant", true: "same tenant other item"}[sameTenant], func(t *testing.T) {
			r, _ := repositoryFor(t, template.Database(t), smallPool())
			seedLeaseFamily(t, r, "org_first")
			otherOrg, otherID := tracker.OrganizationID("org_second"), int64(1)
			if sameTenant {
				otherOrg, otherID = "org_first", 2
			} else {
				seedLeaseFamily(t, r, otherOrg)
			}
			err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error {
				if _, err := w.Claim(t.Context(), leaseClaim(1)); err != nil {
					return err
				}
				other := startRepositoryTask(t, func(ctx context.Context) error {
					return r.WriteLeases(ctx, otherOrg, corestore.Background, func(w leasestore.Writer) error {
						_, err := w.Claim(ctx, leaseClaim(otherID))
						return err
					})
				})
				return other.wait(t)
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLeaseHeartbeatDoesNotRegressWhileWaiting(t *testing.T) {
	for _, operation := range []string{"claim", "renew", "release"} {
		t.Run(operation, func(t *testing.T) {
			admin := template.Database(t)
			r, _ := repositoryFor(t, admin, smallPool())
			seedLeaseFamily(t, r, "org_first")
			var fence leasestore.Fence
			if operation != "claim" {
				fence = leaseFence(claimLease(t, r, "org_first", leaseClaim(1)))
			}
			admin.SetMaxOpenConns(2)
			blocker, err := admin.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			var newer time.Time
			if err := blocker.QueryRowContext(t.Context(), `UPDATE machines SET last_heartbeat_at=clock_timestamp()+interval '1 hour' WHERE organization_id='org_first' AND id='machine_1' RETURNING last_heartbeat_at`).Scan(&newer); err != nil {
				t.Fatal(err)
			}
			task := startRepositoryTask(t, func(ctx context.Context) error {
				return r.WriteLeases(ctx, "org_first", corestore.Serving, func(w leasestore.Writer) error {
					switch operation {
					case "claim":
						_, err := w.Claim(ctx, leaseClaim(1))
						return err
					case "renew":
						_, err := w.Renew(ctx, fence, time.Hour)
						return err
					default:
						_, err := w.Release(ctx, fence, "finished")
						return err
					}
				})
			})
			waitLeaseLock(t, admin, 1, task)
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := task.wait(t); err != nil {
				t.Fatal(err)
			}
			var got time.Time
			if err := admin.QueryRowContext(t.Context(), `SELECT last_heartbeat_at FROM machines WHERE organization_id='org_first' AND id='machine_1'`).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if !got.Equal(newer) {
				t.Fatalf("waiting %s regressed heartbeat: got %v, want %v", operation, got, newer)
			}
		})
	}
}

func TestLeaseMutationsUseOneStatement(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	counter := &reportCountingTracer{}
	traced := tracedFeaturePool(t, pool, counter)
	config := traced.Config()
	var exchanges atomic.Int64
	dial := config.ConnConfig.DialFunc
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &leaseWireConn{Conn: conn, exchanges: &exchanges, startup: true}, nil
	}
	measured, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(measured.Close)
	counted, err := postgres.NewRepository(t.Context(), measured, smallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := counted.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, repository, org)
		historySize := 2
		if org == "org_second" {
			historySize = 50
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at,release_reason)
        SELECT $1,'history_'||n,2,'machine_2','history_'||n,1,now()-interval '2 hours',now()-interval '2 hours',now()-interval '1 hour',now()-interval '1 hour','expired' FROM generate_series(1,$2::integer) n`, org, historySize); err != nil {
			t.Fatal(err)
		}
		for _, operation := range []string{"renew", "release", "expire", "revision"} {
			t.Run(string(org)+"/"+operation, func(t *testing.T) {
				request := leaseClaim(1)
				request.LeaseID, request.SessionID = operation, operation
				lease := claimLease(t, repository, org, request)
				if operation != "revision" {
					recordLeaseAttempt(t, repository, org, attemptEvent(lease))
				}
				if operation == "expire" {
					expireLeaseFixture(t, admin, org, lease.LeaseID)
				}
				counter.count.Store(0)
				exchanges.Store(0)
				err := counted.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
					switch operation {
					case "renew":
						_, err := w.Renew(t.Context(), leaseFence(lease), time.Hour)
						return err
					case "revision":
						return w.AdvanceLeaseWorkItemRevision(t.Context(), leaseFence(lease), lease.IssueID, 1, 2)
					case "release":
						_, err := w.Release(t.Context(), leaseFence(lease), "released")
						return err
					default:
						_, err := w.Expire(t.Context(), leaseFence(lease))
						return err
					}
				})
				if err != nil {
					t.Fatal(err)
				}
				if got := exchanges.Load(); got != 3 {
					t.Errorf("wire exchanges = %d, want 3 including BEGIN/bind/COMMIT", got)
				}
				t.Logf("statements=%d exchanges=%d", counter.count.Load()+2, exchanges.Load())
				if got := counter.count.Load(); got != 2 {
					t.Errorf("statements including tenant binding = %d, want 2", got)
				}
				if operation == "revision" {
					if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
						for _, mismatch := range []struct {
							fence                 leasestore.Fence
							issue, previous, next int64
						}{
							{leaseFence(lease), lease.IssueID, 3, 4},
							{leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken + 1}, lease.IssueID, 2, 3},
							{leaseFence(lease), 2, 2, 3},
						} {
							if err := w.AdvanceLeaseWorkItemRevision(t.Context(), mismatch.fence, mismatch.issue, mismatch.previous, mismatch.next); err != nil {
								return err
							}
						}
						if err := w.AdvanceLeaseWorkItemRevision(t.Context(), leaseFence(lease), lease.IssueID, 2, 4); !errors.Is(err, tracker.ErrInvalidLeaseRequest) {
							t.Errorf("revision jump=%v", err)
						}
						current, err := w.Lease(t.Context(), lease.LeaseID)
						if err == nil && (current.WorkItemRevision != 2 || current.FencingToken != lease.FencingToken) {
							t.Errorf("revision advancement lost fence: %+v", current)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
					lease.WorkItemRevision = 2
					recordLeaseAttempt(t, repository, org, attemptEvent(lease))
					if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
						if err := w.AdvanceLeaseWorkItemRevision(t.Context(), leaseFence(lease), lease.IssueID, 2, 3); err != nil {
							return err
						}
						current, err := w.Lease(t.Context(), lease.LeaseID)
						if err == nil && current.WorkItemRevision != 2 {
							t.Errorf("started attempt revision changed: %+v", current)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				if operation == "renew" || operation == "revision" {
					if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
						_, err := w.Release(t.Context(), leaseFence(lease), "released")
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

type leaseWireConn struct {
	net.Conn
	exchanges *atomic.Int64
	pending   []byte
	startup   bool
}

func (c *leaseWireConn) Write(value []byte) (int, error) {
	n, err := c.Conn.Write(value)
	c.pending = append(c.pending, value[:n]...)
	for {
		if c.startup {
			if len(c.pending) < 4 {
				break
			}
			size := int(binary.BigEndian.Uint32(c.pending[:4]))
			if len(c.pending) < size {
				break
			}
			c.pending = c.pending[size:]
			c.startup = false
			continue
		}
		if len(c.pending) < 5 {
			break
		}
		size := 1 + int(binary.BigEndian.Uint32(c.pending[1:5]))
		if len(c.pending) < size {
			break
		}
		if c.pending[0] == 'S' || c.pending[0] == 'Q' {
			c.exchanges.Add(1)
		}
		c.pending = c.pending[size:]
	}
	return n, err
}
