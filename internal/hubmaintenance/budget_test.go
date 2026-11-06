package hubmaintenance

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
)

type detailCounter struct{ reads, writes int }

func (s *detailCounter) ReadDetail(context.Context, string, attachment.ArchiveReference) ([]byte, error) {
	s.reads++
	return []byte("example"), nil
}

func (s *detailCounter) StoreDetail(context.Context, string, []byte) (attachment.ArchiveReference, error) {
	s.writes++
	return attachment.ArchiveReference{}, nil
}

func TestPayloadBudgetRejectsBeforeIOAndCannotCrossTenant(t *testing.T) {
	tenant := Tenant{Organization: "org_first", Generation: 1}
	ctx := (Budget{Items: 1, Bytes: 20}).Context(t.Context(), tenant)
	storage := &detailCounter{}
	details := LimitDetails(storage)
	ref := attachment.ArchiveReference{CompressedSize: 3, UncompressedSize: 7}
	if _, err := details.ReadDetail(ctx, tenant.Organization, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := details.ReadDetail(ctx, tenant.Organization, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := details.ReadDetail(ctx, tenant.Organization, ref); !errors.Is(err, ErrByteBudget) {
		t.Fatalf("unbounded read: %v", err)
	}
	if _, err := details.StoreDetail(ctx, tenant.Organization, []byte("example")); !errors.Is(err, ErrByteBudget) {
		t.Fatalf("unbounded write: %v", err)
	}
	ctx = (Budget{Items: 1, Bytes: 20}).Context(t.Context(), tenant)
	if _, err := details.ReadDetail(ctx, "org_second", ref); err == nil {
		t.Fatal("cross-tenant maintenance read accepted")
	}
	if storage.reads != 2 || storage.writes != 0 {
		t.Fatalf("I/O after refusal: reads=%d writes=%d", storage.reads, storage.writes)
	}
}
