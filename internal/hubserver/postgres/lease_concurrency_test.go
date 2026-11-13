package postgres_test

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
			admin := pgstoretest.Template.Database(t)
			r, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
			pgstoretest.SeedLeaseFamily(t, r, "org_first")
			lease := pgstoretest.ClaimLease(t, r, "org_first", pgstoretest.LeaseClaim(1))
			event := pgstoretest.AttemptEvent(lease)
			pgstoretest.RecordLeaseAttempt(t, r, "org_first", event)
			if tc.name == "expiry before completion" {
				other := event
				other.ID, other.RunID = "attempt_same_fence", "run_same_fence"
				err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error {
					_, err := w.RecordAttempt(t.Context(), other)
					return err
				})
				if !errors.Is(err, corestore.ErrUniqueViolation) {
					t.Fatalf("second start on the same fence: %v", err)
				}
			}
			var cancellation *leasestore.AttemptCancellation
			var content *eventstore.Content
			if tc.expired {
				expireLeaseFixture(t, admin, "org_first", lease.LeaseID)
				cancellation, content = prepareLeaseCancellationFixture(t, "org_first", lease, event)
			}
			operate := func(ctx context.Context, w leasestore.Writer, operation string) error {
				switch operation {
				case "claim":
					request := pgstoretest.LeaseClaim(1)
					request.LeaseID, request.SessionID = "replacement", "replacement"
					request.PreviousAttemptCancellation = cancellation
					got, err := w.Claim(ctx, request)
					if err == nil && got.FencingToken <= lease.FencingToken {
						t.Error("takeover did not advance the fence")
					}
					return err
				case "renew":
					_, err := w.Renew(ctx, pgstoretest.LeaseFence(lease), time.Hour)
					return err
				case "expire":
					_, err := w.ExpireWithCancellation(ctx, pgstoretest.LeaseFence(lease), cancellation)
					return err
				case "release":
					_, err := w.Release(ctx, pgstoretest.LeaseFence(lease), "cancelled")
					return err
				default:
					finish := event
					finish.Sequence, finish.Status = 2, "succeeded"
					_, err := w.RecordAttempt(ctx, finish)
					return err
				}
			}
			var second *pgstoretest.RepositoryTask
			err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error {
				if err := operate(t.Context(), w, tc.first); err != nil {
					return err
				}
				second = pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
					return r.WriteLeases(ctx, "org_first", corestore.Background, func(other leasestore.Writer) error {
						return operate(ctx, other, tc.second)
					})
				})
				pgstoretest.WaitLeaseLock(t, admin, 1, second)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := second.Wait(t); !errors.Is(err, tc.want) {
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
			var beforeReplay leasestore.Attempt
			if err := r.ReadLeases(t.Context(), "org_first", corestore.Serving, func(reader leasestore.Reader) error {
				var err error
				beforeReplay, err = reader.Attempt(t.Context(), event.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, queued := range []bool{false, true} {
				name := "ordinary receipt replay"
				if queued {
					name = "queued receipt replay"
				}
				t.Run(name, func(t *testing.T) {
					write := func(input leasestore.AttemptEvent) (leasestore.Attempt, bool, error) {
						t.Helper()
						var got leasestore.Attempt
						var duplicate bool
						err := r.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
							apply := func(writer tenantstore.Writer) error {
								var err error
								got, duplicate, err = writer.RecordAttemptEvent(t.Context(), input)
								return err
							}
							if !queued {
								return apply(w)
							}
							facts := w.(interface {
								WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
							})
							return facts.WithRunnerEventFacts(t.Context(), eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: lease.LeaseID, AttemptID: input.ID, Sequences: []int64{input.Sequence}}, apply)
						})
						return got, duplicate, err
					}
					got, duplicate, err := write(event)
					if err != nil || !duplicate || !reflect.DeepEqual(got, beforeReplay) {
						t.Fatalf("matching receipt changed attempt: got=%+v before=%+v duplicate=%t error=%v", got, beforeReplay, duplicate, err)
					}
					conflicting := event
					conflicting.PayloadObjectKey = "example/conflicting-replay"
					_, _, err = write(conflicting)
					want := leasestore.ErrAttemptConflict
					if tc.first == "release" || tc.first == "expire" || tc.first == "claim" {
						want = tracker.ErrStaleFencingToken
					}
					if !errors.Is(err, want) {
						t.Fatalf("conflicting receipt error=%v, want %v", err, want)
					}
					if err := r.ReadLeases(t.Context(), "org_first", corestore.Serving, func(reader leasestore.Reader) error {
						after, err := reader.Attempt(t.Context(), event.ID)
						if err == nil && !reflect.DeepEqual(after, beforeReplay) {
							t.Errorf("rejected receipt mutated attempt: got=%+v before=%+v", after, beforeReplay)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
			if cancellation != nil {
				if err := r.ReadTenant(t.Context(), "org_first", corestore.Serving, func(reader tenantstore.Reader) error {
					attempt, err := reader.Attempt(t.Context(), event.ID)
					if err != nil {
						return err
					}
					if attempt.Sequence != 2 || attempt.PayloadObjectKey != cancellation.Attempt.PayloadObjectKey || attempt.CleanupAcknowledgedAt != nil {
						t.Errorf("expired attempt did not retain fenced evidence: %+v", attempt)
					}
					history, err := reader.LatestRunEvent(t.Context(), "prj_1", event.ID)
					if err != nil {
						return err
					}
					finished, err := content.Event(t.Context(), history)
					if err != nil {
						return err
					}
					if finished.Data.Run == nil || finished.Data.Run.Outcome != "cancelled" || finished.Data.Run.TerminalFailure == nil || finished.Data.Run.TerminalFailure.ErrorClass != "lease_released" {
						t.Errorf("expired attempt history lost cancellation: %+v", finished.Data.Run)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			requireLeaseUsage(t, r, "org_first", lease.MachineID, tc.usage)
		})
	}
	t.Run("attempt insertion before expiry", func(t *testing.T) {
		admin := pgstoretest.Template.Database(t)
		pool := pgstoretest.RolePool(t, admin, "detent_runtime_v2", "")
		connection := pool.Config().ConnConfig
		pool.Close()
		var clock atomic.Pointer[time.Time]
		clock.Store(new(time.Now().UTC()))
		r, err := postgres.Open(t.Context(), postgres.Config{
			Connection: pgstoretest.Template.Connection(t, admin, connection.User, connection.Password), Pool: pgstoretest.SmallPool(),
			LeaseClock: func() time.Time { return *clock.Load() },
		}, os.DirFS("migrations"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := r.Close(); err != nil {
				t.Error(err)
			}
		})
		const org tracker.OrganizationID = "org_first"
		pgstoretest.SeedLeaseFamily(t, r, org)
		lease := pgstoretest.ClaimLease(t, r, org, pgstoretest.LeaseClaim(1))
		event := pgstoretest.AttemptEvent(lease)
		stale, _ := prepareLeaseCancellationFixture(t, org, lease, event)
		progress := event
		progress.Sequence++
		progress.PayloadObjectKey = "example/progress"
		var expiry *pgstoretest.RepositoryTask
		if err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
			if _, err := w.RecordAttempt(t.Context(), event); err != nil {
				return err
			}
			if _, err := w.RecordAttempt(t.Context(), progress); err != nil {
				return err
			}
			clock.Store(new(lease.ExpiresAt))
			expiry = pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
				return r.WriteLeases(ctx, org, corestore.Background, func(w leasestore.Writer) error {
					_, err := w.Expire(ctx, pgstoretest.LeaseFence(lease))
					return err
				})
			})
			pgstoretest.WaitLeaseLock(t, admin, 1, expiry)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := expiry.Wait(t); !errors.Is(err, corestore.ErrSerializationFailure) {
			t.Fatalf("expiry without inserted attempt evidence: %v", err)
		}
		err = r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
			_, err := w.ExpireWithCancellation(t.Context(), pgstoretest.LeaseFence(lease), stale)
			return err
		})
		if !errors.Is(err, corestore.ErrSerializationFailure) {
			t.Fatalf("expiry accepted a stale cancellation sequence: %v", err)
		}
		replacement := pgstoretest.LeaseClaim(1)
		replacement.LeaseID, replacement.SessionID = "replacement", "replacement"
		replacement.PreviousAttemptCancellation = stale
		err = r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
			_, err := w.Claim(t.Context(), replacement)
			return err
		})
		if !errors.Is(err, corestore.ErrSerializationFailure) {
			t.Fatalf("claim accepted a stale cancellation sequence: %v", err)
		}
		if err := r.ReadLeases(t.Context(), org, corestore.Serving, func(reader leasestore.Reader) error {
			current, err := reader.Lease(t.Context(), lease.LeaseID)
			if err != nil {
				return err
			}
			attempt, err := reader.Attempt(t.Context(), event.ID)
			if err == nil && (current.ReleasedAt != nil || attempt.Status != "running" || attempt.Sequence != progress.Sequence) {
				t.Errorf("expiry committed without attempt evidence: lease=%+v attempt=%+v", current, attempt)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := r.ReadLeases(t.Context(), org, corestore.Serving, func(reader leasestore.Reader) error {
			current, err := reader.LeaseLossCandidate(t.Context(), lease.IssueID, false)
			if err == nil && (current.ID != event.ID || current.Sequence != progress.Sequence) {
				t.Errorf("reprepared expiry read stale attempt: %+v", current)
			}
			progress.Sequence = current.Sequence
			return err
		}); err != nil {
			t.Fatal(err)
		}
		cancellation, _ := prepareLeaseCancellationFixture(t, org, lease, progress)
		if err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
			_, err := w.ExpireWithCancellation(t.Context(), pgstoretest.LeaseFence(lease), cancellation)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := r.ReadLeases(t.Context(), org, corestore.Serving, func(reader leasestore.Reader) error {
			current, err := reader.Attempt(t.Context(), event.ID)
			if err == nil && (current.Status != "cancelled" || current.Sequence != progress.Sequence+1 || current.PayloadObjectKey != cancellation.Attempt.PayloadObjectKey) {
				t.Errorf("retry failed to retain current attempt evidence: %+v", current)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLeaseUnrelatedClaimsDoNotShareWriterLock(t *testing.T) {
	for _, sameTenant := range []bool{false, true} {
		t.Run(map[bool]string{false: "other tenant", true: "same tenant other item"}[sameTenant], func(t *testing.T) {
			r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
			pgstoretest.SeedLeaseFamily(t, r, "org_first")
			otherOrg, otherID := tracker.OrganizationID("org_second"), int64(1)
			if sameTenant {
				otherOrg, otherID = "org_first", 2
			} else {
				pgstoretest.SeedLeaseFamily(t, r, otherOrg)
			}
			err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error {
				if _, err := w.Claim(t.Context(), pgstoretest.LeaseClaim(1)); err != nil {
					return err
				}
				other := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
					return r.WriteLeases(ctx, otherOrg, corestore.Background, func(w leasestore.Writer) error {
						_, err := w.Claim(ctx, pgstoretest.LeaseClaim(otherID))
						return err
					})
				})
				return other.Wait(t)
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
			admin := pgstoretest.Template.Database(t)
			r, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
			pgstoretest.SeedLeaseFamily(t, r, "org_first")
			var fence leasestore.Fence
			if operation != "claim" {
				fence = pgstoretest.LeaseFence(pgstoretest.ClaimLease(t, r, "org_first", pgstoretest.LeaseClaim(1)))
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
			task := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
				return r.WriteLeases(ctx, "org_first", corestore.Serving, func(w leasestore.Writer) error {
					switch operation {
					case "claim":
						_, err := w.Claim(ctx, pgstoretest.LeaseClaim(1))
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
			pgstoretest.WaitLeaseLock(t, admin, 1, task)
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := task.Wait(t); err != nil {
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

func TestLeaseMutationStatementBudget(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	counter := &pgstoretest.ReportCountingTracer{}
	traced := pgstoretest.TracedFeaturePool(t, pool, counter)
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
	counted, err := postgres.NewRepository(t.Context(), measured, pgstoretest.SmallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := counted.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, repository, org)
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
				request := pgstoretest.LeaseClaim(1)
				request.LeaseID, request.SessionID = operation, operation
				lease := pgstoretest.ClaimLease(t, repository, org, request)
				if operation != "revision" && operation != "expire" {
					pgstoretest.RecordLeaseAttempt(t, repository, org, pgstoretest.AttemptEvent(lease))
				}
				if operation == "expire" {
					expireLeaseFixture(t, admin, org, lease.LeaseID)
				}
				counter.Count.Store(0)
				exchanges.Store(0)
				err := counted.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
					switch operation {
					case "renew":
						_, err := w.Renew(t.Context(), pgstoretest.LeaseFence(lease), time.Hour)
						return err
					case "revision":
						return w.AdvanceLeaseWorkItemRevision(t.Context(), pgstoretest.LeaseFence(lease), lease.IssueID, 1, 2)
					case "release":
						_, err := w.Release(t.Context(), pgstoretest.LeaseFence(lease), "released")
						return err
					default:
						_, err := w.Expire(t.Context(), pgstoretest.LeaseFence(lease))
						return err
					}
				})
				if err != nil {
					t.Fatal(err)
				}
				wantExchanges, wantStatements := int64(3), int64(2)
				if operation == "expire" {
					wantExchanges++
					wantStatements++
				}
				if got := exchanges.Load(); got != wantExchanges {
					t.Errorf("wire exchanges = %d, want %d including BEGIN/bind/COMMIT", got, wantExchanges)
				}
				t.Logf("statements=%d exchanges=%d", counter.Count.Load()+2, exchanges.Load())
				if got := counter.Count.Load(); got != wantStatements {
					t.Errorf("statements including tenant binding = %d, want %d", got, wantStatements)
				}
				if operation == "revision" {
					if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
						for _, mismatch := range []struct {
							fence                 leasestore.Fence
							issue, previous, next int64
						}{
							{pgstoretest.LeaseFence(lease), lease.IssueID, 3, 4},
							{leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken + 1}, lease.IssueID, 2, 3},
							{pgstoretest.LeaseFence(lease), 2, 2, 3},
						} {
							if err := w.AdvanceLeaseWorkItemRevision(t.Context(), mismatch.fence, mismatch.issue, mismatch.previous, mismatch.next); err != nil {
								return err
							}
						}
						if err := w.AdvanceLeaseWorkItemRevision(t.Context(), pgstoretest.LeaseFence(lease), lease.IssueID, 2, 4); !errors.Is(err, tracker.ErrInvalidLeaseRequest) {
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
					pgstoretest.RecordLeaseAttempt(t, repository, org, pgstoretest.AttemptEvent(lease))
					if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
						if err := w.AdvanceLeaseWorkItemRevision(t.Context(), pgstoretest.LeaseFence(lease), lease.IssueID, 2, 3); err != nil {
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
						_, err := w.Release(t.Context(), pgstoretest.LeaseFence(lease), "released")
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
