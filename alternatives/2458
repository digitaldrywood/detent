package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBillingEventOwnersAdvancePastCompletedPage(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	org := tracker.OrganizationID("org_billing_events")
	seedTenantAccess(t, repository, org)
	pendingID := "event_pending_after_page"
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(writer tenantstore.Writer) error {
		for index := range corestore.MaxPageSize {
			_, err := writer.CreateBillingEvent(t.Context(), billingstore.Event{
				OrganizationID:  org,
				ID:              fmt.Sprintf("event_done_%03d", index),
				Type:            "example",
				ReceivedAt:      time.Date(2026, 11, 1, 12, 0, index, 0, time.UTC),
				CreditProcessed: true,
			})
			if err != nil {
				return err
			}
		}
		_, err := writer.CreateBillingEvent(t.Context(), billingstore.Event{
			OrganizationID: org,
			ID:             pendingID,
			Type:           "example",
			ReceivedAt:     time.Date(2026, 11, 1, 12, 1, 0, 0, time.UTC),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var firstPage []billingstore.Event
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		firstPage, err = reader.BillingEvents(t.Context(), 0, int64(^uint64(0)>>1), corestore.MaxPageSize)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(firstPage) != corestore.MaxPageSize || !firstPage[0].CreditProcessed {
		t.Fatalf("historical event page = %d rows, first credit-processed=%v", len(firstPage), len(firstPage) > 0 && firstPage[0].CreditProcessed)
	}
	var pending []billingstore.Event
	var unprocessed []billingstore.Event
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		pending, err = reader.PendingBillingEvents(t.Context(), corestore.MaxPageSize)
		if err != nil {
			return err
		}
		unprocessed, err = reader.UnprocessedBillingEvents(t.Context(), corestore.MaxPageSize)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != pendingID {
		t.Fatalf("credit-pending page = %+v", pending)
	}
	if len(unprocessed) != corestore.MaxPageSize {
		t.Fatalf("reconcile page = %d rows, want %d", len(unprocessed), corestore.MaxPageSize)
	}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(writer tenantstore.Writer) error {
		for _, event := range unprocessed {
			if err := writer.SetBillingEventProcessed(t.Context(), event.ID, time.Date(2026, 11, 1, 12, 2, 0, 0, time.UTC)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
		next, err := reader.UnprocessedBillingEvents(t.Context(), corestore.MaxPageSize)
		if err != nil {
			return err
		}
		if len(next) != 1 || next[0].ID != pendingID {
			return fmt.Errorf("next reconcile page = %+v", next)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
