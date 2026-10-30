package workspace

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReapingUsesInjectedProcessSource(t *testing.T) {
	for _, test := range []struct {
		name string
		reap func(context.Context, string, string, time.Duration) (int, error)
	}{
		{"workspace", func(ctx context.Context, root, _ string, grace time.Duration) (int, error) {
			return ReapProcesses(ctx, root, grace)
		}},
		{"worker artifacts", ReapWorkerArtifactProcesses},
		{"artifact path", ReapWorkerArtifactPathProcesses},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			scratch, err := PrepareWorkerScratch(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := CleanupWorkerScratch(root, scratch); err != nil {
					t.Error(err)
				}
			})
			want := errors.New("process source unavailable")
			calls := 0
			ctx := WithProcessSource(t.Context(), func(_ context.Context, path string) ([]int, error) {
				calls++
				if path != root && path != scratch {
					t.Errorf("scan path = %q", path)
				}
				return nil, want
			})
			_, err = test.reap(context.WithoutCancel(ctx), root, scratch, time.Millisecond)
			if !errors.Is(err, want) || calls == 0 {
				t.Fatalf("reap error = %v, source calls = %d", err, calls)
			}
		})
	}
}
