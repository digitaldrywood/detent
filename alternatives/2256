package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func seedLeaseFamily(t *testing.T, r *postgres.Repository, org tracker.OrganizationID) {
	t.Helper()
	err := r.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
		for id := int64(1); id <= 2; id++ {
			project := tracker.ProjectID(fmt.Sprintf("prj_%d", id))
			if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}); err != nil {
				return err
			}
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ID: id, ProjectID: project, NativeID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", id)), Number: 1, Title: "Example"}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
		for _, id := range []string{"machine_1", "machine_2"} {
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: id, Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func leaseClaim(id int64) leasestore.Claim {
	return leasestore.Claim{IssueID: id, ExpectedRevision: 1, MachineID: fmt.Sprintf("machine_%d", id), SessionID: fmt.Sprintf("session_%d", id), LeaseID: fmt.Sprintf("lease_%d", id), TTL: time.Hour}
}

func claimLease(t *testing.T, r *postgres.Repository, org tracker.OrganizationID, request leasestore.Claim) leasestore.Lease {
	t.Helper()
	var lease leasestore.Lease
	err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
		var err error
		lease, err = w.Claim(t.Context(), request)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func leaseFence(lease leasestore.Lease) leasestore.Fence {
	return leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}
}

func attemptEvent(lease leasestore.Lease) leasestore.AttemptEvent {
	return leasestore.AttemptEvent{ID: "attempt_" + lease.LeaseID, Fence: leaseFence(lease), RunID: "run_" + lease.LeaseID, Sequence: 1, Status: "running", PayloadObjectKey: "example/start", DispatchGeneration: 2}
}

func recordLeaseAttempt(t *testing.T, r *postgres.Repository, org tracker.OrganizationID, event leasestore.AttemptEvent) leasestore.Attempt {
	t.Helper()
	var attempt leasestore.Attempt
	err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
		var err error
		attempt, err = w.RecordAttempt(t.Context(), event)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}

func expireLeaseFixture(t *testing.T, admin *sql.DB, org tracker.OrganizationID, id string) {
	t.Helper()
	_, err := admin.ExecContext(t.Context(), `UPDATE leases SET acquired_at=clock_timestamp()-interval '3 hours',renewed_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE organization_id=$1 AND lease_id=$2`, org, id)
	if err != nil {
		t.Fatal(err)
	}
}

func requireLeaseUsage(t *testing.T, r *postgres.Repository, org tracker.OrganizationID, machine string, want int64) {
	t.Helper()
	err := r.ReadLeases(t.Context(), org, corestore.Serving, func(reader leasestore.Reader) error {
		got, err := reader.Usage(t.Context(), machine)
		if err == nil && got != want {
			t.Errorf("usage = %d, want %d", got, want)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLeaseAttemptLifecycle(t *testing.T) {
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, r, org)
		for _, id := range []int64{1, 2} {
			t.Run(fmt.Sprintf("%s/%d", org, id), func(t *testing.T) {
				request := leaseClaim(id)
				lease := claimLease(t, r, org, request)
				request.LeaseID = "unused_replay_id"
				replay := claimLease(t, r, org, request)
				if replay.LeaseID != lease.LeaseID || replay.FencingToken != lease.FencingToken || !replay.ExpiresAt.Equal(lease.ExpiresAt) || lease.WorkItemRevision != 1 {
					t.Fatalf("claim replay changed ownership: %+v, first %+v", replay, lease)
				}
				event := attemptEvent(lease)
				start := recordLeaseAttempt(t, r, org, event)
				if start.Sequence != 1 || start.Status != "running" || start.WorkItemRevision != lease.WorkItemRevision {
					t.Fatalf("start = %+v", start)
				}
				event.Sequence, event.PayloadObjectKey = 2, "example/checkpoint"
				event.CheckpointObjectKey, event.ArtifactsObjectKey = new("example/checkpoint"), new("example/artifacts")
				recordLeaseAttempt(t, r, org, event)
				event.Sequence, event.Status, event.PayloadObjectKey = 3, "succeeded", "example/finish"
				event.CheckpointObjectKey, event.ArtifactsObjectKey = nil, nil
				finished := recordLeaseAttempt(t, r, org, event)
				if finished.Status != "succeeded" || finished.Sequence != 3 || finished.CheckpointObjectKey == nil || *finished.CheckpointObjectKey != "example/checkpoint" || finished.ArtifactsObjectKey == nil || *finished.ArtifactsObjectKey != "example/artifacts" {
					t.Fatalf("finish lost checkpoint: %+v", finished)
				}
				requireLeaseUsage(t, r, org, lease.MachineID, 1)
				err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
					renewed, err := w.Renew(t.Context(), leaseFence(lease), 2*time.Hour)
					if err != nil {
						return err
					}
					if renewed.FencingToken != lease.FencingToken || !renewed.ExpiresAt.After(lease.ExpiresAt) {
						t.Errorf("renew changed owner or failed to extend expiry: %+v", renewed)
					}
					released, err := w.Release(t.Context(), leaseFence(lease), "finished")
					if err == nil && (released.ReleasedAt == nil || released.ReleaseReason != "finished") {
						t.Errorf("release = %+v", released)
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				recordLeaseAttempt(t, r, org, event)
				requireLeaseUsage(t, r, org, lease.MachineID, 0)
				request.LeaseID, request.SessionID = lease.LeaseID+"_next", lease.SessionID+"_next"
				next := claimLease(t, r, org, request)
				if next.FencingToken <= lease.FencingToken {
					t.Errorf("next fence = %d, previous %d", next.FencingToken, lease.FencingToken)
				}
				continued := attemptEvent(next)
				continued.RunID = event.RunID
				recordLeaseAttempt(t, r, org, continued)
			})
		}
	}
}

func TestLeaseRejectsStaleAndInvalidMutations(t *testing.T) {
	for _, state := range []string{"live", "expired", "released", "reassigned"} {
		t.Run(state, func(t *testing.T) {
			admin := template.Database(t)
			r, _ := repositoryFor(t, admin, smallPool())
			seedLeaseFamily(t, r, "org_first")
			lease := claimLease(t, r, "org_first", leaseClaim(1))
			event := attemptEvent(lease)
			recordLeaseAttempt(t, r, "org_first", event)
			if state == "expired" || state == "reassigned" {
				expireLeaseFixture(t, admin, "org_first", lease.LeaseID)
			}
			if state == "released" {
				if err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error {
					_, err := w.Release(t.Context(), leaseFence(lease), "cancelled")
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if state == "reassigned" {
				request := leaseClaim(1)
				request.LeaseID, request.SessionID = "replacement", "replacement"
				claimLease(t, r, "org_first", request)
			}
			for _, operation := range []string{"renew", "release", "finish", "wrong fence", "expire live"} {
				if state == "live" && operation != "wrong fence" && operation != "expire live" || state != "live" && operation == "expire live" {
					continue
				}
				t.Run(operation, func(t *testing.T) {
					err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error {
						fence := leaseFence(lease)
						switch operation {
						case "renew":
							_, err := w.Renew(t.Context(), fence, time.Hour)
							return err
						case "release":
							_, err := w.Release(t.Context(), fence, "stale")
							return err
						case "finish":
							finish := event
							finish.Sequence, finish.Status = 2, "succeeded"
							_, err := w.RecordAttempt(t.Context(), finish)
							return err
						case "wrong fence":
							fence.FencingToken++
							_, err := w.Renew(t.Context(), fence, time.Hour)
							return err
						default:
							_, err := w.Expire(t.Context(), fence)
							return err
						}
					})
					if !errors.Is(err, tracker.ErrStaleFencingToken) {
						t.Fatalf("%s error = %v", operation, err)
					}
				})
			}
			want := int64(0)
			if state == "live" || state == "reassigned" {
				want = 1
			}
			requireLeaseUsage(t, r, "org_first", lease.MachineID, want)
		})
	}
}

func TestLeaseAttemptRejectsConflictingSequences(t *testing.T) {
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	seedLeaseFamily(t, r, "org_first")
	lease := claimLease(t, r, "org_first", leaseClaim(1))
	event := attemptEvent(lease)
	recordLeaseAttempt(t, r, "org_first", event)
	for _, tc := range []struct {
		name string
		edit func(*leasestore.AttemptEvent)
	}{
		{"changed receipt", func(e *leasestore.AttemptEvent) { e.PayloadObjectKey = "example/different" }},
		{"skipped sequence", func(e *leasestore.AttemptEvent) { e.Sequence = 3 }},
		{"changed run", func(e *leasestore.AttemptEvent) { e.Sequence, e.RunID = 2, "different" }},
		{"changed generation", func(e *leasestore.AttemptEvent) { e.Sequence, e.DispatchGeneration = 2, 3 }},
		{"empty payload", func(e *leasestore.AttemptEvent) { e.PayloadObjectKey = "" }},
		{"invalid status", func(e *leasestore.AttemptEvent) { e.Status = "invalid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := event
			tc.edit(&changed)
			err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error { _, err := w.RecordAttempt(t.Context(), changed); return err })
			if !errors.Is(err, leasestore.ErrAttemptConflict) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if got := recordLeaseAttempt(t, r, "org_first", event); got.Sequence != 1 || got.PayloadObjectKey != event.PayloadObjectKey {
		t.Fatalf("rejections changed attempt: %+v", got)
	}
}

func waitLeaseLock(t *testing.T, admin *sql.DB, count int, tasks ...*repositoryTask) {
	t.Helper()
	testenv.WaitUntil(t, t.Context(), func() error {
		for _, task := range tasks {
			select {
			case <-task.done:
				t.Fatalf("transaction exited before the lock barrier: %v", task.err)
			default:
			}
		}
		var waiting int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&waiting); err != nil {
			return err
		}
		if waiting < count {
			return fmt.Errorf("waiting transactions = %d, want %d", waiting, count)
		}
		return nil
	})
}

func TestLeaseCompetingClaims(t *testing.T) {
	for _, sameSession := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_session_%t", sameSession), func(t *testing.T) {
			admin := template.Database(t)
			r, _ := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
			seedLeaseFamily(t, r, "org_first")
			admin.SetMaxOpenConns(2)
			blocker, err := admin.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			if _, err := blocker.ExecContext(t.Context(), `SELECT id FROM issues WHERE organization_id='org_first' AND id=1 FOR NO KEY UPDATE`); err != nil {
				t.Fatal(err)
			}
			var leases [2]leasestore.Lease
			var tasks []*repositoryTask
			for i, workload := range []corestore.Workload{corestore.Serving, corestore.Background} {
				tasks = append(tasks, startRepositoryTask(t, func(ctx context.Context) error {
					return r.WriteLeases(ctx, "org_first", workload, func(w leasestore.Writer) error {
						request := leaseClaim(1)
						if !sameSession {
							request.LeaseID, request.SessionID = fmt.Sprintf("lease_%d", i), fmt.Sprintf("session_%d", i)
						}
						var err error
						leases[i], err = w.Claim(ctx, request)
						return err
					})
				}))
			}
			waitLeaseLock(t, admin, 2, tasks...)
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			var winners, conflicts int
			for _, task := range tasks {
				err := task.wait(t)
				if err == nil {
					winners++
				} else if errors.Is(err, tracker.ErrLeaseConflict) {
					conflicts++
				} else {
					t.Fatal(err)
				}
			}
			if !sameSession && (winners != 1 || conflicts != 1) || sameSession && (winners != 2 || leases[0].FencingToken != leases[1].FencingToken) {
				t.Fatalf("winners=%d conflicts=%d leases=%+v", winners, conflicts, leases)
			}
			var active int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM leases WHERE released_at IS NULL`).Scan(&active); err != nil || active != 1 {
				t.Fatalf("active=%d error=%v", active, err)
			}
		})
	}
}

func TestLeaseClaimEligibility(t *testing.T) {
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	seedLeaseFamily(t, r, "org_first")
	for _, tc := range []struct {
		name string
		edit func(*leasestore.Claim)
		want error
	}{
		{"changed revision", func(c *leasestore.Claim) { c.ExpectedRevision++ }, corestore.ErrRevisionConflict},
		{"missing item", func(c *leasestore.Claim) { c.IssueID = 99 }, corestore.ErrNotFound},
		{"zero TTL", func(c *leasestore.Claim) { c.TTL = 0 }, tracker.ErrInvalidClaimRequest},
		{"submicrosecond TTL", func(c *leasestore.Claim) { c.TTL = time.Nanosecond }, tracker.ErrInvalidClaimRequest},
		{"empty session", func(c *leasestore.Claim) { c.SessionID = " " }, tracker.ErrInvalidClaimRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := leaseClaim(1)
			tc.edit(&request)
			err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error { _, err := w.Claim(t.Context(), request); return err })
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
		})
	}
	if err := r.Write(t.Context(), "org_first", corestore.Serving, func(w corestore.Writer) error {
		item, err := w.WorkItem(t.Context(), "prj_1", "wi_1")
		if err != nil {
			return err
		}
		item.Archived = true
		_, err = w.UpdateWorkItem(t.Context(), item)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request := leaseClaim(1)
	request.ExpectedRevision = 2
	err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error { _, err := w.Claim(t.Context(), request); return err })
	if !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("archived claim = %v", err)
	}
	if strings.Contains(fmt.Sprint(err), "Example") {
		t.Fatal("error contains customer content")
	}
}
