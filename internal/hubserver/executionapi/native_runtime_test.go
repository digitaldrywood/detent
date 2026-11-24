package executionapi

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReadNativeRuntimeCallerContext(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cancelAt time.Duration
		elapsed  time.Duration
		wantErr  error
	}{
		{name: "read completes past old cutoff", elapsed: 20 * time.Second},
		{name: "caller cancellation interrupts read", cancelAt: time.Second, elapsed: time.Second, wantErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				started := time.Now()
				if tc.cancelAt > 0 {
					timer := time.AfterFunc(tc.cancelAt, cancel)
					defer timer.Stop()
				}
				identity := func(ctx context.Context) context.Context { return ctx }
				d := RuntimeDeps{
					DetailContext:  identity,
					RequestDetails: identity,
					ReadWorkItem: func(ctx context.Context, _ hubscope.NativeScope, _ string) (corestore.WorkItem, eventstore.CollaborationVersion, error) {
						timer := time.NewTimer(20 * time.Second)
						defer timer.Stop()
						select {
						case <-ctx.Done():
							return corestore.WorkItem{}, eventstore.CollaborationVersion{}, ctx.Err()
						case <-timer.C:
							return corestore.WorkItem{}, eventstore.CollaborationVersion{}, nil
						}
					},
					HydrateWorkItem: func(context.Context, corestore.WorkItem, eventstore.CollaborationVersion) (tracker.NativeIssue, error) {
						return tracker.NativeIssue{}, nil
					},
					HydrateRelationships: func(context.Context, hubscope.NativeScope, *tracker.NativeIssue) error { return nil },
					ReadOperatorEvidence: func(context.Context, hubscope.NativeScope, tracker.NativeIssue, bool) (tracker.NativeRuntimeEvidence, error) {
						return tracker.NativeRuntimeEvidence{Attempt: &tracker.NativeAttempt{}}, nil
					},
					ReadAdmission: func(context.Context, hubscope.NativeScope, tracker.NativeIssue, *tracker.NativeRuntimeEvidence, []tracker.NativeAdmissionContext) error {
						return nil
					},
				}
				if _, err := d.ReadNativeRuntime(ctx, hubscope.NativeScope{}, "wi_example", ""); !errors.Is(err, tc.wantErr) {
					t.Fatalf("runtime read error = %v, want %v", err, tc.wantErr)
				}
				if elapsed := time.Since(started); elapsed != tc.elapsed {
					t.Errorf("runtime read took %s, want %s", elapsed, tc.elapsed)
				}
			})
		})
	}
}
