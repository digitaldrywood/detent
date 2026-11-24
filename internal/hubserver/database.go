package hubserver

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type database struct {
	policies               hubrecords.ProjectPolicyCache
	dashboardCommitted     chan struct{}
	linkedSourceBase       string
	postgres               *postgres.Repository
	billingRepository      tenantstore.Repository
	billingAuditStore      attachment.DetailStore
	detailObjects          *postgres.DetailStore
	schemaVersion          int64
	schemaFingerprint      string
	hostedOrganization     tracker.OrganizationID
	hostedRuntime          atomic.Pointer[hostedplans.RuntimeConfig]
	runtimeLoads           singleflight.Group
	runtimeBilling         *hostedplans.HostedBillingConfig
	logger                 *slog.Logger
	hostedBilling          bool
	aiCreditMode           string
	aiCreditCostMultiplier float64
	now                    func() time.Time
	newLeaseID             func() string
	closeOnce              sync.Once
	closeErr               error
}

func openDatabase(ctx context.Context, cfg Config) (*database, error) {
	return openRuntimeDatabase(ctx, cfg, nil, "")
}

func openRuntimeDatabase(ctx context.Context, cfg Config, host *HostedRuntime, org tracker.OrganizationID) (*database, error) {
	if err := cfg.ValidateServingBackend(host != nil || cfg.Postgres != nil); err != nil {
		return nil, err
	}
	var store *database
	var err error
	if host != nil {
		store, err = host.tenantDatabase(org, cfg.Hosted.SharedEntry.Generation)
	} else {
		store, err = openPostgresDatabase(ctx, *cfg.Postgres)
	}
	if err != nil {
		return nil, err
	}
	store.billingRepository = cfg.BillingRepository
	if store.billingRepository == nil {
		store.billingRepository = store.postgres
	}
	store.billingAuditStore = cfg.RunDetails
	return store, nil
}

func (d *database) health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if d.postgres == nil {
		return ErrDatabaseBackend
	}
	if err := d.postgres.Ping(ctx); err != nil {
		return fmt.Errorf("ping hub PostgreSQL: %w", err)
	}
	return nil
}

func (d *database) Close() error {
	if d == nil {
		return nil
	}
	d.closeOnce.Do(func() {
		if d.postgres != nil {
			d.closeErr = d.postgres.Close()
		}
	})
	return d.closeErr
}
