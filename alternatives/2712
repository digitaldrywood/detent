package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOrganizationModelUpdatesWaitForProjectModelValidation(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	org := tracker.OrganizationID("org_models_lock")
	seedTenantAccess(t, repository, org)
	locked, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	first := startRepositoryTask(t, func(ctx context.Context) error {
		return repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.RequireOrganizationModels(ctx, 0); err != nil {
				return err
			}
			close(locked)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})
	waitRepositorySignal(t, locked)
	started := make(chan struct{})
	policy := policyTestRevision(org, "prj_first", "models", time.Now())
	row := policystore.OrganizationModels{OrganizationID: org, Revision: 1, Payload: eventstore.Reference(policy.PayloadObjectKey, policy.PayloadSHA256, 1, 1)}
	second := startRepositoryTask(t, func(ctx context.Context) error {
		return repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			close(started)
			return w.SetOrganizationModels(ctx, row, 0)
		})
	})
	waitRepositorySignal(t, started)
	waitLeaseLock(t, admin, 1, second)
	releaseOnce.Do(func() { close(release) })
	if err := first.wait(t); err != nil {
		t.Fatal(err)
	}
	if err := second.wait(t); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.RequireOrganizationModels(t.Context(), 0) }); !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("stale inherited model validation = %v", err)
	}
}

func TestOrganizationModelsEnforceTenantRevisionAndRollback(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		policy := policyTestRevision(org, "prj_first", "models", time.Now())
		row := policystore.OrganizationModels{OrganizationID: org, Revision: 1, Payload: eventstore.Reference(policy.PayloadObjectKey, policy.PayloadSHA256, 1, 1)}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.SetOrganizationModels(t.Context(), row, 0)
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.SetOrganizationModels(t.Context(), row, 0)
		}); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("stale update error = %v", err)
		}
		rollback := errors.New("rollback")
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			next := row
			next.Revision++
			if err := w.SetOrganizationModels(t.Context(), next, 1); err != nil {
				return err
			}
			return rollback
		}); !errors.Is(err, rollback) {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			got, err := r.OrganizationModels(t.Context())
			if err == nil && (got.Revision != 1 || got.OrganizationID != org || got.Payload.Key != row.Payload.Key) {
				t.Errorf("committed selection = %+v", got)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, revision := range []int64{0, 1, 2} {
			err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.RequireOrganizationModels(t.Context(), revision)
			})
			if revision == 1 && err != nil || revision != 1 && !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("require revision %d: %v", revision, err)
			}
		}
	}
	if _, err := admin.ExecContext(t.Context(), `UPDATE organizations SET model_selection_bytes=NULL WHERE organization_id='org_first'`); err == nil {
		t.Fatal("partial payload metadata was accepted")
	}
}
