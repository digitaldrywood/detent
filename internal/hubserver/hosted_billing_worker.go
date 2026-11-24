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
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type hostedBillingWorker struct {
	service *Service
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
}

func (s *Service) startHostedBilling() {
	if s.config.Hosted == nil {
		return
	}
	if s.runtimeHost != nil {
		s.billing = &hostedBillingWorker{service: s}
		return
	}
	if s.hostedBillingConfig() == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.billing = &hostedBillingWorker{service: s, cancel: cancel, done: make(chan struct{})}
	go s.billing.run(ctx)
}

func (s *Service) stopHostedBilling() {
	if s.billing != nil && s.billing.cancel != nil {
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

func (s *Service) BillingReconcileSeconds(ctx context.Context) (int64, error) {
	if s.config.Hosted == nil {
		return 0, nil
	}
	plans := s.database.plansService()
	if err := plans.RefreshRuntime(ctx); err != nil {
		return 0, err
	}
	cfg := s.hostedBillingConfig()
	if cfg == nil {
		return 0, nil
	}
	_, err := plans.BillingBinding(ctx, cfg)
	if errors.Is(err, hostedplans.ErrBillingUnbound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return cfg.ReconcileSeconds, nil
}

func (s *Service) maintainHostedBilling(ctx context.Context, limit int) (int, error) {
	if s.billing == nil {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !s.billing.mu.TryLock() {
		return 0, nil
	}
	defer s.billing.mu.Unlock()
	seconds, err := s.BillingReconcileSeconds(ctx)
	if err != nil || seconds == 0 {
		return 0, err
	}
	if claim := s.config.ClaimBillingReconciliation; claim != nil {
		claimed, err := claim(ctx)
		if err != nil || !claimed {
			return 0, err
		}
	}
	return limit, s.billing.reconcilePage(ctx, (limit-2)/2)
}

func (w *hostedBillingWorker) reconcile(ctx context.Context) error {
	return w.reconcilePage(ctx, corestore.MaxPageSize)
}

func (w *hostedBillingWorker) reconcilePage(ctx context.Context, limit int) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	s := w.service
	if err := hubapi.FromFeatureError(s.database.plansService().RefreshRuntime(ctx)); err != nil {
		return err
	}
	var through int64
	var unprocessedEventIDs []string
	type pendingEvent struct{ id, kind string }
	var events []pendingEvent

	if err := hubapi.FromFeatureError(s.database.plansService().ReadBillingTenant(ctx, func(reader tenantstore.Reader) error {
		unprocessed, err := reader.UnprocessedBillingEvents(ctx, limit)
		if err != nil {
			return err
		}
		for _, row := range unprocessed {
			through = row.Sequence
			unprocessedEventIDs = append(unprocessedEventIDs, row.ID)
		}
		pending, err := reader.PendingBillingEvents(ctx, limit)
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
	})); err != nil {
		return err
	}

	var creditErr error
	for _, event := range events {
		if err := w.creditEvent(ctx, event.id, event.kind); err != nil {
			creditErr = errors.Join(creditErr, err)
			continue
		}
		{
			if err := hubapi.FromFeatureError(s.database.plansService().WriteBillingTenant(ctx, func(writer tenantstore.Writer) error { return writer.SetBillingEventCreditProcessed(ctx, event.id) })); err != nil {
				return err
			}
		}
	}
	creditErr = errors.Join(creditErr, w.autoFund(ctx))
	return errors.Join(creditErr, w.reconcileSubscription(ctx, through, unprocessedEventIDs))
}

func (w *hostedBillingWorker) reconcileSubscription(ctx context.Context, through int64, unprocessedEventIDs []string) error {
	s := w.service
	previous, err := hubapi.FeatureResult(s.database.plansService().ReadBilling(ctx))
	if err != nil {
		return err
	}
	cfg := s.hostedBillingConfig()
	if cfg == nil {
		return nil
	}
	binding, err := hubapi.FeatureResult(s.database.plansService().BillingBinding(ctx, cfg))
	if errors.Is(err, hostedplans.ErrBillingUnbound) {
		return nil
	}
	if err != nil {
		return err
	}
	snapshot, err := cfg.Provider.Reconcile(ctx, binding)
	if err != nil {
		return err
	}
	now := s.config.Clock().UTC()
	return hubapi.FromFeatureError(s.database.plansService().CommitBilling(ctx, hostedplans.ResolveBilling(cfg, previous, snapshot, now), through, unprocessedEventIDs, now))
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
	event, err := billing.VerifyModeEvent(body, c.Request().Header.Get("Stripe-Signature"), cfg.WebhookSecret, s.config.Clock(), cfg.CreditMode() == billing.ModeLive)
	if err != nil {
		return s.hostedJSONError(c, http.StatusBadRequest, "Billing webhook signature, timestamp or mode is invalid")
	}
	if !event.Relevant() {
		return c.NoContent(http.StatusOK)
	}

	err = hubapi.FromFeatureError(s.database.plansService().WriteBillingTenant(c.Request().Context(), func(writer tenantstore.Writer) error {
		_, err := writer.CreateBillingEvent(c.Request().Context(), billingstore.Event{OrganizationID: s.database.hostedOrganization, ID: event.ID, Type: event.Type, ReceivedAt: s.config.Clock().UTC()})
		return err
	}))
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	return c.NoContent(http.StatusOK)
}
