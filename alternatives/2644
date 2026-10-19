package runner

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type localProcessSessions struct {
	fakeSessionStore
	processes map[int64]store.WorkerProcess
	reaped    map[int64]int
}

func (s *localProcessSessions) StartSession(context.Context, store.SessionStart) (int64, error) {
	s.sessionID++
	return s.sessionID, nil
}

func (s *localProcessSessions) UpdateSessionWorkerProcess(_ context.Context, id int64, process store.WorkerProcessRegistration) error {
	s.processes[id] = store.WorkerProcess{SessionID: id, WorkerProcessIdentity: process.WorkerProcessIdentity, CleanupRoot: process.CleanupRoot, CleanupPath: process.CleanupPath}
	return nil
}

func (s *localProcessSessions) ListActiveWorkerProcesses(context.Context) ([]store.WorkerProcess, error) {
	processes := make([]store.WorkerProcess, 0, len(s.processes))
	for _, process := range s.processes {
		processes = append(processes, process)
	}
	return processes, nil
}

func (s *localProcessSessions) MarkSessionWorkerProcessReaped(_ context.Context, id int64, _ store.WorkerProcessReap) error {
	s.reaped[id]++
	return nil
}

func TestLocalSessionProcessOwnership(t *testing.T) {
	sessions := &localProcessSessions{processes: make(map[int64]store.WorkerProcess), reaped: make(map[int64]int)}
	run := &Runner{store: sessions}
	first, second := run.LocalRunCallbacks(RunRequest{}), run.LocalRunCallbacks(RunRequest{})
	call := func(channel *SSHCallbacks, method string, values ...any) (any, error) {
		t.Helper()
		var args []json.RawMessage
		for _, value := range values {
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			args = append(args, raw)
		}
		return channel.Handle(t.Context(), method, args)
	}
	for index, channel := range []*SSHCallbacks{first, second} {
		id, err := call(channel, "store.StartSession", store.SessionStart{})
		if err != nil || id != int64(index+1) {
			t.Fatalf("independent session not created: %v %v", id, err)
		}
		if _, err := call(channel, "local.register", id, store.WorkerProcessRegistration{WorkerProcessIdentity: store.WorkerProcessIdentity{PID: 100 + index}}); err != nil {
			t.Fatal(err)
		}
	}
	for index, channel := range []*SSHCallbacks{first, second} {
		own, other := int64(index+1), int64(2-index)
		result, err := call(channel, "local.processes")
		processes, ok := result.([]store.WorkerProcess)
		if err != nil || !ok || len(processes) != 1 || processes[0].SessionID != own {
			t.Fatalf("worker observed another channel's processes: %v %v", result, err)
		}
		if _, err := call(channel, "local.reaped", other, store.WorkerProcessReap{}); err == nil {
			t.Fatal("worker could reap another channel's session")
		}
		if _, err := call(channel, "local.register", other, store.WorkerProcessRegistration{}); err == nil {
			t.Fatal("worker could replace another channel's process identity")
		}
		if _, err := call(channel, "local.reaped", own, store.WorkerProcessReap{}); err != nil || sessions.reaped[own] != 1 {
			t.Fatalf("own process was unusable: %v", err)
		}
	}
	childTemp, controllerTemp, workspacePath := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("TMPDIR", childTemp)
	t.Setenv("TMP", childTemp)
	t.Setenv("TEMP", childTemp)
	scratch, err := workspace.PrepareWorkerScratch(t.Context(), workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	central := NewSSHPeer(t.Context(), a, a, first.Handle)
	defer central.Close()
	child := NewSSHPeer(t.Context(), b, b, nil)
	defer child.Close()
	local := LocalSessionStore{SSHSessionStore: SSHSessionStore{Peer: child}}
	if err := local.UpdateSessionWorkerProcess(t.Context(), 1, store.WorkerProcessRegistration{CleanupRoot: workspacePath, CleanupPath: scratch}); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := local.UpdateSessionWorkerProcess(t.Context(), 1, store.WorkerProcessRegistration{CleanupRoot: workspacePath, CleanupPath: outside}); err == nil {
		t.Fatal("unrelated cleanup path accepted")
	}
	link := filepath.Join(filepath.Dir(scratch), "attempt-link")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
	} else if err := local.UpdateSessionWorkerProcess(t.Context(), 1, store.WorkerProcessRegistration{CleanupRoot: workspacePath, CleanupPath: link}); err == nil {
		t.Fatal("symlink cleanup path accepted")
	}
	t.Setenv("TMPDIR", controllerTemp)
	t.Setenv("TMP", controllerTemp)
	t.Setenv("TEMP", controllerTemp)
	recorded := sessions.processes[1]
	if _, err := workspace.ReapWorkerArtifactProcesses(t.Context(), recorded.CleanupRoot, recorded.CleanupPath, time.Millisecond); err != nil {
		t.Fatalf("controller could not reap child scratch: %v", err)
	}
	if err := workspace.CleanupOwnedPath(recorded.CleanupRoot, recorded.CleanupPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatal("registered scratch remains")
	}
	if _, err := os.Stat(workspacePath); err != nil {
		t.Fatal("cleanup removed issue workspace")
	}
	if filepath.Clean(recorded.CleanupPath) != filepath.Clean(scratch) {
		t.Fatal("scratch identity changed")
	}
	remote := run.SSHRunCallbacks(RunRequest{})
	if _, err := call(remote, "local.processes"); err == nil {
		t.Fatal("remote channel entered the local process registry")
	}
}
