package dispatch

import (
	"context"
	"hash/maphash"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

type machineSyncPoll struct {
	done    chan struct{}
	changed bool
}

func (s *Scheduler) waitMachineSync(ctx context.Context, client *Client, wake chan<- struct{}) {
	interval := 24*time.Second + time.Duration(maphash.String(maphash.MakeSeed(), string(s.machine.ID))%7)*time.Second
	for ctx.Err() == nil {
		s.mu.Lock()
		poll := s.syncPoll
		leader := poll == nil
		if leader {
			poll = &machineSyncPoll{done: make(chan struct{})}
			s.syncPoll = poll
		}
		versions := s.syncVersions
		s.mu.Unlock()
		if leader {
			wait := int(interval / time.Second)
			if versions.Cursor == "" {
				wait = 0
			}
			result, err := client.SyncRunner(ctx, runnerauth.SyncRequest{Versions: versions, WaitSeconds: wait})
			if err != nil && ctx.Err() == nil {
				timer := time.NewTimer(interval)
				select {
				case <-timer.C:
				case <-ctx.Done():
				}
				timer.Stop()
			}
			s.mu.Lock()
			if err == nil && result.Versions != nil {
				s.syncVersions = *result.Versions
				poll.changed = !result.NoChange || versions.Cursor != result.Versions.Cursor
			}
			s.syncPoll = nil
			close(poll.done)
			s.mu.Unlock()
		} else {
			select {
			case <-ctx.Done():
				return
			case <-poll.done:
			}
		}
		if poll.changed {
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}
}
