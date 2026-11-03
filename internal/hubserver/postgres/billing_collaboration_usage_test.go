package postgres_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCollaborationBytesAdjustsExistingTenantCounterAtomically(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second", "org_missing", "org_overflow", "org_negative"} {
		seedTenantAccess(t, repository, org)
	}
	for _, row := range []struct {
		org   string
		value int64
	}{{"org_first", 5}, {"org_second", 11}, {"org_overflow", math.MaxInt64}, {"org_negative", -1}} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES($1,'collaboration_bytes',$2) ON CONFLICT(organization_id,name) DO UPDATE SET value=excluded.value`, row.org, row.value); err != nil {
			t.Fatal(err)
		}
	}

	if err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		used, err := r.CollaborationBytes(t.Context())
		if err != nil {
			return err
		}
		if used != 5 {
			t.Fatalf("first tenant collaboration bytes=%d, want 5", used)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), "org_second", corestore.Serving, func(r tenantstore.Reader) error {
		used, err := r.CollaborationBytes(t.Context())
		if err != nil {
			return err
		}
		if used != 11 {
			t.Fatalf("second tenant collaboration bytes=%d, want 11", used)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		result, err := w.AdjustCollaborationBytes(t.Context(), 4, 10)
		if err != nil || !result.Applied || !result.Allowed || result.Used != 9 {
			return errors.Join(err, errors.New("unexpected admitted collaboration bytes result"))
		}
		result, err = w.AdjustCollaborationBytes(t.Context(), 2, 10)
		if err != nil || result.Applied || result.Allowed || result.Used != 9 {
			return errors.Join(err, errors.New("unexpected denied collaboration bytes result"))
		}
		result, err = w.AdjustCollaborationBytes(t.Context(), -4, 10)
		if err != nil || !result.Applied || !result.Allowed || result.Used != 5 {
			return errors.Join(err, errors.New("unexpected collaboration bytes decrement result"))
		}
		result, err = w.AdjustCollaborationBytes(t.Context(), 0, math.MaxInt64)
		if err != nil || result.Applied || !result.Allowed || result.Used != 5 {
			return errors.Join(err, errors.New("unexpected collaboration bytes no-op result"))
		}
		if _, err := w.AdjustCollaborationBytes(t.Context(), -6, math.MaxInt64); !errors.Is(err, billingstore.ErrInvalidCollaborationBytes) {
			return errors.Join(err, errors.New("collaboration bytes underflow was not rejected"))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("rollback collaboration byte adjustment")
	err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.AdjustCollaborationBytes(t.Context(), 2, 10); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("adjustment transaction error=%v", err)
	}
	if err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		used, err := r.CollaborationBytes(t.Context())
		if err != nil {
			return err
		}
		if used != 5 {
			t.Fatalf("rolled back collaboration bytes=%d, want 5", used)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := admin.ExecContext(t.Context(), `DELETE FROM hosted_usage_counters WHERE organization_id='org_missing' AND name='collaboration_bytes'`); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), "org_missing", corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.AdjustCollaborationBytes(t.Context(), 1, math.MaxInt64); !errors.Is(err, corestore.ErrNotFound) {
			return errors.Join(err, errors.New("missing collaboration bytes counter was not rejected"))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), "org_overflow", corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.AdjustCollaborationBytes(t.Context(), 1, math.MaxInt64); !errors.Is(err, billingstore.ErrInvalidCollaborationBytes) {
			return errors.Join(err, errors.New("collaboration bytes overflow was not rejected"))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), "org_negative", corestore.Serving, func(r tenantstore.Reader) error {
		if _, err := r.CollaborationBytes(t.Context()); !errors.Is(err, billingstore.ErrInvalidCollaborationBytes) {
			return errors.Join(err, errors.New("negative collaboration bytes counter was accepted"))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), "org_missing", corestore.Serving, func(r tenantstore.Reader) error {
		if _, err := r.CollaborationBytes(t.Context()); !errors.Is(err, corestore.ErrNotFound) {
			return errors.Join(err, errors.New("missing collaboration bytes counter was fabricated"))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNewOrganizationsOwnCollaborationCounters(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
	for _, org := range []tracker.OrganizationID{"org_new_first", "org_new_second"} {
		seedTenantAccess(t, repository, org)
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			used, err := w.CollaborationBytes(t.Context())
			if err != nil {
				return err
			}
			if used != 0 {
				t.Fatalf("new organization bytes=%d, want 0", used)
			}
			result, err := w.AdjustCollaborationBytes(t.Context(), 7, 7)
			if err != nil || !result.Allowed || result.Used != 7 {
				return errors.Join(err, errors.New("new organization counter is unusable"))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	locked, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	first := startRepositoryTask(t, func(ctx context.Context) error {
		return repository.WriteTenant(ctx, "org_new_first", corestore.Serving, func(w tenantstore.Writer) error {
			result, err := w.AdjustCollaborationBytes(ctx, 1, 8)
			if err != nil || !result.Allowed || result.Used != 8 {
				return errors.Join(err, errors.New("first concurrent allocation was not admitted"))
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
	second := startRepositoryTask(t, func(ctx context.Context) error {
		return repository.WriteTenant(ctx, "org_new_first", corestore.Serving, func(w tenantstore.Writer) error {
			result, err := w.AdjustCollaborationBytes(ctx, 1, 8)
			if err != nil || result.Allowed || result.Applied || result.Used != 8 {
				return errors.Join(err, errors.New("concurrent allocation bypassed quota"))
			}
			return nil
		})
	})
	waitLeaseLock(t, admin, 1, second)
	releaseOnce.Do(func() { close(release) })
	if err := first.wait(t); err != nil {
		t.Fatal(err)
	}
	if err := second.wait(t); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), "org_new_second", corestore.Serving, func(r tenantstore.Reader) error {
		used, err := r.CollaborationBytes(t.Context())
		if err != nil || used != 7 {
			return errors.Join(err, errors.New("concurrent allocation changed another organization"))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
