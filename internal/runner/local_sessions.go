package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type LocalSessionStore struct {
	SSHSessionStore
}

type localSessionStartResult struct {
	ID      int64
	Failure *sshError
}

func (s LocalSessionStore) StartSession(ctx context.Context, start store.SessionStart) (int64, error) {
	if start.Worker == nil {
		return s.SSHSessionStore.StartSession(ctx, start)
	}
	var result localSessionStartResult
	if err := s.Peer.Call(ctx, "local.start", &result, start); err != nil {
		return 0, err
	}
	return result.ID, result.Failure.err()
}

func (s LocalSessionStore) FinishSession(ctx context.Context, id int64, finish store.SessionFinish) error {
	if finish.Reap == nil {
		return s.SSHSessionStore.FinishSession(ctx, id, finish)
	}
	return s.Peer.Call(ctx, "local.finish", nil, id, finish)
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

func (c *SSHCallbacks) startLocalSession(ctx context.Context, start store.SessionStart) (localSessionStartResult, error) {
	if start.Worker == nil || strings.TrimSpace(start.Worker.CleanupPath) != "" && strings.TrimSpace(start.Worker.CleanupRoot) == "" {
		return localSessionStartResult{}, errors.New("local session start requires worker ownership and a cleanup root for scratch")
	}
	root, err := workspace.WorkerArtifactCleanupRoot(start.Worker.CleanupRoot, start.Worker.CleanupPath)
	if err != nil {
		return localSessionStartResult{}, err
	}
	start.Worker.CleanupRoot = root
	id, err := c.store.StartSession(ctx, start)
	if err != nil && id <= 0 {
		return localSessionStartResult{}, err
	}
	return localSessionStartResult{ID: id, Failure: encodeSSHError(err)}, nil
}

func (c *SSHCallbacks) finishLocalSession(ctx context.Context, id int64, finish store.SessionFinish) error {
	if finish.Reap == nil {
		return errors.New("local session finish requires process reap")
	}
	return c.store.FinishSession(ctx, id, finish)
}

func (c *SSHCallbacks) handleLocalProcess(ctx context.Context, method string, args []json.RawMessage) (any, error) {
	if c.local == nil {
		return nil, errors.New("local process ownership is unavailable on a remote channel")
	}
	if method == "local.start" {
		if c.store == nil {
			return nil, ErrExecutionAuthorityUnavailable
		}
		return invokeSSHMethod(ctx, c.startLocalSession, "", args)
	}
	if method != "local.processes" {
		var id int64
		if len(args) != 2 || json.Unmarshal(args[0], &id) != nil {
			return nil, errors.New("invalid local process arguments")
		}
		c.mu.Lock()
		_, owned := c.active[id]
		c.mu.Unlock()
		if !owned {
			return nil, errors.New("local process operation requires a session opened by this worker")
		}
	}
	switch method {
	case "local.finish":
		return invokeSSHMethod(ctx, c.finishLocalSession, "", args)
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
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, process := range active {
			if _, owned := c.active[process.SessionID]; owned {
				result = append(result, process)
			}
		}
		return result, nil
	}
	return nil, errors.New("unsupported local process operation")
}
