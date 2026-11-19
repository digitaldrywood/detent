package hubconfig

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

type maintenanceBatchProbe struct {
	*hubtest.Details
	batches int
}

func (s *maintenanceBatchProbe) ReadDetails(ctx context.Context, org string, refs []attachment.ArchiveReference) ([]attachment.DetailReadResult, error) {
	s.batches++
	return attachment.ReadDetailBatch(ctx, org, refs, s.ReadDetail)
}

func TestHostedMaintenancePreservesRequestBatchReads(t *testing.T) {
	t.Parallel()
	store := &maintenanceBatchProbe{Details: hubtest.NewDetails()}
	ref, err := store.StoreDetail(t.Context(), "org_example", bytes.Repeat([]byte("example"), 10))
	if err != nil {
		t.Fatal(err)
	}
	cfg := (Config{DatabaseBackend: DatabasePostgres, RunDetails: store}).Normalized()
	batch, ok := cfg.RunDetails.(attachment.DetailBatchStore)
	if !ok {
		t.Fatal("normalization removed batched request reads")
	}
	for _, remaining := range []int64{0, ref.CompressedSize + ref.UncompressedSize} {
		ctx := t.Context()
		if remaining > 0 {
			ctx = (hubmaintenance.Budget{Items: 1, Bytes: remaining}).Context(ctx, hubmaintenance.Tenant{Organization: "org_example", Generation: 1})
		}
		rows, err := batch.ReadDetails(ctx, "org_example", []attachment.ArchiveReference{ref})
		if err != nil || len(rows) != 1 {
			t.Fatalf("batch read: rows=%d err=%v", len(rows), err)
		}
		if remaining > 0 {
			if _, err := batch.ReadDetails(ctx, "org_example", []attachment.ArchiveReference{ref}); err == nil {
				t.Fatal("exhausted batch budget accepted")
			}
		}
	}
	if store.batches != 2 {
		t.Fatalf("batched I/O calls=%d want=2", store.batches)
	}
}

func TestDatabaseBackendRejectsBindingMigration(t *testing.T) {
	t.Parallel()
	cfg := Config{DatabaseBackend: DatabasePostgres, Postgres: &postgres.Config{}, hostedBindingMigration: true}
	if err := cfg.Normalized().ValidateDatabaseBackend(); !errors.Is(err, ErrDatabaseBackend) {
		t.Fatalf("binding migration validation = %v, want %v", err, ErrDatabaseBackend)
	}
}
