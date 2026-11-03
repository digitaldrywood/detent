package postgres_test

import (
	"errors"
	"math"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
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
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES($1,'collaboration_bytes',$2)`, row.org, row.value); err != nil {
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
