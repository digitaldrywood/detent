package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type tenantMutation struct {
	event   eventstore.Event
	receipt eventstore.Receipt
}

func prepareTenantMutation(t *testing.T, organization tracker.OrganizationID, key string) tenantMutation {
	t.Helper()
	content := eventstore.NewContent(newHistoryObjects())
	event := historyEvent(t, content, organization, "event_"+key)
	receipt, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{
		OrganizationID: organization, ActorID: "example", Operation: "update_item", CommandKey: key,
		RequestHash: artifact.Digest([]byte(string(organization) + key)), CreatedAt: historyAt(),
	}, []byte(`{"updated":true}`))
	if err != nil {
		t.Fatal(err)
	}
	return tenantMutation{event: event, receipt: receipt}
}

func (m tenantMutation) write(ctx context.Context, w tenantstore.Writer, after func(string) error) error {
	item, err := w.WorkItem(ctx, m.event.ProjectID, m.event.WorkItemID)
	if err != nil {
		return err
	}
	item.Title = "Updated atomically"
	if _, err := w.UpdateWorkItem(ctx, item); err != nil {
		return err
	}
	if err := after("item"); err != nil {
		return err
	}
	if _, err := w.AppendEvent(ctx, m.event); err != nil {
		return err
	}
	if err := after("event"); err != nil {
		return err
	}
	if _, err := w.PutReceipt(ctx, m.receipt); err != nil {
		return err
	}
	return after("receipt")
}

func (m tenantMutation) check(t *testing.T, r *postgres.Repository, committed bool) {
	t.Helper()
	err := r.ReadTenant(t.Context(), m.event.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
		item, err := reader.WorkItem(t.Context(), m.event.ProjectID, m.event.WorkItemID)
		if err != nil {
			return err
		}
		revision, sequence, title := int64(1), int64(0), "Example title"
		if committed {
			revision, sequence, title = 2, 1, "Updated atomically"
		}
		if item.OrganizationID != m.event.OrganizationID || item.Revision != revision || item.EventSequence != sequence || item.Title != title {
			t.Errorf("item after commit=%v: %+v", committed, item)
		}
		events, err := reader.Events(t.Context(), m.event.ProjectID, m.event.WorkItemID, 0, 2)
		if err != nil {
			return err
		}
		if len(events) != int(sequence) || (committed && (events[0].ID != m.event.ID || events[0].OrganizationID != m.event.OrganizationID)) {
			t.Errorf("events after commit=%v: %+v", committed, events)
		}
		receipt, err := reader.Receipt(t.Context(), m.receipt.ActorID, m.receipt.Operation, m.receipt.CommandKey)
		if !committed {
			if !errors.Is(err, corestore.ErrNotFound) {
				t.Errorf("rolled back receipt = %+v, error=%v", receipt, err)
			}
			return nil
		}
		if err == nil && (receipt.OrganizationID != m.receipt.OrganizationID || receipt.RequestHash != m.receipt.RequestHash) {
			t.Errorf("receipt = %+v", receipt)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTenantTransactionClaimAndCompletionAtomicity(t *testing.T) {
	for _, phase := range []string{"claim", "completion"} {
		for _, stop := range []string{"item", "event", "receipt", "cancellation", "canceled failure", "stale fence", "commit"} {
			t.Run(phase+"/"+stop, func(t *testing.T) {
				r, _ := repositoryFor(t, template.Database(t), smallPool())
				seedRepository(t, r, "org_first")
				m := prepareTenantMutation(t, "org_first", "command")
				var claim leasestore.Claim
				if err := r.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
					item, err := w.WorkItem(t.Context(), "prj_first", "wi_first_1")
					if err != nil {
						return err
					}
					claim = leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "machine", SessionID: "session", LeaseID: "lease", TTL: time.Hour}
					return w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_first", ID: "machine", Hostname: "example", Capacity: 1})
				}); err != nil {
					t.Fatal(err)
				}
				var lease leasestore.Lease
				if phase == "completion" {
					lease = claimLease(t, r, "org_first", claim)
					recordLeaseAttempt(t, r, "org_first", attemptEvent(lease))
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				injected := errors.New("injected command failure")
				err := r.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
					if phase == "claim" {
						var err error
						lease, err = w.Claim(ctx, claim)
						if err != nil {
							return err
						}
					}
					event := attemptEvent(lease)
					if stop == "stale fence" {
						event.Fence.FencingToken++
					}
					if phase == "completion" {
						event.Sequence, event.Status = 2, "succeeded"
					}
					if _, err := w.RecordAttempt(ctx, event); err != nil {
						return err
					}
					if phase == "completion" {
						if _, err := w.Release(ctx, leaseFence(lease), "completed"); err != nil {
							return err
						}
					}
					return m.write(ctx, w, func(stage string) error {
						if stop == stage {
							return injected
						}
						if stop == "cancellation" && stage == "receipt" {
							cancel()
						}
						if stop == "canceled failure" && stage == "receipt" {
							cancel()
							return injected
						}
						return nil
					})
				})
				committed := stop == "commit"
				want := injected
				if stop == "cancellation" {
					want = context.Canceled
				} else if stop == "stale fence" {
					want = tracker.ErrStaleFencingToken
				} else if committed {
					want = nil
				}
				if !errors.Is(err, want) {
					t.Fatalf("transaction error = %v, want %v", err, want)
				}
				m.check(t, r, committed)
				if err := r.ReadTenant(t.Context(), "org_first", corestore.Serving, func(reader tenantstore.Reader) error {
					stored, leaseErr := reader.Lease(t.Context(), claim.LeaseID)
					attempt, attemptErr := reader.Attempt(t.Context(), attemptEvent(lease).ID)
					usage, err := reader.Usage(t.Context(), claim.MachineID)
					if err != nil {
						return err
					}
					if phase == "claim" && !committed {
						if !errors.Is(leaseErr, corestore.ErrNotFound) || !errors.Is(attemptErr, corestore.ErrNotFound) || usage != 0 {
							t.Errorf("partial claim: lease=%v attempt=%v usage=%d", leaseErr, attemptErr, usage)
						}
						return nil
					}
					if leaseErr != nil || attemptErr != nil {
						return errors.Join(leaseErr, attemptErr)
					}
					sequence, status, used := int64(1), "running", int64(1)
					if phase == "completion" && committed {
						sequence, status, used = 2, "succeeded", 0
					}
					if attempt.Sequence != sequence || attempt.Status != status || usage != used || (stored.ReleasedAt != nil) != (used == 0) || stored.FencingToken != lease.FencingToken {
						t.Errorf("partial completion: lease=%+v attempt=%+v usage=%d", stored, attempt, usage)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestTenantTransactionIsolation(t *testing.T) {
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, r, org)
		m := prepareTenantMutation(t, org, "shared_command")
		if err := r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return m.write(t.Context(), w, func(string) error { return nil })
		}); err != nil {
			t.Fatal(err)
		}
		m.check(t, r, true)
	}
	foreign := prepareTenantMutation(t, "org_second", "foreign_command")
	for _, family := range []string{"core", "event", "lease"} {
		t.Run(family, func(t *testing.T) {
			err := r.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
				item, err := w.WorkItem(t.Context(), "prj_first", "wi_first_1")
				if err != nil {
					return err
				}
				item.Title = "Must roll back"
				if _, err := w.UpdateWorkItem(t.Context(), item); err != nil {
					return err
				}
				switch family {
				case "core":
					item.OrganizationID = "org_second"
					_, err = w.UpdateWorkItem(t.Context(), item)
				case "event":
					_, err = w.AppendEvent(t.Context(), foreign.event)
				case "lease":
					err = w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_second", ID: "foreign"})
				}
				return err
			})
			if !errors.Is(err, corestore.ErrTenantMismatch) {
				t.Fatalf("foreign write = %v", err)
			}
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				prepareTenantMutation(t, org, "shared_command").check(t, r, true)
			}
		})
	}
}

func TestTenantTransactionPrimaryFailure(t *testing.T) {
	for _, code := range []string{"40001", "40P01"} {
		for _, stage := range []string{"statement", "commit"} {
			t.Run(code+"/"+stage, func(t *testing.T) {
				admin := template.Database(t)
				r, _ := repositoryFor(t, admin, smallPool())
				seedRepository(t, r, "org_first")
				m := prepareTenantMutation(t, "org_first", "rejected")
				if _, err := admin.ExecContext(t.Context(), fmt.Sprintf(`CREATE FUNCTION reject_tenant_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure' USING ERRCODE='%s'; END $$`, code)); err != nil {
					t.Fatal(err)
				}
				trigger := `CREATE TRIGGER reject_receipt BEFORE INSERT ON native_commands FOR EACH ROW EXECUTE FUNCTION reject_tenant_receipt()`
				if stage == "commit" {
					trigger = `CREATE CONSTRAINT TRIGGER reject_receipt AFTER INSERT ON native_commands DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_tenant_receipt()`
				}
				if _, err := admin.ExecContext(t.Context(), trigger); err != nil {
					t.Fatal(err)
				}
				calls := 0
				err := r.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
					calls++
					return m.write(t.Context(), w, func(string) error { return nil })
				})
				if !errors.Is(err, corestore.ErrSerializationFailure) || calls != 1 {
					t.Fatalf("primary error=%v calls=%d", err, calls)
				}
				requireSQLState(t, err, code)
				m.check(t, r, false)
			})
		}
	}
}
