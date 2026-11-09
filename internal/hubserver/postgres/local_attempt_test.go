package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalAttemptCorrelationOwnsFencedReceipt(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued=%t", queued), func(t *testing.T) {
			admin := template.Database(t)
			repository, pool := repositoryFor(t, admin, smallPool())
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				seedLeaseFamily(t, repository, org)
				if err := repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
					_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ID: 3, ProjectID: "prj_1", NativeID: "wi_3", Number: 2, Title: "Other example"})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				for _, item := range []int64{1, 2, 3} {
					claim := leaseClaim(item)
					project := tracker.ProjectID("prj_1")
					if item == 2 {
						project = "prj_2"
					}
					if item == 3 {
						claim.MachineID = "machine_1"
					}
					lease := claimLease(t, repository, org, claim)
					event := attemptEvent(lease)
					write := func(event leasestore.AttemptEvent) (leasestore.Attempt, bool, error) {
						var got leasestore.Attempt
						var replay bool
						apply := func(w leasestore.Writer) error {
							var err error
							got, replay, err = w.RecordAttemptEvent(t.Context(), event)
							return err
						}
						var err error
						if queued {
							err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
								facts := w.(interface {
									WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
								})
								return facts.WithRunnerEventFacts(t.Context(), eventstore.RunCommand{Project: project, WorkItem: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", item)), LeaseID: lease.LeaseID, AttemptID: event.ID, Sequences: []int64{event.Sequence}}, func(w tenantstore.Writer) error { return apply(w) })
							})
						} else {
							err = repository.WriteLeases(t.Context(), org, corestore.Serving, apply)
						}
						return got, replay, err
					}
					if got, replay, err := write(event); err != nil || replay || got.LocalAttemptID != 0 || !got.LocalAttemptIDRecorded {
						t.Fatalf("unobserved start: %+v replay=%v err=%v", got, replay, err)
					}
					event.Sequence, event.LocalAttemptID, event.PayloadObjectKey = 2, 42, "example/observed"
					if got, replay, err := write(event); err != nil || replay || got.LocalAttemptID != 42 || !got.LocalAttemptIDRecorded {
						t.Fatalf("accepted correlation: %+v replay=%v err=%v", got, replay, err)
					}
					if got, replay, err := write(event); err != nil || !replay || got.LocalAttemptID != 42 {
						t.Fatalf("replay: %+v replay=%v err=%v", got, replay, err)
					}
					for _, test := range []struct {
						name   string
						change func(*leasestore.AttemptEvent)
					}{
						{"changed replay", func(e *leasestore.AttemptEvent) { e.LocalAttemptID = 43 }},
						{"sequence gap", func(e *leasestore.AttemptEvent) { e.Sequence = 4; e.LocalAttemptID = 43 }},
						{"rebound identity", func(e *leasestore.AttemptEvent) { e.Sequence = 3; e.LocalAttemptID = 43 }},
						{"wrong fence", func(e *leasestore.AttemptEvent) { e.Sequence = 3; e.Fence.FencingToken++ }},
						{"negative identity", func(e *leasestore.AttemptEvent) { e.Sequence = 3; e.LocalAttemptID = -1 }},
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
					event.Sequence, event.PayloadObjectKey = 3, "example/unknown-continuation"
					if got, _, err := write(event); err != nil || got.LocalAttemptID != 0 || got.LocalAttemptIDRecorded {
						t.Fatalf("later observation promoted unknown coverage: %+v err=%v", got, err)
					}
					if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET local_attempt_id=42,local_attempt_id_recorded=true WHERE organization_id=$1 AND id=$2`, org, event.ID); err != nil {
						t.Fatal(err)
					}
					event.Sequence, event.LocalAttemptID, event.Status, event.PayloadObjectKey = 4, 0, "failed", "example/finished"
					if got, _, err := write(event); err != nil || got.LocalAttemptID != 42 {
						t.Fatalf("later omitted correlation: %+v err=%v", got, err)
					}
					if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
						_, err := w.Release(t.Context(), leaseFence(lease), "finished")
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			repository, _ = attemptCountedRepository(t, repository, pool)
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
				claim := leaseClaim(1)
				claim.LeaseID, claim.SessionID = "lease_next", "session_next"
				lease := claimLease(t, repository, org, claim)
				event := attemptEvent(lease)
				event.LocalAttemptID = 42
				recordLeaseAttempt(t, repository, org, event)
				if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
					_, err := w.Release(t.Context(), leaseFence(lease), "finished")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				claim.LeaseID, claim.SessionID = "lease_third", "session_third"
				third := attemptEvent(claimLease(t, repository, org, claim))
				third.LocalAttemptID = 42
				recordLeaseAttempt(t, repository, org, third)
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
