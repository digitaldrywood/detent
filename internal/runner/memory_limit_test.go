package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

func TestObserveAgentRSS(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		rssBytes   uint64
		readErr    error
		wantBreach bool
		wantUpdate bool
	}{
		{name: "below ceiling", rssBytes: 511, wantUpdate: true},
		{name: "at ceiling", rssBytes: 512, wantUpdate: true},
		{name: "above ceiling", rssBytes: 513, wantBreach: true, wantUpdate: true},
		{name: "read failure is fail open", readErr: errors.New("rss unavailable")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var updates []AgentUpdate
			request := AgentTurnRequest{
				MaxRSSBytes: 512,
				processRSS: func(context.Context, procgroup.Identity) (uint64, error) {
					return tt.rssBytes, tt.readErr
				},
			}
			err := observeAgentRSS(t.Context(), request, procgroup.Identity{PID: 1899}, func(_ context.Context, update AgentUpdate) error {
				updates = append(updates, update)
				return nil
			})
			if got := errors.Is(err, ErrSessionMemoryCeilingExceeded); got != tt.wantBreach {
				t.Fatalf("observeAgentRSS() error = %v, breach = %v, want %v", err, got, tt.wantBreach)
			}
			if got := len(updates) == 1; got != tt.wantUpdate {
				t.Fatalf("updates = %#v, present = %v, want %v", updates, got, tt.wantUpdate)
			}
			if tt.wantUpdate && (updates[0].RSSBytes != tt.rssBytes || updates[0].RSSCeilingBytes != 512) {
				t.Fatalf("resource update = %#v, want RSS %d and ceiling 512", updates[0], tt.rssBytes)
			}
		})
	}
}

func TestRunAgentBackendTurnStopsOnInitialMemoryBreach(t *testing.T) {
	t.Parallel()

	backend := processStartingAgentBackend{identity: procgroup.Identity{PID: 1899}}
	_, err, cleanupErr := runAgentBackendTurnWithToolsUsingLimit(
		t.Context(),
		backend,
		AgentTurnRequest{
			MaxRSSBytes: 512,
			processRSS: func(context.Context, procgroup.Identity) (uint64, error) {
				return 513, nil
			},
		},
		nil,
		nil,
		nil,
		nil,
	)
	if !errors.Is(err, ErrSessionMemoryCeilingExceeded) {
		t.Fatalf("runAgentBackendTurnWithToolsUsingLimit() error = %v, want memory ceiling", err)
	}
	var memoryErr *SessionMemoryCeilingError
	if !errors.As(err, &memoryErr) || memoryErr.RSSBytes != 513 || memoryErr.CeilingBytes != 512 {
		t.Fatalf("memory error = %#v, want RSS 513 and ceiling 512", memoryErr)
	}
	if cleanupErr != nil {
		t.Fatalf("cleanup error = %v, want nil", cleanupErr)
	}
}

type processStartingAgentBackend struct {
	identity procgroup.Identity
}

func (b processStartingAgentBackend) RunTurn(_ context.Context, _ AgentTurnRequest, onUpdate AgentUpdateHandler) (AgentTurnResult, error) {
	if onUpdate == nil {
		return AgentTurnResult{}, nil
	}
	return AgentTurnResult{}, onUpdate(AgentUpdate{Type: AgentUpdateProcessStarted, WorkerProcess: b.identity})
}
