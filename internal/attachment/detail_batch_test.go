package attachment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
)

func TestDetailBatchBoundsJoinsAndPreservesFailures(t *testing.T) {
	refs := make([]ArchiveReference, 24)
	for index := range refs {
		var err error
		refs[index], _, err = PrepareArchive("org_example", 1, bytes.NewBufferString(fmt.Sprintf("example %02d", index)))
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered := make(chan struct{}, 8)
			release := make(chan struct{})
			var active, peak atomic.Int64
			done := make(chan []DetailReadResult, 1)
			go func() {
				rows, err := ReadDetailBatch(ctx, "org_example", refs, func(ctx context.Context, _ string, ref ArchiveReference) ([]byte, error) {
					current := active.Add(1)
					defer active.Add(-1)
					for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
					}
					select {
					case entered <- struct{}{}:
					default:
					}
					select {
					case <-release:
					case <-ctx.Done():
						return []byte("must not retain"), ctx.Err()
					}
					if ref == refs[3] {
						return []byte("must not retain"), artifact.ErrMissing
					}
					if ref == refs[4] {
						return []byte("short"), nil
					}
					for index, candidate := range refs {
						if candidate == ref {
							return []byte(fmt.Sprintf("example %02d", index)), nil
						}
					}
					return nil, artifact.ErrIntegrity
				})
				if err != nil {
					t.Error(err)
				}
				done <- rows
			}()
			for range 8 {
				<-entered
			}
			if canceled {
				cancel()
			} else {
				close(release)
			}
			rows := <-done
			if active.Load() != 0 || peak.Load() != 8 || len(rows) != len(refs) {
				t.Fatalf("active=%d peak=%d rows=%d", active.Load(), peak.Load(), len(rows))
			}
			for index, row := range rows {
				if row.Reference != refs[index] {
					t.Fatal("reordered reference")
				}
				data, err := row.Bytes()
				switch {
				case canceled:
					if !errors.Is(err, context.Canceled) || len(row.Data) != 0 {
						t.Fatalf("cancellation: %v", err)
					}
				case index == 3:
					if !errors.Is(err, artifact.ErrMissing) || len(row.Data) != 0 {
						t.Fatalf("missing: %v", err)
					}
				case index == 4:
					if !errors.Is(err, artifact.ErrIntegrity) || len(row.Data) != 0 {
						t.Fatalf("wrong size: %v", err)
					}
				default:
					if err != nil || string(data) != fmt.Sprintf("example %02d", index) {
						t.Fatalf("row %d: %v", index, err)
					}
				}
			}
		})
	}
	for _, batch := range [][]ArchiveReference{nil, append(make([]ArchiveReference, MaxDetailBatchReferences), refs[0]), {refs[0]}} {
		organization := "org_example"
		if len(batch) == 1 {
			organization = "org_foreign"
		}
		if _, err := ReadDetailBatch(t.Context(), organization, batch, func(context.Context, string, ArchiveReference) ([]byte, error) {
			t.Fatal("invalid batch reached storage")
			return nil, nil
		}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid scope/size accepted: %v", err)
		}
	}
}
