package detailbatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

func TestRequestDetailBatchReusesOnlyScopedVerifiedResults(t *testing.T) {
	owner := &boardDetailBatchCounter{DetailStore: hubtest.NewDetails()}
	service := New(Deps{Store: func() attachment.DetailStore { return owner }})
	for _, organization := range []string{"org_first", "org_second"} {
		refs := make([]attachment.ArchiveReference, 201)
		for index := range refs {
			ref, err := owner.StoreDetail(t.Context(), organization, []byte(fmt.Sprintf("example %03d", index)))
			if err != nil {
				t.Fatal(err)
			}
			refs[index] = ref
		}
		missing, _, err := attachment.PrepareArchive(organization, 1, bytes.NewBufferString("absent optional later object"))
		if err != nil {
			t.Fatal(err)
		}
		ctx := service.WithRequest(t.Context())
		owner.batches.Store(0)
		owner.scalar.Store(0)
		service.Prefetch(ctx, organization, append(append(refs, refs[0]), missing))
		for index, ref := range refs {
			data, err := service.StoreFor(ctx).ReadDetail(ctx, organization, ref)
			if err != nil || string(data) != fmt.Sprintf("example %03d", index) {
				t.Fatalf("cached row %d: %v", index, err)
			}
			data[0] ^= 1
			again, err := service.StoreFor(ctx).ReadDetail(ctx, organization, ref)
			if err != nil || string(again) != fmt.Sprintf("example %03d", index) {
				t.Fatal("caller mutated request cache")
			}
		}
		if _, err := service.StoreFor(ctx).ReadDetail(ctx, organization, missing); !errors.Is(err, artifact.ErrMissing) {
			t.Fatalf("missing consumed object: %v", err)
		}
		if _, err := service.StoreFor(ctx).ReadDetail(ctx, "org_foreign", refs[0]); !errors.Is(err, attachment.ErrInvalid) {
			t.Fatal("foreign cache read accepted")
		}
		service.Prefetch(ctx, organization, refs)
		if owner.batches.Load() != 2 || owner.scalar.Load() != 0 {
			t.Fatalf("batch/scalar calls %d/%d", owner.batches.Load(), owner.scalar.Load())
		}
		fresh := service.WithRequest(t.Context())
		service.Prefetch(fresh, organization, refs[:1])
		if owner.batches.Load() != 3 {
			t.Fatal("request reused previous allocation validation")
		}
		owner.scalar.Store(0)
		for range 2 {
			data, err := service.StoreFor(fresh).ReadDetail(fresh, organization, refs[1])
			if err != nil || string(data) != "example 001" {
				t.Fatalf("repeated scalar read: %v", err)
			}
		}
		if _, err := service.Contextual().ReadDetail(fresh, organization, refs[1]); err != nil || owner.scalar.Load() != 1 {
			t.Fatalf("request repeated an object read: scalar=%d error=%v", owner.scalar.Load(), err)
		}
		if _, err := service.Contextual().ReadDetail(t.Context(), organization, refs[1]); err != nil || owner.scalar.Load() != 2 {
			t.Fatalf("read outside the request used its cache: scalar=%d error=%v", owner.scalar.Load(), err)
		}
	}
}

type boardDetailBatchCounter struct {
	attachment.DetailStore
	batches atomic.Int64
	scalar  atomic.Int64
}

func (d *boardDetailBatchCounter) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	d.scalar.Add(1)
	return d.DetailStore.ReadDetail(ctx, organization, ref)
}
func (d *boardDetailBatchCounter) ReadDetails(ctx context.Context, organization string, refs []attachment.ArchiveReference) ([]attachment.DetailReadResult, error) {
	d.batches.Add(1)
	return attachment.ReadDetailBatch(ctx, organization, refs, d.DetailStore.ReadDetail)
}
