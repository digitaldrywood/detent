package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresNativeRetentionRepairsProjectionsWithoutLosingHistory(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	admin.SetMaxOpenConns(2)
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	old := now.Add(-90 * 24 * time.Hour)
	objects := &memoryRunDetails{}
	content := eventstore.NewContent(objects)
	const project tracker.ProjectID = "example"
	const item tracker.NativeWorkItemID = "wi_closed"
	const pending = corestore.MaxPageSize + 1
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	versions := make(map[tracker.OrganizationID]eventstore.CollaborationVersion)
	bodies := make(map[tracker.OrganizationID][]byte)
	encode := func(value any) []byte {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, org := range organizations {
		state, actorKind, actor := "Done", "human", "principal_"+string(org)
		body := encode(tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: item, Number: 1, Revision: 1}, Title: string(org), Body: "Retained closed work", State: state, CreatedAt: old, UpdatedAt: old})
		version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: item, RecordID: string(item), Revision: 1}, body)
		if err != nil {
			t.Fatal(err)
		}
		versions[org], bodies[org] = version, body
		events := make([]eventstore.Event, 0, pending+1)
		for index := range pending + 1 {
			value := tracker.CollaborationEvent{OrganizationID: org, ProjectID: project, AggregateType: "issue", AggregateID: item, ID: fmt.Sprintf("event_%d", index), Type: "run.checkpointed", SchemaVersion: 1, Actor: tracker.Actor{Kind: actorKind, PrincipalID: actor}, RecordedAt: old}
			if index == 0 {
				value.Type = "workflow.transitioned"
				value.Data.FromState, value.Data.ToState = "Todo", state
			}
			row, err := content.PrepareEvent(t.Context(), org, encode(value))
			if err != nil {
				t.Fatal(err)
			}
			row.PushKnown = index == 0
			events = append(events, row)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: old}); err != nil {
				return err
			}
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: item, Number: 1, Title: string(org), WorkflowStateName: &state, ActorKind: &actorKind, ActorPrincipalID: &actor, CreatedAt: old, UpdatedAt: old}); err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
				return err
			}
			for _, event := range events {
				if _, err := w.AppendEvent(t.Context(), event); err != nil {
					return err
				}
			}
			for _, key := range []string{"expired", "live"} {
				created := old
				if key == "live" {
					created = now
				}
				receipt := receiptstore.Record{OrganizationID: org, ActorID: actor, Operation: "POST /example", CommandKey: key, RequestHash: strings.Repeat("a", 64), CreatedAt: created, PayloadObjectKey: version.PayloadObjectKey, PayloadSHA256: version.PayloadSHA256, PayloadBytes: version.PayloadBytes, PayloadContentBytes: version.PayloadContentBytes, ResponseReplayUntilNs: created.Add(receiptstore.ReplayRetention).UnixNano()}
				if _, err := w.PutReceipt(t.Context(), receipt); err != nil {
					return err
				}
				challenge := corestore.DuplicateChallenge{OrganizationID: org, ProjectID: project, Token: "dup_" + strings.Repeat("a", 32), PrincipalID: actor, RequestID: key, ContentHash: strings.Repeat("b", 64), PolicyHash: strings.Repeat("c", 64), CreatedAt: created, ExpiresAt: created.Add(time.Hour), Candidates: []corestore.DuplicateCandidate{{WorkItemID: item, Number: 1, Revision: 1}}}
				if key == "live" {
					challenge.Token = "dup_" + strings.Repeat("d", 32)
				}
				if err := w.(corestore.DuplicateChallengeWriter).CreateDuplicateChallenge(t.Context(), challenge); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		metric := metricsstore.Metric{At: old, Route: "GET /example", CallerKind: "browser", CallerID: actor, StatusClass: 2, Latency: metricsstore.Latency{Count: 1, Sum: 1, Max: 1, Histogram: [64]int64{1}}}
		if err := repository.MergeRequestMetrics(t.Context(), org, old, []metricsstore.Metric{metric}); err != nil {
			t.Fatal(err)
		}
	}
	for index, org := range organizations {
		t.Run(string(org), func(t *testing.T) {
			service := &Service{config: Config{TenantRepository: repository, AccessRepository: repository, Hosted: &HostedConfig{OrganizationID: string(org)}, Logger: discardLogger(), now: func() time.Time { return now }}, database: &database{postgres: repository}, requestMetrics: &tenantRequestMetrics{repository: repository, organization: org}}
			service.config.RunDetails = objects
			for pass := range 2 {
				if err := service.maintainNativeRetention(t.Context(), now); err != nil {
					t.Fatal(err)
				}
				var unresolved int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND NOT push_known`, org).Scan(&unresolved); err != nil {
					t.Fatal(err)
				}
				if want := max(0, pending-(pass+1)*corestore.MaxPageSize); unresolved != want {
					t.Fatalf("projection pass %d left %d records, want %d", pass, unresolved, want)
				}
			}
			if index == 0 {
				var untouched int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_command_states WHERE organization_id=$1 AND NOT response_expired`, organizations[1]).Scan(&untouched); err != nil || untouched != 2 {
					t.Fatalf("foreign tenant receipts changed: count=%d error=%v", untouched, err)
				}
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				stored, err := r.CollaborationVersion(t.Context(), string(item), 1)
				if err != nil {
					return err
				}
				raw, err := content.CollaborationVersion(t.Context(), stored)
				if err != nil || !bytes.Equal(raw, bodies[org]) {
					t.Fatalf("closed issue payload changed: error=%v", err)
				}
				events, err := r.Events(t.Context(), project, item, 0, corestore.MaxPageSize)
				if err != nil {
					return err
				}
				if len(events) != corestore.MaxPageSize || events[0].Type != "workflow.transitioned" {
					t.Fatalf("retained history page=%d", len(events))
				}
				if _, err := content.Event(t.Context(), events[0]); err != nil {
					return err
				}
				tail, err := r.Events(t.Context(), project, item, events[len(events)-1].Sequence, corestore.MaxPageSize)
				if err != nil {
					return err
				}
				if len(tail) != 2 || tail[1].Sequence != pending+1 {
					t.Fatalf("retained history tail=%+v", tail)
				}
				if _, err := content.Event(t.Context(), tail[1]); err != nil {
					return err
				}
				facts, err := r.(reportstore.Reader).ReadOverviewFacts(t.Context(), reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: old.Add(-time.Hour), Split: old.Add(-time.Minute), To: now, Limit: 10})
				if err != nil {
					return err
				}
				if len(facts) != 1 || facts[0].Title != string(org) || facts[0].CompletedAt == nil || !facts[0].CompletedAt.Equal(old) {
					t.Fatalf("closed report facts=%+v", facts)
				}
				object, err := r.(artifactstore.Reader).ArchiveObjectByReference(t.Context(), eventstore.Reference(stored.PayloadObjectKey, stored.PayloadSHA256, stored.PayloadBytes, stored.PayloadContentBytes))
				if err != nil || object.Reference.Key != versions[org].PayloadObjectKey {
					t.Fatalf("still-owned archive disappeared: error=%v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var expired bool
			var requestHash string
			var owners, challenges, minutes, days int
			if err := admin.QueryRowContext(t.Context(), `SELECT response_expired,request_hash FROM native_command_states WHERE organization_id=$1 AND command_key='expired'`, org).Scan(&expired, &requestHash); err != nil {
				t.Fatal(err)
			}
			if requestHash != strings.Repeat("a", 64) {
				t.Fatalf("expired receipt lost its idempotency hash: %q", requestHash)
			}
			if err := admin.QueryRowContext(t.Context(), `SELECT
 (SELECT count(*) FROM archive_object_owners WHERE organization_id=$1),
 (SELECT count(*) FROM native_duplicate_challenges WHERE organization_id=$1),
 (SELECT count(*) FROM request_minutes WHERE organization_id=$1),
 (SELECT sum(count) FROM request_days WHERE organization_id=$1)`, org).Scan(&owners, &challenges, &minutes, &days); err != nil {
				t.Fatal(err)
			}
			if !expired || owners != 1 || challenges != 1 || minutes != 0 || days != 1 {
				t.Fatalf("cleanup expired=%t owners=%d challenges=%d minutes=%d day_requests=%d", expired, owners, challenges, minutes, days)
			}
		})
	}

	t.Run("lease recovery during retention", func(t *testing.T) {
		type ownership struct {
			leases     map[string]leasestore.Lease
			attempts   map[string]leasestore.Attempt
			checkpoint attachment.ArchiveReference
		}
		owners := make(map[tracker.OrganizationID]ownership)
		for _, org := range organizations {
			checkpoint, err := content.PreparePayload(t.Context(), org, encode(nativeTestCheckpoint()))
			if err != nil {
				t.Fatal(err)
			}
			owner := ownership{leases: make(map[string]leasestore.Lease), attempts: make(map[string]leasestore.Attempt), checkpoint: checkpoint}
			for index, kind := range []string{"live", "orphan", "recoverable"} {
				var work corestore.WorkItem
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					var err error
					state := "Todo"
					work, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: tracker.NativeWorkItemID("wi_" + kind), Number: int64(index + 2), Title: kind, WorkflowStateName: &state})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
					machine := leasestore.Machine{OrganizationID: org, ID: "machine_" + kind, Hostname: "example", Capacity: 1, Version: "test"}
					if err := w.RegisterMachine(t.Context(), machine); err != nil {
						return err
					}
					lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: work.ID, ExpectedRevision: work.Revision, MachineID: machine.ID, SessionID: "session_" + kind, LeaseID: "lease_" + kind, TTL: time.Hour})
					if err != nil {
						return err
					}
					status := "running"
					if kind == "recoverable" {
						status = "interrupted"
					}
					body := encode(tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{AttemptID: "attempt_" + kind, RunID: "run_" + kind, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1}, Status: status, StartedAt: lease.AcquiredAt, UpdatedAt: lease.AcquiredAt, Checkpoint: nativeTestCheckpoint()})
					payload, err := content.PreparePayload(t.Context(), org, body)
					if err != nil {
						return err
					}
					fence := leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}
					attempt, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "attempt_" + kind, RunID: "run_" + kind, Fence: fence, Sequence: 1, Status: status, PayloadObjectKey: payload.Key, PayloadReference: payload, CheckpointObjectKey: &checkpoint.Key, CheckpointReference: checkpoint})
					if err != nil {
						return err
					}
					if kind == "recoverable" {
						lease, err = w.Release(t.Context(), fence, "service_restart")
					}
					owner.leases[kind], owner.attempts[kind] = lease, attempt
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET acquired_at=clock_timestamp()-interval '2 hours',renewed_at=clock_timestamp()-interval '1 hour',expires_at=clock_timestamp()-interval '1 second' WHERE organization_id=$1 AND lease_id=$2`, org, owner.leases["orphan"].LeaseID); err != nil {
				t.Fatal(err)
			}
			owners[org] = owner
		}
		for index, org := range organizations {
			t.Run(string(org), func(t *testing.T) {
				ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
				blocker, err := admin.BeginTx(ctx, nil)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				var workers sync.WaitGroup
				t.Cleanup(func() { cancel(); _ = blocker.Rollback(); workers.Wait() })
				var blockerPID int
				if err := blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
					t.Fatal(err)
				}
				if _, err := blocker.ExecContext(ctx, "LOCK TABLE native_duplicate_challenges IN SHARE MODE"); err != nil {
					t.Fatal(err)
				}
				service := &Service{config: Config{TenantRepository: repository, Hosted: &HostedConfig{OrganizationID: string(org)}}}
				retained := make(chan error, 1)
				workers.Go(func() { retained <- service.expireNativeRecords(ctx, now.Add(2*time.Hour)) })
				testenv.WaitUntil(t, testenv.WaitContext(t, ctx), func() error {
					select {
					case err := <-retained:
						t.Fatalf("retention returned before lock barrier: %v", err)
					default:
					}
					var waiting bool
					if err := admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, blockerPID).Scan(&waiting); err != nil {
						return err
					}
					if !waiting {
						return fmt.Errorf("retention has not reached challenge lock")
					}
					return nil
				})
				foreign := organizations[1-index]
				var foreignLease leasestore.Lease
				var foreignAttempt leasestore.Attempt
				if err := repository.ReadLeases(ctx, foreign, corestore.Serving, func(r leasestore.Reader) error {
					var err error
					foreignLease, err = r.Lease(ctx, owners[foreign].leases["orphan"].LeaseID)
					if err != nil {
						return err
					}
					foreignAttempt, err = r.Attempt(ctx, owners[foreign].attempts["orphan"].ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				owner := owners[org]
				heartbeatAt := time.Now().UTC().Truncate(time.Microsecond)
				progress := make(chan error, 1)
				workers.Go(func() {
					progress <- repository.WriteLeases(ctx, org, corestore.Serving, func(w leasestore.Writer) error {
						live, orphan := owner.leases["live"], owner.leases["orphan"]
						if err := w.HeartbeatMachine(ctx, leasestore.Machine{OrganizationID: org, ID: live.MachineID, Hostname: "example", Capacity: 1, Version: "test"}, heartbeatAt); err != nil {
							return err
						}
						if _, err := w.Renew(ctx, leasestore.Fence{LeaseID: live.LeaseID, FencingToken: live.FencingToken}, 2*time.Hour); err != nil {
							return err
						}
						_, err := w.Expire(ctx, leasestore.Fence{LeaseID: orphan.LeaseID, FencingToken: orphan.FencingToken})
						return err
					})
				})
				select {
				case err := <-progress:
					if err != nil {
						t.Fatalf("lease progress during retention: %v", err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				select {
				case err := <-retained:
					t.Fatalf("retention returned before blocker released: %v", err)
				default:
				}
				if err := blocker.Commit(); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-retained:
					if err != nil {
						t.Fatalf("retention after lease progress: %v", err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if err := repository.ReadLeases(ctx, org, corestore.Serving, func(r leasestore.Reader) error {
					for _, kind := range []string{"live", "orphan", "recoverable"} {
						lease, err := r.Lease(ctx, owner.leases[kind].LeaseID)
						if err != nil {
							return err
						}
						attempt, err := r.Attempt(ctx, owner.attempts[kind].ID)
						if err != nil {
							return err
						}
						switch kind {
						case "live":
							if lease.ReleasedAt != nil || !lease.ExpiresAt.After(owner.leases[kind].ExpiresAt) || attempt.Status != "running" {
								t.Fatalf("renewed live ownership changed: lease=%+v attempt=%+v", lease, attempt)
							}
						case "orphan":
							if lease.ReleasedAt == nil || lease.ReleaseReason != "expired" || attempt.Status != "cancelled" || attempt.Sequence != 2 {
								t.Fatalf("expired orphan was not recovered: lease=%+v attempt=%+v", lease, attempt)
							}
						case "recoverable":
							if !reflect.DeepEqual(lease, owner.leases[kind]) || !reflect.DeepEqual(attempt, owner.attempts[kind]) {
								t.Fatalf("recoverable history changed: lease=%+v attempt=%+v", lease, attempt)
							}
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := repository.ReadLeases(ctx, foreign, corestore.Serving, func(r leasestore.Reader) error {
					lease, err := r.Lease(ctx, foreignLease.LeaseID)
					if err != nil {
						return err
					}
					attempt, err := r.Attempt(ctx, foreignAttempt.ID)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(lease, foreignLease) || !reflect.DeepEqual(attempt, foreignAttempt) {
						t.Fatalf("foreign tenant ownership changed: lease=%+v attempt=%+v", lease, attempt)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				var heartbeat time.Time
				if err := admin.QueryRowContext(ctx, "SELECT last_heartbeat_at FROM machines WHERE organization_id=$1 AND id=$2", org, owner.leases["live"].MachineID).Scan(&heartbeat); err != nil || heartbeat.Before(heartbeatAt) {
					t.Fatalf("heartbeat did not persist: at=%v error=%v", heartbeat, err)
				}
				checkpoint, err := objects.ReadDetail(ctx, string(org), owner.checkpoint)
				if err != nil || !bytes.Equal(checkpoint, encode(nativeTestCheckpoint())) {
					t.Fatalf("recoverable checkpoint lost: error=%v", err)
				}
				if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
					history, err := r.Events(ctx, project, item, pending, corestore.MaxPageSize)
					if err != nil || len(history) != 1 || history[0].Sequence != pending+1 {
						t.Fatalf("authoritative historical tail changed: history=%+v error=%v", history, err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
}
