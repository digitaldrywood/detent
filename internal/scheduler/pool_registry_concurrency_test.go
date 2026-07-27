package scheduler

import (
	"runtime"
	"testing"
	"time"
)

func TestPoolRegistrySerializesReadyRouteWithReconfigure(t *testing.T) {
	t.Parallel()

	project := ProjectCandidate{ID: "worker", Pool: "code"}
	registry, err := NewPoolRegistry([]PoolConfig{
		{Name: DefaultPoolName, Scheduler: Config{Kind: "weighted", Capacity: 1}},
		{Name: "code", Scheduler: Config{Kind: "weighted", Capacity: 1}},
		{Name: "video", Scheduler: Config{Kind: "weighted", Capacity: 1}},
	}, []ProjectCandidate{project})
	if err != nil {
		t.Fatalf("NewPoolRegistry() error = %v", err)
	}
	if registry == nil {
		t.Fatal("NewPoolRegistry() = nil")
		return
	}
	oldRuntime, ok := registry.active["code"]
	if !ok || oldRuntime == nil || oldRuntime.gate == nil {
		t.Fatalf("code pool runtime = %#v, want configured gate", oldRuntime)
		return
	}
	oldRuntime.gate.mu.Lock()

	readyDone := make(chan struct{})
	go func() {
		defer close(readyDone)
		registry.MarkReady(project)
	}()

	deadline := time.Now().Add(time.Second)
	for registry.reconfigureMu.TryLock() {
		registry.reconfigureMu.Unlock()
		if time.Now().After(deadline) {
			oldRuntime.gate.mu.Unlock()
			<-readyDone
			t.Fatal("MarkReady() did not lock routing")
		}
		runtime.Gosched()
	}

	reconfigureDone := make(chan error, 1)
	go func() {
		reconfigureDone <- registry.Reconfigure([]PoolConfig{
			{Name: DefaultPoolName, Scheduler: Config{Kind: "weighted", Capacity: 1}},
			{Name: "code", Scheduler: Config{Kind: "weighted", Capacity: 1}},
			{Name: "video", Scheduler: Config{Kind: "weighted", Capacity: 1}},
		}, []ProjectCandidate{{ID: project.ID, Pool: "video"}})
	}()

	oldRuntime.gate.mu.Unlock()
	<-readyDone
	if err := <-reconfigureDone; err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	oldRuntime.gate.mu.Lock()
	_, ready := oldRuntime.gate.ready[project.ID]
	_, configured := oldRuntime.gate.projects[project.ID]
	oldRuntime.gate.mu.Unlock()
	if ready || configured {
		t.Fatalf("old pool retained project after reassignment: ready=%t configured=%t", ready, configured)
	}
	if snapshot := registry.PoolSnapshotFor(project.ID); snapshot.Name != "video" {
		t.Fatalf("PoolSnapshotFor() = %#v, want video", snapshot)
	}
}
