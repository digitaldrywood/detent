package runner

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type LocalSessionStore struct {
	SSHSessionStore
}

func (s LocalSessionStore) UpdateSessionWorkerProcess(ctx context.Context, id int64, process store.WorkerProcessRegistration) error {
	root, err := workspace.WorkerArtifactCleanupRoot(process.CleanupRoot, process.CleanupPath)
	if err != nil {
		return err
	}
	process.CleanupRoot = root
	return s.Peer.Call(ctx, "local.register", nil, id, process)
}

func (s LocalSessionStore) ListActiveWorkerProcesses(ctx context.Context) ([]store.WorkerProcess, error) {
	var processes []store.WorkerProcess
	err := s.Peer.Call(ctx, "local.processes", &processes)
	return processes, err
}

func (s LocalSessionStore) MarkSessionWorkerProcessReaped(ctx context.Context, id int64, result store.WorkerProcessReap) error {
	return s.Peer.Call(ctx, "local.reaped", nil, id, result)
}

func (c *SSHCallbacks) handleLocalProcess(ctx context.Context, method string, args []json.RawMessage) (any, error) {
	if c.local == nil {
		return nil, errors.New("local process ownership is unavailable on a remote channel")
	}
	c.mu.Lock()
	owned := make(map[int64]bool, len(c.active))
	for id := range c.active {
		owned[id] = true
	}
	c.mu.Unlock()
	if method != "local.processes" {
		var id int64
		if len(args) != 2 || json.Unmarshal(args[0], &id) != nil || !owned[id] {
			return nil, errors.New("local process operation requires a session opened by this worker")
		}
	}
	switch method {
	case "local.register":
		processes, ok := c.store.(sessionWorkerProcessStore)
		if !ok {
			return nil, errors.New("local session process registration is unavailable")
		}
		return invokeSSHMethod(ctx, processes, "UpdateSessionWorkerProcess", args)
	case "local.reaped":
		processes, ok := c.store.(sessionWorkerProcessReaper)
		if !ok {
			return nil, errors.New("local session process cleanup is unavailable")
		}
		return invokeSSHMethod(ctx, processes, "MarkSessionWorkerProcessReaped", args)
	case "local.processes":
		processes, ok := c.store.(sessionWorkerProcessReaper)
		if !ok || len(args) != 0 {
			return nil, errors.New("local session process reads are unavailable")
		}
		active, err := processes.ListActiveWorkerProcesses(ctx)
		if err != nil {
			return nil, err
		}
		result := make([]store.WorkerProcess, 0, len(active))
		for _, process := range active {
			if owned[process.SessionID] {
				result = append(result, process)
			}
		}
		return result, nil
	}
	return nil, errors.New("unsupported local process operation")
}
