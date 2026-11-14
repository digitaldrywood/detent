package executionrecords_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptIdentityProjectionMergesFencedEvents(t *testing.T) {
	t.Parallel()
	observed := time.Date(2026, 10, 8, 12, 0, 0, 123456000, time.UTC)
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued=%t", queued), func(t *testing.T) {
			t.Parallel()
			admin := pgstoretest.Template.Database(t)
			repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
			organizations := []tracker.OrganizationID{"org_first", "org_second"}
			for _, org := range organizations {
				pgstoretest.SeedLeaseFamily(t, repository, org)
			}
			type attemptRun struct {
				event    leasestore.AttemptEvent
				expected leasestore.AttemptIdentity
				sequence int64
			}
			runs := map[tracker.OrganizationID]*attemptRun{}
			write := func(org tracker.OrganizationID, event leasestore.AttemptEvent) (leasestore.Attempt, bool, error) {
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
						return facts.WithRunnerEventFacts(t.Context(), eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: event.Fence.LeaseID, AttemptID: event.ID, Sequences: []int64{event.Sequence}}, func(w tenantstore.Writer) error { return apply(w) })
					})
				} else {
					err = repository.WriteLeases(t.Context(), org, corestore.Serving, apply)
				}
				return got, replay, err
			}
			stored := func(t *testing.T, org tracker.OrganizationID) leasestore.Attempt {
				t.Helper()
				var got leasestore.Attempt
				if err := repository.ReadLeases(t.Context(), org, corestore.Serving, func(r leasestore.Reader) error {
					var err error
					got, err = r.Attempt(t.Context(), runs[org].event.ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return got
			}
			requireIdentity := func(t *testing.T, org tracker.OrganizationID, label string, got leasestore.Attempt) {
				t.Helper()
				run := runs[org]
				if got.IdentitySequence != run.sequence || !reflect.DeepEqual(got.Identity, run.expected) {
					t.Fatalf("%s %s identity sequence=%d identity=%+v, want sequence=%d identity=%+v", org, label, got.IdentitySequence, got.Identity, run.sequence, run.expected)
				}
			}
			advance := func(t *testing.T, org tracker.OrganizationID, identity leasestore.AttemptIdentity) {
				t.Helper()
				run := runs[org]
				run.event.Sequence++
				run.event.PayloadObjectKey = fmt.Sprintf("example/%s/%d", org, run.event.Sequence)
				run.event.Identity = identity
				got, replay, err := write(org, run.event)
				if err != nil || replay {
					t.Fatalf("%s sequence %d: replay=%v err=%v", org, run.event.Sequence, replay, err)
				}
				run.expected, run.sequence = run.expected.Merge(identity), run.event.Sequence
				requireIdentity(t, org, "returned", got)
				requireIdentity(t, org, "stored", stored(t, org))
			}
			for _, org := range organizations {
				lease := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(1))
				event := pgstoretest.AttemptEvent(lease)
				event.PayloadObjectKey = fmt.Sprintf("example/%s/1", org)
				configured := agentidentity.Configured("codex", "codex", "default", "implement", "requested-"+string(org), "openai", "medium", "flex", observed)
				configured.Selection = agentidentity.Selection{Policy: "project", PolicySource: "workflow", Reason: "default route", Level: "standard", RequestedModel: "requested-" + string(org), ModelSource: "policy", EffortSource: "policy", BackendSource: "policy", RouteSource: "policy"}
				event.Identity = leasestore.AttemptIdentity{Execution: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "requested-" + string(org)}, Runtime: configured}
				runs[org] = &attemptRun{event: event, expected: leasestore.AttemptIdentity{}.Merge(event.Identity), sequence: 1}
				got, replay, err := write(org, event)
				if err != nil || replay {
					t.Fatalf("%s start: replay=%v err=%v", org, replay, err)
				}
				requireIdentity(t, org, "returned start", got)
			}
			for _, org := range organizations {
				requireIdentity(t, org, "stored start", stored(t, org))
				if runs[org].expected.Runtime.ObservedAt == nil || !runs[org].expected.Runtime.ObservedAt.Equal(observed) {
					t.Fatalf("expected identity lost its observation time: %+v", runs[org].expected.Runtime)
				}
			}
			later := observed.Add(time.Minute)
			for _, test := range []struct {
				name     string
				identity leasestore.AttemptIdentity
				check    func(leasestore.AttemptIdentity) error
			}{
				{"partial runtime update keeps configured values", leasestore.AttemptIdentity{Runtime: agentidentity.RuntimeUpdate("resolved-model", "", "high", "", later)}, func(identity leasestore.AttemptIdentity) error {
					if identity.Runtime.ResolvedModel.Value != "resolved-model" || identity.Runtime.ReasoningEffort != agentidentity.NewValue("high", agentidentity.ProvenanceRuntime) || identity.Runtime.Provider != agentidentity.NewValue("openai", agentidentity.ProvenanceConfigured) || identity.Runtime.Selection.Reason != "default route" {
						return fmt.Errorf("partial update lost known values: %+v", identity.Runtime)
					}
					return nil
				}},
				{"event without identity erases nothing", leasestore.AttemptIdentity{}, func(identity leasestore.AttemptIdentity) error {
					if identity.Execution == nil || identity.Runtime.ResolvedModel.Value != "resolved-model" || identity.Runtime.ReasoningEffort.Value != "high" {
						return fmt.Errorf("identity was erased: %+v", identity)
					}
					return nil
				}},
				{"explicit unknown replaces only that value", leasestore.AttemptIdentity{Runtime: agentidentity.Identity{ServiceTier: agentidentity.UnknownValue()}}, func(identity leasestore.AttemptIdentity) error {
					if identity.Runtime.ServiceTier != agentidentity.UnknownValue() || identity.Runtime.ReasoningEffort.Value != "high" {
						return fmt.Errorf("unknown value merge: %+v", identity.Runtime)
					}
					return nil
				}},
				{"new selection replaces the old reasons", leasestore.AttemptIdentity{Runtime: agentidentity.Identity{Selection: agentidentity.Selection{Reason: "fallback", FallbackReason: "capacity"}}}, func(identity leasestore.AttemptIdentity) error {
					if identity.Runtime.Selection != (agentidentity.Selection{Reason: "fallback", FallbackReason: "capacity"}) {
						return fmt.Errorf("selection merge: %+v", identity.Runtime.Selection)
					}
					return nil
				}},
			} {
				t.Run(test.name, func(t *testing.T) {
					for _, org := range organizations {
						advance(t, org, test.identity)
						if err := test.check(runs[org].expected); err != nil {
							t.Fatal(err)
						}
					}
				})
			}
			t.Run("equal sequence replay", func(t *testing.T) {
				for _, org := range organizations {
					run := runs[org]
					for _, test := range []struct {
						name     string
						event    func() leasestore.AttemptEvent
						conflict bool
					}{
						{"exact replay", func() leasestore.AttemptEvent { return run.event }, false},
						{"same receipt with another derived identity", func() leasestore.AttemptEvent {
							event := run.event
							event.Identity = leasestore.AttemptIdentity{Runtime: agentidentity.RuntimeUpdate("other-model", "", "low", "", later)}
							return event
						}, false},
						{"changed content", func() leasestore.AttemptEvent {
							event := run.event
							event.PayloadObjectKey += "-changed"
							event.Identity = leasestore.AttemptIdentity{Runtime: agentidentity.RuntimeUpdate("other-model", "", "low", "", later)}
							return event
						}, true},
					} {
						_, replay, err := write(org, test.event())
						if test.conflict && !errors.Is(err, leasestore.ErrAttemptConflict) || !test.conflict && (err != nil || !replay) {
							t.Fatalf("%s %s: replay=%v err=%v", org, test.name, replay, err)
						}
						requireIdentity(t, org, test.name, stored(t, org))
					}
				}
			})
			t.Run("unprojected attempt stays unknown", func(t *testing.T) {
				for _, org := range organizations {
					if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET identity_sequence=NULL,identity_execution_role=NULL,identity_execution_backend=NULL,identity_execution_model=NULL,identity_execution_reasoning_effort=NULL WHERE organization_id=$1 AND id=$2`, org, runs[org].event.ID); err != nil {
						t.Fatal(err)
					}
					run := runs[org]
					run.event.Sequence++
					run.event.PayloadObjectKey = fmt.Sprintf("example/%s/%d", org, run.event.Sequence)
					run.event.Identity = leasestore.AttemptIdentity{Execution: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "later"}, Runtime: agentidentity.RuntimeUpdate("later", "", "low", "", later)}
					got, _, err := write(org, run.event)
					if err != nil || got.IdentitySequence != 0 || got.Identity.Execution != nil {
						t.Fatalf("%s legacy attempt became projected from a partial history: %+v err=%v", org, got, err)
					}
					if current := stored(t, org); current.IdentitySequence != 0 || current.Identity.Execution != nil {
						t.Fatalf("%s stored legacy attempt became projected: %+v", org, current)
					}
				}
			})
		})
	}
}
