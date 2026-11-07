package hubmaintenance

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
)

type detailCounter struct{ reads, writes, canonicalReads int }

func (s *detailCounter) ReadDetail(context.Context, string, attachment.ArchiveReference) ([]byte, error) {
	s.reads++
	return []byte("example"), nil
}

func (s *detailCounter) StoreDetail(context.Context, string, []byte) (attachment.ArchiveReference, error) {
	s.writes++
	return attachment.ArchiveReference{}, nil
}

func (s *detailCounter) ReadCanonicalDetail(context.Context, string, string, string) ([]byte, error) {
	s.canonicalReads++
	return []byte("example"), nil
}

func TestPayloadBudgetRejectsBeforeIOAndCannotCrossTenant(t *testing.T) {
	tenant := Tenant{Organization: "org_first", Generation: 1}
	for _, test := range []struct {
		name  string
		items int
		valid bool
	}{
		{"single", 1, true},
		{"batch", 64, true},
		{"empty", 0, false},
		{"negative", -1, false},
		{"oversized", 65, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := (Budget{Items: test.items, Bytes: 20}).Context(t.Context(), tenant)
			storage := &detailCounter{}
			details := LimitDetails(storage)
			ref := attachment.ArchiveReference{CompressedSize: 3, UncompressedSize: 7}
			if !test.valid {
				if _, err := details.ReadDetail(ctx, tenant.Organization, ref); err == nil || storage.reads != 0 {
					t.Fatalf("invalid item budget reached storage: reads=%d err=%v", storage.reads, err)
				}
				return
			}
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
			ctx = (Budget{Items: test.items, Bytes: 20}).Context(t.Context(), tenant)
			if _, err := details.ReadDetail(ctx, "org_second", ref); err == nil {
				t.Fatal("cross-tenant maintenance read accepted")
			}
			if storage.reads != 2 || storage.writes != 0 {
				t.Fatalf("I/O after refusal: reads=%d writes=%d", storage.reads, storage.writes)
			}
		})
	}
	key, err := attachment.ArchiveKey(tenant.Organization, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := attachment.ArchiveKey("org_second", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		bytes        int64
		organization string
		key          string
		wantErr      bool
	}{
		{"request", 0, tenant.Organization, key, false},
		{"maintenance", 2 * attachment.MaxArchiveBytes, tenant.Organization, key, false},
		{"insufficient budget", 2*attachment.MaxArchiveBytes - 1, tenant.Organization, key, true},
		{"cross tenant budget", 2 * attachment.MaxArchiveBytes, "org_second", otherKey, true},
		{"cross tenant key", 0, "org_second", key, true},
		{"malformed key", 0, tenant.Organization, "malformed", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage := &detailCounter{}
			owner, ok := attachment.DetailStore(LimitDetails(storage)).(attachment.CanonicalDetailStore)
			if !ok {
				t.Fatal("limiter removed canonical recovery")
			}
			ctx := t.Context()
			if test.bytes > 0 {
				ctx = (Budget{Items: 1, Bytes: test.bytes}).Context(ctx, tenant)
			}
			raw, err := owner.ReadCanonicalDetail(ctx, test.organization, "prj_example", test.key)
			wantReads := 0
			if test.wantErr {
				if err == nil {
					t.Fatal("invalid canonical read accepted")
				}
			} else {
				wantReads = 1
				if err != nil || string(raw) != "example" {
					t.Fatalf("canonical recovery: %q %v", raw, err)
				}
				if test.bytes > 0 {
					if _, err := owner.ReadCanonicalDetail(ctx, test.organization, "prj_example", test.key); !errors.Is(err, ErrByteBudget) {
						t.Fatalf("exhausted canonical budget: %v", err)
					}
				}
			}
			if storage.canonicalReads != wantReads {
				t.Fatalf("canonical I/O calls = %d, want %d", storage.canonicalReads, wantReads)
			}
		})
	}
}

func TestDetailBatchPreservesOwnerAndBudget(t *testing.T) {
	organization := "org_example"
	key, err := attachment.ArchiveKey(organization, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	refs := []attachment.ArchiveReference{{Key: key, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CompressedSize: 1, UncompressedSize: 7, Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}}
	for _, test := range []struct {
		name      string
		bytes     int64
		wantError bool
	}{
		{"request", 0, false},
		{"maintenance", 8, false},
		{"insufficient budget", 7, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage := &batchDetailCounter{}
			owner, ok := attachment.DetailStore(LimitDetails(storage)).(attachment.DetailBatchStore)
			if !ok {
				t.Fatal("limiter removed batch reads")
			}
			ctx := t.Context()
			if test.bytes > 0 {
				ctx = (Budget{Items: 1, Bytes: test.bytes}).Context(ctx, Tenant{Organization: organization, Generation: 1})
			}
			rows, err := owner.ReadDetails(ctx, organization, refs)
			if test.wantError {
				if !errors.Is(err, ErrByteBudget) || storage.batches != 0 {
					t.Fatalf("refused batch: calls=%d error=%v", storage.batches, err)
				}
				return
			}
			if err != nil || storage.batches != 1 || len(rows) != 1 || string(rows[0].Data) != "example" {
				t.Fatalf("batch: calls=%d rows=%v error=%v", storage.batches, rows, err)
			}
		})
	}
}

type batchDetailCounter struct {
	detailCounter
	batches int
}

func (s *batchDetailCounter) ReadDetails(_ context.Context, _ string, refs []attachment.ArchiveReference) ([]attachment.DetailReadResult, error) {
	s.batches++
	return []attachment.DetailReadResult{{Reference: refs[0], Data: []byte("example")}}, nil
}
