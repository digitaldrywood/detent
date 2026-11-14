package executionrecords_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalAttemptCorrelationOwnsFencedReceipt(t *testing.T) {
	t.Parallel()
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued=%t", queued), func(t *testing.T) {
			admin := pgstoretest.Template.Database(t)
			repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				pgstoretest.SeedLeaseFamily(t, repository, org)
				if err := repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
					_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ID: 3, ProjectID: "prj_1", NativeID: "wi_3", Number: 2, Title: "Other example"})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				for _, item := range []int64{1, 2, 3} {
					claim := pgstoretest.LeaseClaim(item)
					project := tracker.ProjectID("prj_1")
					if item == 2 {
						project = "prj_2"
					}
					if item == 3 {
						claim.MachineID = "machine_1"
					}
					lease := pgstoretest.ClaimLease(t, repository, org, claim)
					event := pgstoretest.AttemptEvent(lease)
					event.CheckpointObjectKey = new("example/source-checkpoint")
					event.CheckpointPreservesSource = new(true)
					beat := func(offset time.Duration) *time.Time {
						return new(lease.RenewedAt.Truncate(time.Microsecond).Add(offset))
					}
					var interfere func()
					write := func(events ...leasestore.AttemptEvent) (leasestore.Attempt, bool, error) {
						var got leasestore.Attempt
						var replay bool
						sequences := make([]int64, len(events))
						for i, event := range events {
							sequences[i] = event.Sequence
						}
						apply := func(w leasestore.Writer) error {
							for _, event := range events {
								var err error
								if got, replay, err = w.RecordAttemptEvent(t.Context(), event); err != nil {
									return err
								}
							}
							if interfere != nil {
								interfere()
							}
							return nil
						}
						var err error
						if queued {
							err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
								facts := w.(interface {
									WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
								})
								return facts.WithRunnerEventFacts(t.Context(), eventstore.RunCommand{Project: project, WorkItem: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", item)), LeaseID: lease.LeaseID, AttemptID: events[0].ID, Sequences: sequences}, func(w tenantstore.Writer) error { return apply(w) })
							})
						} else {
							err = repository.WriteLeases(t.Context(), org, corestore.Serving, apply)
						}
						return got, replay, err
					}
					if got, replay, err := write(event); err != nil || replay || got.LocalAttemptID != 0 || !got.LocalAttemptIDRecorded {
						t.Fatalf("unobserved start: %+v replay=%v err=%v", got, replay, err)
					}
					if got, _, err := write(event); err != nil || got.CheckpointPreservesSource == nil || !*got.CheckpointPreservesSource {
						t.Fatalf("source checkpoint insert: %+v err=%v", got, err)
					}
					event.Sequence, event.LocalAttemptID, event.PayloadObjectKey = 2, 42, "example/observed"
					event.CheckpointObjectKey = new("example/clean-checkpoint")
					event.CheckpointPreservesSource = new(false)
					event.RuntimeHeartbeatAt, event.RuntimeOrdered = beat(900*time.Nanosecond), true
					if got, replay, err := write(event); err != nil || replay || got.LocalAttemptID != 42 || !got.LocalAttemptIDRecorded || got.RuntimeHeartbeatAt == nil || !got.RuntimeHeartbeatAt.Truncate(time.Microsecond).Equal(*beat(0)) {
						t.Fatalf("accepted correlation: %+v replay=%v err=%v", got, replay, err)
					}
					if got, replay, err := write(event); err != nil || !replay || got.LocalAttemptID != 42 {
						t.Fatalf("replay: %+v replay=%v err=%v", got, replay, err)
					}
					if got, _, err := write(event); err != nil || got.CheckpointPreservesSource == nil || *got.CheckpointPreservesSource || got.CheckpointObjectKey == nil || *got.CheckpointObjectKey != "example/clean-checkpoint" {
						t.Fatalf("false checkpoint replacement: %+v err=%v", got, err)
					}
					replayFact := event
					replayFact.CheckpointPreservesSource = new(true)
					if got, replay, err := write(replayFact); err != nil || !replay || got.CheckpointPreservesSource == nil || *got.CheckpointPreservesSource {
						t.Fatalf("internal fact changed receipt identity or stored classification: %+v replay=%v err=%v", got, replay, err)
					}
					for _, test := range []struct {
						name   string
						change func(*leasestore.AttemptEvent)
					}{
						{"checkpoint key without fact", func(e *leasestore.AttemptEvent) { e.Sequence = 3; e.CheckpointPreservesSource = nil }},
						{"checkpoint fact without key", func(e *leasestore.AttemptEvent) { e.Sequence = 3; e.CheckpointObjectKey = nil }},
						{"changed replay", func(e *leasestore.AttemptEvent) { e.LocalAttemptID = 43 }},
						{"sequence gap", func(e *leasestore.AttemptEvent) { e.Sequence = 4; e.LocalAttemptID = 43 }},
						{"rebound identity", func(e *leasestore.AttemptEvent) { e.Sequence = 3; e.LocalAttemptID = 43 }},
						{"wrong fence", func(e *leasestore.AttemptEvent) { e.Sequence = 3; e.Fence.FencingToken++ }},
						{"negative identity", func(e *leasestore.AttemptEvent) { e.Sequence = 3; e.LocalAttemptID = -1 }},
						{"older heartbeat", func(e *leasestore.AttemptEvent) { e.Sequence = 3; e.RuntimeHeartbeatAt = beat(-100 * time.Nanosecond) }},
						{"missing heartbeat", func(e *leasestore.AttemptEvent) { e.Sequence = 3; e.RuntimeHeartbeatAt = nil }},
						{"interrupted older heartbeat", func(e *leasestore.AttemptEvent) {
							e.Sequence, e.Status, e.RuntimeHeartbeatAt = 3, "interrupted", beat(-100*time.Nanosecond)
						}},
						{"interrupted missing heartbeat", func(e *leasestore.AttemptEvent) { e.Sequence, e.Status, e.RuntimeHeartbeatAt = 3, "interrupted", nil }},
						{"usage correction leaves running without heartbeat", func(e *leasestore.AttemptEvent) {
							e.Sequence, e.Status, e.UsageCorrection, e.RuntimeHeartbeatAt = 3, "failed", true, nil
						}},
					} {
						t.Run(fmt.Sprintf("%s/%d/%s", org, item, test.name), func(t *testing.T) {
							invalid := event
							test.change(&invalid)
							if _, _, err := write(invalid); !errors.Is(err, leasestore.ErrAttemptConflict) && !errors.Is(err, tracker.ErrStaleFencingToken) {
								t.Fatalf("invalid observation accepted: %v", err)
							}
						})
					}
					if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET local_attempt_id=NULL,local_attempt_id_recorded=false WHERE organization_id=$1 AND id=$2`, org, event.ID); err != nil {
						t.Fatal(err)
					}
					event.Sequence, event.PayloadObjectKey, event.RuntimeHeartbeatAt = 3, "example/unknown-continuation", beat(100*time.Nanosecond)
					if got, _, err := write(event); err != nil || got.LocalAttemptID != 0 || got.LocalAttemptIDRecorded {
						t.Fatalf("later observation promoted unknown coverage: %+v err=%v", got, err)
					}
					if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET local_attempt_id=42,local_attempt_id_recorded=true WHERE organization_id=$1 AND id=$2`, org, event.ID); err != nil {
						t.Fatal(err)
					}
					step := func(sequence int64, status string, heartbeat *time.Time, ordered bool) leasestore.AttemptEvent {
						next := event
						next.Sequence, next.Status, next.RuntimeHeartbeatAt, next.RuntimeOrdered, next.PayloadObjectKey = sequence, status, heartbeat, ordered, fmt.Sprintf("example/ordered-%d", sequence)
						return next
					}
					requireCurrent := func(name string, sequence int64, heartbeat *time.Time) {
						t.Helper()
						var receipts, tokens int
						if err := admin.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM native_attempt_events WHERE organization_id=$1 AND attempt_id=$2 AND sequence>$3),
 (SELECT count(*) FROM runner_attempt_token_usage WHERE organization_id=$1 AND attempt_id=$2)+(SELECT count(*) FROM runner_issue_tokens WHERE organization_id=$1 AND issue_id=$4)`, org, event.ID, sequence, item).Scan(&receipts, &tokens); err != nil {
							t.Fatal(err)
						}
						if err := repository.ReadLeases(t.Context(), org, corestore.Serving, func(r leasestore.Reader) error {
							got, err := r.Attempt(t.Context(), event.ID)
							if err == nil && (receipts != 0 || tokens != 0 || got.Sequence != sequence || got.RuntimeSummarySequence != sequence || got.RuntimeHeartbeatAt == nil || !got.RuntimeHeartbeatAt.Equal(*heartbeat)) {
								t.Fatalf("%s changed the attempt: receipts=%d tokens=%d attempt=%+v", name, receipts, tokens, got)
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					if got, _, err := write(step(4, "running", beat(time.Second+900*time.Nanosecond), true), step(5, "running", beat(time.Second+100*time.Nanosecond), true)); err != nil || got.Sequence != 5 {
						t.Fatalf("same microsecond batch: %+v err=%v", got, err)
					}
					metered := step(6, "running", beat(2*time.Second), true)
					metered.Usage = []tracker.NativeUsage{{Model: "example-model", Input: 10, Output: 1, To: *beat(0)}}
					if _, _, err := write(metered, step(7, "running", beat(time.Second-time.Microsecond), true)); !errors.Is(err, leasestore.ErrAttemptConflict) {
						t.Fatalf("older batch suffix: %v", err)
					}
					requireCurrent("older batch suffix", 5, beat(time.Second))
					if queued {
						interfere = func() {
							if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET runtime_heartbeat_at=$3 WHERE organization_id=$1 AND id=$2`, org, event.ID, *beat(time.Hour)); err != nil {
								t.Fatal(err)
							}
						}
						_, _, err := write(metered)
						interfere = nil
						if !errors.Is(err, leasestore.ErrAttemptConflict) || errors.Is(err, corestore.ErrNotFound) {
							t.Fatalf("queued advance refusal: %v", err)
						}
						requireCurrent("queued advance refusal", 5, beat(time.Hour))
						if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET runtime_heartbeat_at=$3 WHERE organization_id=$1 AND id=$2`, org, event.ID, *beat(time.Second)); err != nil {
							t.Fatal(err)
						}
					}
					if got, _, err := write(step(6, "interrupted", beat(2*time.Second), true)); err != nil || got.Status != "interrupted" {
						t.Fatalf("interrupted with heartbeat: %+v err=%v", got, err)
					}
					for name, heartbeat := range map[string]*time.Time{"older reconnect": beat(time.Second), "reconnect without heartbeat": nil} {
						if _, _, err := write(step(7, "running", heartbeat, true)); !errors.Is(err, leasestore.ErrAttemptConflict) {
							t.Fatalf("%s: %v", name, err)
						}
					}
					requireCurrent("refused reconnect", 6, beat(2*time.Second))
					if got, _, err := write(step(7, "running", beat(2*time.Second), true)); err != nil || got.Status != "running" {
						t.Fatalf("equal reconnect: %+v err=%v", got, err)
					}
					if got, _, err := write(step(8, "running", nil, false)); err != nil || got.RuntimeHeartbeatAt != nil || got.RuntimeSummarySequence != 8 {
						t.Fatalf("generic absent runtime: %+v err=%v", got, err)
					}
					metered = step(9, "running", beat(2*time.Second), true)
					metered.Usage = []tracker.NativeUsage{{Model: "example-model", Input: 10, Output: 1, To: *beat(0)}}
					var tokens int
					if _, _, err := write(metered); err != nil {
						t.Fatalf("heartbeat after cleared summary: %v", err)
					} else if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM runner_attempt_token_usage WHERE organization_id=$1 AND attempt_id=$2`, org, event.ID).Scan(&tokens); err != nil || tokens != 1 {
						t.Fatalf("accepted usage rows=%d err=%v", tokens, err)
					}
					event.Sequence, event.LocalAttemptID, event.Status, event.PayloadObjectKey, event.RuntimeHeartbeatAt = 10, 0, "failed", "example/finished", nil
					event.CheckpointObjectKey, event.CheckpointPreservesSource = nil, nil
					if got, _, err := write(event); err != nil || got.LocalAttemptID != 42 || got.RuntimeHeartbeatAt != nil || got.RuntimeSummarySequence != 10 {
						t.Fatalf("later omitted correlation: %+v err=%v", got, err)
					}
					if got, replay, err := write(event); err != nil || !replay || got.CheckpointPreservesSource == nil || *got.CheckpointPreservesSource || got.CheckpointObjectKey == nil || *got.CheckpointObjectKey != "example/clean-checkpoint" {
						t.Fatalf("checkpoint-free finish lost retained key/fact: %+v err=%v", got, err)
					}
					if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
						_, err := w.Release(t.Context(), pgstoretest.LeaseFence(lease), "finished")
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			repository, _ = attemptCountedRepository(t, repository, pool)
			if queued {
				columnAdded := false
				t.Cleanup(func() {
					if !columnAdded {
						return
					}
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
					defer cancel()
					if _, err := admin.ExecContext(cleanup, `ALTER TABLE native_attempts DROP COLUMN test_nullable_attempt_column`); err != nil {
						t.Error(err)
					}
				})
				if _, err := admin.ExecContext(t.Context(), `ALTER TABLE native_attempts ADD COLUMN test_nullable_attempt_column text`); err != nil {
					t.Fatal(err)
				}
				columnAdded = true
			}
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				for _, test := range []struct {
					project tracker.ProjectID
					item    tracker.NativeWorkItemID
					local   int64
					want    string
				}{
					{"prj_1", "wi_1", 42, "attempt_lease_1"},
					{"prj_2", "wi_2", 42, "attempt_lease_2"},
					{"prj_1", "wi_3", 42, "attempt_lease_3"},
					{"prj_1", "wi_2", 42, ""},
					{"prj_2", "wi_1", 42, ""},
					{"prj_1", "wi_1", 43, ""},
				} {
					if err := repository.ReadLeases(t.Context(), org, corestore.Serving, func(r leasestore.Reader) error {
						matches, err := r.LocalAttemptMatches(t.Context(), test.project, test.item, test.local)
						if err != nil {
							return err
						}
						if test.want == "" && len(matches) != 0 || test.want != "" && !slices.Equal(matches, []string{test.want}) {
							t.Fatalf("restart lookup %s/%s/%s/%d = %v", org, test.project, test.item, test.local, matches)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				claim := pgstoretest.LeaseClaim(1)
				claim.LeaseID, claim.SessionID = "lease_next", "session_next"
				lease := pgstoretest.ClaimLease(t, repository, org, claim)
				event := pgstoretest.AttemptEvent(lease)
				event.LocalAttemptID = 42
				if queued {
					event.CheckpointObjectKey = new("example/next-checkpoint")
					event.CheckpointPreservesSource = new(true)
					event.ArtifactsObjectKey = new("example/next-artifacts")
					event.RuntimeOrdered = true
					event.RuntimeHeartbeatAt = new(lease.RenewedAt.Truncate(time.Microsecond))
				}
				pgstoretest.RecordLeaseAttempt(t, repository, org, event)
				if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
					_, err := w.Release(t.Context(), pgstoretest.LeaseFence(lease), "finished")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				claim.LeaseID, claim.SessionID = "lease_third", "session_third"
				thirdLease := pgstoretest.ClaimLease(t, repository, org, claim)
				third := pgstoretest.AttemptEvent(thirdLease)
				third.LocalAttemptID = 42
				if queued {
					third.CheckpointObjectKey = new("example/third-checkpoint")
					third.CheckpointPreservesSource = new(false)
					third.ArtifactsObjectKey = new("example/third-artifacts")
					third.RuntimeOrdered = true
					third.RuntimeHeartbeatAt = new(thirdLease.RenewedAt.Truncate(time.Microsecond))
				}
				pgstoretest.RecordLeaseAttempt(t, repository, org, third)
				if queued {
					var expected, expectedLatest leasestore.Attempt
					if err := repository.ReadLeases(t.Context(), org, corestore.Serving, func(r leasestore.Reader) error {
						var err error
						expected, err = r.Attempt(t.Context(), event.ID)
						if err != nil {
							return err
						}
						expectedLatest, err = r.LatestAttempt(t.Context(), 1)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if expected.ID != event.ID || expectedLatest.ID != third.ID || expected.ID == expectedLatest.ID || expected.LocalAttemptID != 42 || !expected.LocalAttemptIDRecorded || expected.CheckpointObjectKey == nil || *expected.CheckpointObjectKey != "example/next-checkpoint" || expected.ArtifactsObjectKey == nil || *expected.ArtifactsObjectKey != "example/next-artifacts" || expected.RuntimeHeartbeatAt == nil || expectedLatest.CheckpointObjectKey == nil || *expectedLatest.CheckpointObjectKey != "example/third-checkpoint" || expectedLatest.ArtifactsObjectKey == nil || *expectedLatest.ArtifactsObjectKey != "example/third-artifacts" || expectedLatest.RuntimeHeartbeatAt == nil {
						t.Fatalf("compatibility fixture %s: attempt=%+v latest=%+v", org, expected, expectedLatest)
					}
					if expected.CheckpointPreservesSource == nil || !*expected.CheckpointPreservesSource || expectedLatest.CheckpointPreservesSource == nil || *expectedLatest.CheckpointPreservesSource {
						t.Fatalf("extra-column source facts did not persist: older=%+v latest=%+v", expected, expectedLatest)
					}
					for _, attemptID := range []string{event.ID, ""} {
						if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
							facts := w.(interface {
								WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
							})
							return facts.WithRunnerEventFacts(t.Context(), eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: lease.LeaseID, AttemptID: attemptID}, func(w tenantstore.Writer) error {
								got, err := w.Attempt(t.Context(), event.ID)
								if err != nil {
									return err
								}
								latest, err := w.LatestAttempt(t.Context(), 1)
								if err != nil {
									return err
								}
								if !reflect.DeepEqual(got, expected) || !reflect.DeepEqual(latest, expectedLatest) {
									t.Fatalf("extra-column snapshot %s/%q: attempt=%+v latest=%+v", org, attemptID, got, latest)
								}
								return nil
							})
						}); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := repository.ReadLeases(t.Context(), org, corestore.Serving, func(r leasestore.Reader) error {
					matches, err := r.LocalAttemptMatches(t.Context(), "prj_1", "wi_1", 42)
					if err == nil && (len(matches) != 2 || matches[0] == matches[1] || !slices.Contains([]string{"attempt_lease_1", "attempt_lease_next", "attempt_lease_third"}, matches[0]) || !slices.Contains([]string{"attempt_lease_1", "attempt_lease_next", "attempt_lease_third"}, matches[1])) {
						t.Fatalf("ambiguity lost: %v", matches)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
