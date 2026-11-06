package hubserver

import (
	"context"
	"errors"
	"io"
	"math"
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

func (s *Service) MaintainBillingCredit(ctx context.Context, after int64) (int64, bool, error) {
	if after < 0 || s.database == nil || s.database.hostedOrganization == "" || s.database.billingRepository == nil {
		return after, false, ErrHostedDatabaseBinding
	}
	var events []billingstore.Event
	err := s.database.billingRepository.ReadTenant(ctx, s.database.hostedOrganization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		events, err = r.BillingEvents(ctx, after, math.MaxInt64, 1)
		return err
	})
	if err != nil {
		return after, false, err
	}
	if len(events) == 0 {
		return 0, false, nil
	}
	event := events[0]
	if event.CreditProcessed {
		return event.Sequence, true, nil
	}
	worker := &hostedBillingWorker{service: s}
	if err := worker.creditEvent(ctx, event.ID, event.Type); err != nil {
		return event.Sequence, true, err
	}
	err = s.database.billingRepository.WriteTenant(ctx, s.database.hostedOrganization, corestore.Background, func(w tenantstore.Writer) error {
		return w.SetBillingEventCreditProcessed(ctx, event.ID)
	})
	return event.Sequence, true, err
}

func (s *Service) MaintainBillingFunding(ctx context.Context) error {
	return (&hostedBillingWorker{service: s}).autoFund(ctx)
}

func (s *Service) MaintainBillingSubscription(ctx context.Context) error {
	cfg := s.hostedBillingConfig()
	if cfg == nil {
		return nil
	}
	var events []billingstore.Event
	var reconciled *time.Time
	if err := s.database.billingRepository.ReadTenant(ctx, s.database.hostedOrganization, corestore.Background, func(r tenantstore.Reader) error {
		account, err := r.BillingAccount(ctx)
		if err != nil {
			return err
		}
		reconciled = account.ReconciledAt
		events, err = r.UnprocessedBillingEvents(ctx, 1)
		return err
	}); err != nil {
		return err
	}
	if len(events) == 0 && reconciled != nil && s.config.now().Before(reconciled.Add(time.Duration(cfg.ReconcileSeconds)*time.Second)) {
		return nil
	}
	var ids []string
	var through int64
	if len(events) > 0 {
		ids, through = []string{events[0].ID}, events[0].Sequence
	}
	return (&hostedBillingWorker{service: s}).reconcileSubscription(ctx, through, ids)
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
	if err := s.database.refreshRuntime(ctx); err != nil {
		return err
	}
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
		{
			if err := s.database.writeBillingTenant(ctx, func(writer tenantstore.Writer) error { return writer.SetBillingEventCreditProcessed(ctx, event.id) }); err != nil {
				return err
			}
		}
	}
	creditErr = errors.Join(creditErr, w.autoFund(ctx))
	return errors.Join(creditErr, w.reconcileSubscription(ctx, through, unprocessedEventIDs))
}

func (w *hostedBillingWorker) reconcileSubscription(ctx context.Context, through int64, unprocessedEventIDs []string) error {
	s := w.service
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
	return s.database.commitHostedBilling(ctx, resolveHostedBilling(cfg, previous, snapshot, now), through, unprocessedEventIDs, now)
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
