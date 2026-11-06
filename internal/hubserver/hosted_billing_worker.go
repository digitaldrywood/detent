package hubserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type hostedBillingWorker struct {
	service *Service
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
}

func (s *Service) startHostedBilling() {
	if s.config.Hosted == nil || s.hostedBillingConfig() == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.billing = &hostedBillingWorker{service: s, cancel: cancel, done: make(chan struct{})}
	go s.billing.run(ctx)
}

func (s *Service) stopHostedBilling() {
	if s.billing != nil {
		s.billing.cancel()
		<-s.billing.done
	}
}

func (w *hostedBillingWorker) run(ctx context.Context) {
	defer close(w.done)
	cfg := w.service.hostedBillingConfig()
	if cfg == nil {
		return
	}
	ticker := time.NewTicker(time.Duration(cfg.ReconcileSeconds) * time.Second)
	defer ticker.Stop()
	for {
		w.mu.Lock()
		err := w.reconcile(ctx)
		w.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			w.service.config.Logger.WarnContext(ctx, "hosted billing reconciliation unavailable; retaining bounded local access", billingErrorAttrs(err)...)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *hostedBillingWorker) reconcile(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	s := w.service
	var through int64
	var unprocessedEventIDs []string
	type pendingEvent struct{ id, kind string }
	var events []pendingEvent

	if err := s.database.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
		unprocessed, err := reader.UnprocessedBillingEvents(ctx, corestore.MaxPageSize)
		if err != nil {
			return err
		}
		for _, row := range unprocessed {
			through = row.Sequence
			unprocessedEventIDs = append(unprocessedEventIDs, row.ID)
		}
		pending, err := reader.PendingBillingEvents(ctx, corestore.MaxPageSize)
		if err != nil {
			return err
		}
		for _, row := range pending {
			if row.Sequence > through {
				through = row.Sequence
			}
			events = append(events, pendingEvent{id: row.ID, kind: row.Type})
		}
		return nil
	}); err != nil {
		return err
	}

	var creditErr error
	for _, event := range events {
		if err := w.creditEvent(ctx, event.id, event.kind); err != nil {
			creditErr = errors.Join(creditErr, err)
			continue
		}
		if s.database.billingRepository != nil {
			if err := s.database.writeBillingTenant(ctx, func(writer tenantstore.Writer) error { return writer.SetBillingEventCreditProcessed(ctx, event.id) }); err != nil {
				return err
			}
		} else {
			if _, err := s.database.db.ExecContext(ctx, "UPDATE hosted_billing_events SET credit_processed=1 WHERE event_id=?", event.id); err != nil {
				return err
			}
		}
	}
	creditErr = errors.Join(creditErr, w.autoFund(ctx))
	previous, err := s.database.readHostedBilling(ctx)
	if err != nil {
		return err
	}
	cfg := s.hostedBillingConfig()
	if cfg == nil {
		return nil
	}
	binding, err := s.database.hostedBillingBinding(ctx, cfg)
	if errors.Is(err, errHostedBillingUnbound) {
		return nil
	}
	if err != nil {
		return err
	}
	snapshot, err := cfg.Provider.Reconcile(ctx, binding)
	if err != nil {
		return err
	}
	now := s.config.now().UTC()
	return errors.Join(creditErr, s.database.commitHostedBilling(ctx, resolveHostedBilling(cfg, previous, snapshot, now), through, unprocessedEventIDs, now))
}

func (s *Service) hostedStripeWebhook(c echo.Context) error {
	cfg := s.hostedBillingConfig()
	if cfg == nil {
		return c.NoContent(http.StatusNotFound)
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, 1024*1024))
	if err != nil {
		return s.hostedJSONError(c, http.StatusBadRequest, "Billing webhook body could not be read within the size limit")
	}
	event, err := billing.VerifyModeEvent(body, c.Request().Header.Get("Stripe-Signature"), cfg.WebhookSecret, s.config.now(), cfg.mode() == billing.ModeLive)
	if err != nil {
		return s.hostedJSONError(c, http.StatusBadRequest, "Billing webhook signature, timestamp or mode is invalid")
	}
	if !event.Relevant() {
		return c.NoContent(http.StatusOK)
	}

	err = s.database.writeBillingTenant(c.Request().Context(), func(writer tenantstore.Writer) error {
		_, err := writer.CreateBillingEvent(c.Request().Context(), billingstore.Event{OrganizationID: s.database.hostedOrganization, ID: event.ID, Type: event.Type, ReceivedAt: s.config.now().UTC()})
		return err
	})
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	return c.NoContent(http.StatusOK)
}
