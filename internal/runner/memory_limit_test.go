package runner

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/transport"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestRunAgentBackendTurnMemoryPolling(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rss         uint64
		readErr     error
		outage      bool
		wantSamples int
	}{
		{"below ceiling", 511, nil, false, 1},
		{"at ceiling", 512, nil, false, 1},
		{"above ceiling", 513, nil, false, 1},
		{"read failure", 0, errors.New("rss unavailable"), false, 0},
		{"registration outage", 511, nil, true, 2},
		{"above ceiling during outage", 513, nil, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sessions := &memoryProcessStore{offline: tc.outage}
				observeProcess := (&Runner{store: sessions}).workerProcessObserver(1, "")
				identity := procgroup.Identity{PID: 1899, StartedAt: time.Now()}
				backend := terminalWorkerAgentBackend{identity: identity}
				restored := make(chan struct{})
				if tc.outage {
					backend.registered = restored
				}
				request := AgentTurnRequest{MaxRSSBytes: 512, TempDir: "attempt-scratch", processRSS: func(context.Context, procgroup.Identity) (uint64, error) { return tc.rss, tc.readErr }}
				samples := 0
				result, err, cleanup := runAgentBackendTurnWithToolsUsingLimit(t.Context(), backend, request, nil, nil, func(ctx context.Context, u AgentUpdate) error {
					if u.Type == AgentUpdateResourceUsage {
						samples++
						if u.WorkerProcess != identity || u.RSSBytes != tc.rss || u.RSSCeilingBytes != 512 || u.workerScratchPath != request.TempDir {
							t.Errorf("incorrect RSS observation: %+v", u)
						}
					}
					err := observeProcess(ctx, u, u.workerScratchPath)
					if sessions.calls == 3 {
						close(restored)
					}
					return err
				}, nil)
				if cleanup != nil || samples != tc.wantSamples {
					t.Fatalf("samples=%d want=%d cleanup=%v", samples, tc.wantSamples, cleanup)
				}
				if tc.rss > request.MaxRSSBytes {
					var breach *SessionMemoryCeilingError
					if !errors.Is(err, ErrSessionMemoryCeilingExceeded) || !errors.As(err, &breach) || breach.RSSBytes != tc.rss || breach.CeilingBytes != 512 {
						t.Fatalf("memory breach = %v", err)
					}
				} else if err != nil || result.TurnID == "" {
					t.Fatalf("turn=%q error=%v", result.TurnID, err)
				}
			})
		})
	}
}

type memoryProcessStore struct {
	SessionStore
	offline bool
	calls   int
}

func (s *memoryProcessStore) UpdateSessionWorkerProcess(context.Context, int64, store.WorkerProcessRegistration) error {
	s.calls++
	if s.offline && s.calls <= 2 {
		return transport.ErrUnavailable
	}
	return nil
}
