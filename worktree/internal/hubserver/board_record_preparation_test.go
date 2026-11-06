package hubserver

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardRecordPreparationBoundsWorkersAndPreservesErrors(t *testing.T) {
	for _, test := range []struct {
		name    string
		cancel  bool
		failure int
		used    int
	}{{"ordered page", false, -1, 17}, {"joined cancellation", true, -1, 0}, {"required failure", false, 0, 1}, {"unreturned failure", false, 16, 1}} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			records := make([]viewstore.BoardRecord, 17)
			for index := range records {
				records[index].SourceRevision = int64(index)
			}
			started := make(chan struct{}, len(records))
			release := make(chan struct{})
			failed := make(chan struct{})
			var active, maximum, finished atomic.Int32
			p := prepareBoardRecordPage(ctx, records, func(ctx context.Context, row viewstore.BoardRecord) (tracker.NativeBoardCard, error) {
				current := active.Add(1)
				for prior := maximum.Load(); current > prior && !maximum.CompareAndSwap(prior, current); prior = maximum.Load() {
				}
				defer active.Add(-1)
				defer finished.Add(1)
				started <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return tracker.NativeBoardCard{}, ctx.Err()
				}
				if int(row.SourceRevision) == test.failure {
					close(failed)
					return tracker.NativeBoardCard{}, errNoApprovedRepositoryPolicy
				}
				return tracker.NativeBoardCard{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{Number: int(row.SourceRevision)}}}, nil
			})
			t.Cleanup(p.close)
			for range 8 {
				<-started
			}
			if maximum.Load() != 8 {
				t.Fatalf("active workers=%d", maximum.Load())
			}
			if test.cancel {
				p.close()
				if finished.Load() != 8 || active.Load() != 0 {
					t.Fatalf("canceled workers remain: finished%d active%d", finished.Load(), active.Load())
				}
				return
			}
			close(release)
			if test.failure >= test.used {
				<-failed
			}
			for index := range test.used {
				card, err := p.card(ctx, index)
				if index == test.failure {
					if !errors.Is(err, errNoApprovedRepositoryPolicy) {
						t.Fatalf("required failure=%v", err)
					}
					break
				}
				if err != nil || card.Issue.Number != index {
					t.Fatalf("ordered row%d number%d error%v", index, card.Issue.Number, err)
				}
			}
			p.close()
			if active.Load() != 0 || maximum.Load() > 8 {
				t.Fatalf("unjoined or excessive workers: active%d max%d", active.Load(), maximum.Load())
			}
		})
	}
}
