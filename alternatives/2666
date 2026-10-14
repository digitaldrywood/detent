package scheduler

func effectivePoolBurst(cfg PoolConfig) int {
	if cfg.BurstTo >= cfg.Scheduler.Capacity {
		return cfg.BurstTo
	}
	return cfg.Scheduler.Capacity
}

func reconfigurePoolRuntime(runtime *poolRuntime, cfg PoolConfig) error {
	schedulerConfig := cfg.Scheduler
	schedulerConfig.Capacity = effectivePoolBurst(cfg)
	if err := runtime.gate.Reconfigure(schedulerConfig); err != nil {
		return err
	}
	runtime.guaranteed = cfg.Scheduler.Capacity
	runtime.burstTo = schedulerConfig.Capacity
	return nil
}

func (r *PoolRegistry) setCapacityAdmission(runtime *poolRuntime) {
	runtime.gate.admit = &poolCapacityAdmission{
		allow: func(currentUsed int, projectedUsed int, req SlotRequest) string {
			return r.allowPoolCapacity(runtime, currentUsed, projectedUsed, req)
		},
	}
}

func (r *PoolRegistry) allowPoolCapacity(
	runtime *poolRuntime,
	currentUsed int,
	projectedUsed int,
	req SlotRequest,
) string {
	if req.PressureCapacity > 0 {
		currentPressureUsed := r.pressureUsed(runtime, currentUsed)
		if currentPressureUsed+req.Weight > req.PressureCapacity {
			return DispatchGateReasonPressureCapacityFull
		}
	}
	sharedCapacity, sharedUsed := r.elasticTotals(runtime, projectedUsed)
	if projectedUsed > runtime.burstTo || sharedUsed > sharedCapacity {
		return DispatchGateReasonGlobalCapacityFull
	}
	return ""
}

func (r *PoolRegistry) elasticTotals(current *poolRuntime, projectedUsed int) (int, int) {
	r.mu.RLock()
	active := make([]*poolRuntime, 0, len(r.active))
	for _, runtime := range r.active {
		active = append(active, runtime)
	}
	r.mu.RUnlock()

	capacity := 0
	for _, runtime := range active {
		capacity += runtime.guaranteed
	}
	used := 0
	for _, runtime := range active {
		if runtime == current {
			used += projectedUsed
			continue
		}
		used += runtime.gate.PoolSnapshot().Used
	}
	return capacity, used
}

func (r *PoolRegistry) pressureUsed(current *poolRuntime, currentUsed int) int {
	r.mu.RLock()
	runtimes := make([]*poolRuntime, 0, len(r.byGeneration))
	for _, runtime := range r.byGeneration {
		runtimes = append(runtimes, runtime)
	}
	r.mu.RUnlock()

	used := 0
	for _, runtime := range runtimes {
		if runtime == current {
			used += currentUsed
			continue
		}
		used += runtime.gate.PoolSnapshot().Used
	}
	return used
}

func (r *PoolRegistry) elasticPoolSnapshot(runtime *poolRuntime) PoolSnapshot {
	snapshot := runtime.gate.PoolSnapshot()
	sharedCapacity, sharedUsed := r.elasticTotals(nil, 0)
	snapshot.Capacity = runtime.burstTo
	snapshot.Guaranteed = runtime.guaranteed
	snapshot.BurstTo = runtime.burstTo
	snapshot.Borrowed = max(0, snapshot.Used-runtime.guaranteed)
	snapshot.Available = min(
		max(0, runtime.burstTo-snapshot.Used),
		max(0, sharedCapacity-sharedUsed),
	)
	snapshot.Draining = snapshot.Draining || runtime.retired
	if runtime.retired {
		snapshot.Available = 0
	}
	snapshot.Generation = runtime.generation
	return snapshot
}

func (r *PoolRegistry) elasticDecision(
	runtime *poolRuntime,
	decision DispatchGateDecision,
) DispatchGateDecision {
	snapshot := r.elasticPoolSnapshot(runtime)
	sharedCapacity, sharedUsed := r.elasticTotals(nil, 0)
	decision.GlobalCapacity = snapshot.Capacity
	decision.GlobalUsed = snapshot.Used
	decision.GlobalAvailable = snapshot.Available
	decision.GuaranteedCapacity = snapshot.Guaranteed
	decision.BurstCapacity = snapshot.BurstTo
	decision.BorrowedSlots = snapshot.Borrowed
	decision.SharedCapacity = sharedCapacity
	decision.SharedUsed = sharedUsed
	decision.SharedAvailable = max(0, sharedCapacity-sharedUsed)
	decision.PressureUsed = r.pressureUsed(nil, 0)
	decision.PressureAvailable = max(0, decision.PressureCapacity-decision.PressureUsed)
	return decision
}
