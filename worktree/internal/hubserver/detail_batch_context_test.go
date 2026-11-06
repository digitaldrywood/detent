package hubserver

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

func TestRequestDetailBatchReusesOnlyScopedVerifiedResults(t *testing.T) {
	owner := &boardDetailBatchCounter{DetailStore: &memoryRunDetails{}}
	service := &Service{config: Config{RunDetails: owner}}
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
		ctx := service.withRequestDetails(t.Context())
		owner.batches.Store(0)
		owner.scalar.Store(0)
		service.prefetchDetails(ctx, organization, append(append(refs, refs[0]), missing))
		for index, ref := range refs {
			data, err := service.runDetailStoreFor(ctx).ReadDetail(ctx, organization, ref)
			if err != nil || string(data) != fmt.Sprintf("example %03d", index) {
				t.Fatalf("cached row %d: %v", index, err)
			}
			data[0] ^= 1
			again, err := service.runDetailStoreFor(ctx).ReadDetail(ctx, organization, ref)
			if err != nil || string(again) != fmt.Sprintf("example %03d", index) {
				t.Fatal("caller mutated request cache")
			}
		}
		if _, err := service.runDetailStoreFor(ctx).ReadDetail(ctx, organization, missing); !errors.Is(err, artifact.ErrMissing) {
			t.Fatalf("missing consumed object: %v", err)
		}
		if _, err := service.runDetailStoreFor(ctx).ReadDetail(ctx, "org_foreign", refs[0]); !errors.Is(err, attachment.ErrInvalid) {
			t.Fatal("foreign cache read accepted")
		}
		service.prefetchDetails(ctx, organization, refs)
		if owner.batches.Load() != 2 || owner.scalar.Load() != 0 {
			t.Fatalf("batch/scalar calls %d/%d", owner.batches.Load(), owner.scalar.Load())
		}
		fresh := service.withRequestDetails(t.Context())
		service.prefetchDetails(fresh, organization, refs[:1])
		if owner.batches.Load() != 3 {
			t.Fatal("request reused previous allocation validation")
		}
	}
}
