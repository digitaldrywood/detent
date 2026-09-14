package scheduler

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestQueuedDispatchRanksSynchronousAndQueuedCallersTogether(t *testing.T) {
	for _, useRegistry := range []bool{false, true} {
		for _, higherQueued := range []bool{false, true} {
			name := map[bool]string{false: "gate", true: "registry"}[useRegistry] + "/" + map[bool]string{false: "higher synchronous", true: "higher queued"}[higherQueued]
			t.Run(name, func(t *testing.T) {
				global := NewGlobalDispatchGate(NewStrictPriority(Config{Capacity: 1}))
				var gate ProjectDispatchGate = global
				lock, unlock := global.mu.Lock, global.mu.Unlock
				pendingCount := func() int { global.requestMu.Lock(); defer global.requestMu.Unlock(); return len(global.pending) }
				if useRegistry {
					r, err := NewPoolRegistry([]PoolConfig{{Name: DefaultPoolName, Scheduler: Config{Kind: "strict", Capacity: 1}}}, nil)
					if err != nil {
						t.Fatal(err)
					}
					gate, lock, unlock = r, r.reconfigureMu.Lock, r.reconfigureMu.Unlock
					pendingCount = func() int { r.requestMu.Lock(); defer r.requestMu.Unlock(); return len(r.pending) }
				}
				type result struct {
					id      string
					slot    Slot
					granted bool
					err     error
				}
				results := make(chan result, 2)
				lock()
				for _, project := range []ProjectCandidate{{ID: "lower", Priority: 4}, {ID: "higher", Priority: 1}} {
					go func() {
						out := result{id: project.ID}
						if (project.ID == "higher") == higherQueued {
							response, cancel, _ := gate.(QueuedProjectDispatchGate).Submit(t.Context(), project, SlotRequest{State: "Todo"}, time.Now(), nil)
							select {
							case grant := <-response:
								out.slot, out.granted, out.err = grant.Slot, grant.Err == nil, grant.Err
							default:
							}
							cancel()
						} else {
							out.slot, out.granted, out.err = gate.TryAcquire(t.Context(), project, SlotRequest{State: "Todo"}, time.Now())
						}
						results <- out
					}()
				}
				deadline := time.Now().Add(5 * time.Second)
				for pendingCount() != 2 {
					if time.Now().After(deadline) {
						unlock()
						t.Fatal("mixed callers did not reach the common acquisition path")
					}
					runtime.Gosched()
				}
				unlock()
				var held Slot
				for range 2 {
					got := <-results
					if got.err != nil || got.granted != (got.id == "higher") {
						t.Fatalf("result = %+v", got)
					}
					if got.granted {
						held = got.slot
					}
				}
				if err := gate.Release(held); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestQueuedDispatchReleaseIncludesEnrolledCallers(t *testing.T) {
	for _, useRegistry := range []bool{false, true} {
		t.Run(map[bool]string{false: "gate", true: "registry"}[useRegistry], func(t *testing.T) {
			global := NewGlobalDispatchGate(NewStrictPriority(Config{Capacity: 1}))
			var gate ProjectDispatchGate = global
			enroll := func(call *dispatchRequest) {
				global.requestMu.Lock()
				defer global.requestMu.Unlock()
				global.pending = append(global.pending, call)
			}
			if useRegistry {
				r, err := NewPoolRegistry([]PoolConfig{{Name: DefaultPoolName, Scheduler: Config{Kind: "strict", Capacity: 1}}}, nil)
				if err != nil {
					t.Fatal(err)
				}
				gate = r
				enroll = func(call *dispatchRequest) {
					r.requestMu.Lock()
					defer r.requestMu.Unlock()
					r.pending = append(r.pending, call)
				}
			}
			held, ok, err := gate.TryAcquire(t.Context(), ProjectCandidate{ID: "holder"}, SlotRequest{State: "Todo"}, time.Now())
			if err != nil || !ok {
				t.Fatalf("initial grant = %t %v", ok, err)
			}
			lower, cancel, _ := gate.(QueuedProjectDispatchGate).Submit(t.Context(), ProjectCandidate{ID: "lower", Priority: 4}, SlotRequest{State: "Todo"}, time.Now(), nil)
			defer cancel()
			// Freeze a caller after intake enrollment and before mutex acquisition.
			// Release must use this same intake, not only the retained queue.
			higher := &dispatchRequest{ctx: t.Context(), project: ProjectCandidate{ID: "higher", Priority: 1}, request: SlotRequest{State: "Todo"}, now: time.Now()}
			enroll(higher)
			if err := gate.Release(held); err != nil {
				t.Fatal(err)
			}
			if !higher.granted || higher.err != nil {
				t.Fatalf("enrolled higher request ignored: %+v", higher)
			}
			select {
			case got := <-lower:
				t.Fatalf("lower bypassed enrolled higher request: %+v", got)
			default:
			}
			if err := gate.Release(higher.slot); err != nil {
				t.Fatal(err)
			}
			select {
			case grant := <-lower:
				if grant.Err != nil {
					t.Fatal(grant.Err)
				}
				if err := gate.Release(grant.Slot); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("lower was not dispatched on subsequent release")
			}
		})
	}
}

func TestQueuedDispatchRanksIndependentRequests(t *testing.T) {
	for _, registry := range []bool{false, true} {
		for _, higherFirst := range []bool{false, true} {
			name := "gate"
			if registry {
				name = "registry"
			}
			if higherFirst {
				name += "/higher-first"
			} else {
				name += "/lower-first"
			}
			t.Run(name, func(t *testing.T) {
				var gate ProjectDispatchGate = NewGlobalDispatchGate(NewStrictPriority(Config{Capacity: 1}))
				if registry {
					r, err := NewPoolRegistry([]PoolConfig{{Name: DefaultPoolName, Scheduler: Config{Kind: "strict", Capacity: 1}}}, nil)
					if err != nil {
						t.Fatal(err)
					}
					gate = r
				}
				running, ok, err := gate.TryAcquire(t.Context(), ProjectCandidate{ID: "running", Priority: 5}, SlotRequest{State: "Todo"}, time.Now())
				if err != nil || !ok {
					t.Fatalf("initial grant: %t %v", ok, err)
				}
				projects := []ProjectCandidate{{ID: "lower", Priority: 4}, {ID: "higher", Priority: 1}}
				if higherFirst {
					projects[0], projects[1] = projects[1], projects[0]
				}
				results := make(map[string]<-chan DispatchResult)
				for _, project := range projects {
					result, cancel, decision := gate.(QueuedProjectDispatchGate).Submit(t.Context(), project, SlotRequest{State: "Todo"}, time.Now(), make(chan struct{}, 1))
					t.Cleanup(cancel)
					if decision.Reason != DispatchGateReasonGlobalCapacityFull {
						t.Fatalf("refusal = %+v", decision)
					}
					results[project.ID] = result
					select {
					case got := <-result:
						t.Fatalf("running work replaced: %+v", got)
					default:
					}
				}
				if err := gate.Release(running); err != nil {
					t.Fatal(err)
				}
				var higher DispatchResult
				select {
				case higher = <-results["higher"]:
				default:
					t.Fatal("higher request was not granted on release")
				}
				if higher.Err != nil || higher.Slot == (Slot{}) {
					t.Fatalf("higher result = %+v", higher)
				}
				select {
				case got := <-results["lower"]:
					t.Fatalf("lower granted too soon: %+v", got)
				default:
				}
				if err := gate.Release(higher.Slot); err != nil {
					t.Fatal(err)
				}
				select {
				case lower := <-results["lower"]:
					if lower.Err != nil || lower.Slot == (Slot{}) {
						t.Fatalf("lower result = %+v", lower)
					}
					if err := gate.Release(lower.Slot); err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatal("lower request was not granted on the next release")
				}
			})
		}
	}
}

func TestQueuedDispatchPreservesProjectCeilings(t *testing.T) {
	for _, tt := range []struct {
		name    string
		request SlotRequest
		want    string
	}{
		{"project", SlotRequest{State: "Todo", ProjectCapacity: 1}, DecisionReasonProjectCapacityFull},
		{"state", SlotRequest{State: "Todo", ProjectStateCapacity: 1}, DecisionReasonLocalSlotUnavailable},
		{"host", SlotRequest{State: "Todo", Host: "host", ProjectHostCapacity: 1}, DecisionReasonWorkerHostUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gate := NewGlobalDispatchGate(NewStrictPriority(Config{Capacity: 3}))
			high := ProjectCandidate{ID: "higher", Priority: 1}
			first, ok, err := gate.TryAcquire(t.Context(), high, tt.request, time.Now())
			if err != nil || !ok {
				t.Fatalf("initial grant = %t %v", ok, err)
			}
			result, cancel, decision := gate.Submit(t.Context(), high, tt.request, time.Now(), nil)
			defer cancel()
			if decision.Reason != tt.want {
				t.Fatalf("reason = %q, want %q", decision.Reason, tt.want)
			}
			lower, ok, err := gate.TryAcquire(t.Context(), ProjectCandidate{ID: "lower", Priority: 4}, SlotRequest{State: "Todo"}, time.Now())
			if err != nil || !ok {
				t.Fatalf("lower blocked by unstartable request = %t %v", ok, err)
			}
			if err := gate.Release(first); err != nil {
				t.Fatal(err)
			}
			select {
			case grant := <-result:
				if grant.Err != nil {
					t.Fatal(grant.Err)
				}
				if err := gate.Release(grant.Slot); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("queued request not granted after its ceiling clears")
			}
			if err := gate.Release(lower); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestQueuedDispatchCancellationAndReconfigure(t *testing.T) {
	for _, cancelBefore := range []bool{false, true} {
		t.Run(map[bool]string{false: "grant", true: "cancel"}[cancelBefore], func(t *testing.T) {
			gate := NewGlobalDispatchGate(NewStrictPriority(Config{Capacity: 1}))
			held, ok, err := gate.TryAcquire(t.Context(), ProjectCandidate{ID: "running"}, SlotRequest{State: "Todo"}, time.Now())
			if err != nil || !ok {
				t.Fatalf("grant = %t %v", ok, err)
			}
			result, cancel, _ := gate.Submit(t.Context(), ProjectCandidate{ID: "waiting"}, SlotRequest{State: "Todo"}, time.Now(), nil)
			defer cancel()
			if cancelBefore {
				cancel()
			}
			if err := gate.Reconfigure(Config{Capacity: 2}); err != nil {
				t.Fatal(err)
			}
			select {
			case grant := <-result:
				if cancelBefore || grant.Err != nil {
					t.Fatalf("unexpected result: %+v", grant)
				}
				if err := gate.Release(grant.Slot); err != nil {
					t.Fatal(err)
				}
			default:
				if !cancelBefore {
					t.Fatal("capacity increase left queued work idle")
				}
			}
			if err := gate.Release(held); err != nil {
				t.Fatal(err)
			}
			if got := gate.PoolSnapshot().Used; got != 0 {
				t.Fatalf("leaked capacity: %d", got)
			}
		})
	}
}

func TestQueuedDispatchSharedPoolRelease(t *testing.T) {
	r, err := NewPoolRegistry([]PoolConfig{
		{Name: DefaultPoolName, BurstTo: 2, Scheduler: Config{Kind: "strict", Capacity: 1}},
		{Name: "other", BurstTo: 2, Scheduler: Config{Kind: "strict", Capacity: 1}},
	}, []ProjectCandidate{{ID: "holder"}, {ID: "waiting", Pool: "other"}})
	if err != nil {
		t.Fatal(err)
	}
	held, ok, err := r.TryAcquire(t.Context(), ProjectCandidate{ID: "holder"}, SlotRequest{State: "Todo", Weight: 2}, time.Now())
	if err != nil || !ok {
		t.Fatalf("initial grant = %t %v", ok, err)
	}
	result, cancel, decision := r.GateFor("waiting").(QueuedProjectDispatchGate).Submit(t.Context(), ProjectCandidate{ID: "waiting", Pool: "other"}, SlotRequest{State: "Todo"}, time.Now(), nil)
	defer cancel()
	if decision.Reason != DispatchGateReasonGlobalCapacityFull {
		t.Fatalf("refusal = %+v", decision)
	}
	if err := r.Release(held); err != nil {
		t.Fatal(err)
	}
	select {
	case grant := <-result:
		if grant.Err != nil || grant.Slot.poolName != "other" {
			t.Fatalf("grant = %+v", grant)
		}
		if err := r.Release(grant.Slot); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("shared capacity release left other pool idle")
	}
}

func TestQueuedDispatchCapacityChanges(t *testing.T) {
	for _, kind := range []string{"gate unpause", "registry unpause", "registry increase", "remove project", "idle project", "cancel context"} {
		t.Run(kind, func(t *testing.T) {
			project := ProjectCandidate{ID: "waiting"}
			global := NewGlobalDispatchGate(NewStrictPriority(Config{Capacity: 1}), project)
			var gate ProjectDispatchGate = global
			var registry *PoolRegistry
			if kind == "registry unpause" || kind == "registry increase" {
				var err error
				registry, err = NewPoolRegistry([]PoolConfig{{Name: DefaultPoolName, Scheduler: Config{Kind: "strict", Capacity: 1}}}, []ProjectCandidate{project})
				if err != nil {
					t.Fatal(err)
				}
				gate = registry
			}
			var held Slot
			var resume func()
			switch kind {
			case "gate unpause":
				resume = global.PauseDispatch()
			case "registry unpause":
				resume = registry.PauseDispatch()
			default:
				var ok bool
				var err error
				held, ok, err = gate.TryAcquire(t.Context(), ProjectCandidate{ID: "holder"}, SlotRequest{State: "Todo"}, time.Now())
				if err != nil || !ok {
					t.Fatalf("initial grant = %t %v", ok, err)
				}
			}
			ctx, cancelContext := context.WithCancel(t.Context())
			defer cancelContext()
			result, cancel, _ := gate.(QueuedProjectDispatchGate).Submit(ctx, project, SlotRequest{State: "Todo"}, time.Now(), make(chan struct{}, 1))
			defer cancel()
			wantError := false
			switch kind {
			case "gate unpause", "registry unpause":
				resume()
			case "registry increase":
				if err := registry.Reconfigure([]PoolConfig{{Name: DefaultPoolName, Scheduler: Config{Kind: "strict", Capacity: 2}}}, []ProjectCandidate{project}); err != nil {
					t.Fatal(err)
				}
			case "remove project":
				global.SetProjects(nil)
				wantError = true
			case "idle project":
				global.MarkIdle(project)
				wantError = true
			case "cancel context":
				cancelContext()
				wantError = true
			}
			if held != (Slot{}) {
				if err := gate.Release(held); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case grant := <-result:
				if (grant.Err != nil) != wantError {
					t.Fatalf("grant = %+v, want error %t", grant, wantError)
				}
				if grant.Slot != (Slot{}) {
					if err := gate.Release(grant.Slot); err != nil {
						t.Fatal(err)
					}
				}
			default:
				t.Fatal("pending owner was not notified")
			}
		})
	}
}
