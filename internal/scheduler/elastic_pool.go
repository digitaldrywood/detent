package scheduler

type elasticPoolState struct {
	reclaimTargets map[uint64]int
	borrowers      []uint64
}

func newElasticPoolState() elasticPoolState {
	return elasticPoolState{reclaimTargets: map[uint64]int{}}
}

func (s *elasticPoolState) remove(generation uint64) {
	delete(s.reclaimTargets, generation)
	for index := 0; index < len(s.borrowers); {
		if s.borrowers[index] == generation {
			s.borrowers = append(s.borrowers[:index], s.borrowers[index+1:]...)
			continue
		}
		index++
	}
}

func (s *elasticPoolState) enqueue(generation uint64) {
	for _, current := range s.borrowers {
		if current == generation {
			return
		}
	}
	s.borrowers = append(s.borrowers, generation)
}

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
		allow: func(currentUsed int, projectedUsed int) bool {
			return r.allowPoolCapacity(runtime, currentUsed, projectedUsed)
		},
		complete: func(used int) {
			r.completePoolAdmission(runtime, used)
		},
	}
}

func (r *PoolRegistry) allowPoolCapacity(
	runtime *poolRuntime,
	currentUsed int,
	projectedUsed int,
) bool {
	sharedCapacity, sharedUsed := r.elasticTotals(runtime, projectedUsed)
	if projectedUsed <= runtime.guaranteed {
		if sharedUsed <= sharedCapacity {
			return true
		}
		target := min(runtime.guaranteed, projectedUsed)
		if target > currentUsed {
			r.elastic.reclaimTargets[runtime.generation] = max(
				r.elastic.reclaimTargets[runtime.generation],
				target,
			)
		}
		return false
	}
	if projectedUsed > runtime.burstTo || runtime.burstTo == runtime.guaranteed {
		return false
	}
	if r.hasReclaimDemand(runtime, currentUsed) ||
		r.hasGuaranteedReadyDemand(runtime) ||
		sharedUsed > sharedCapacity {
		r.elastic.enqueue(runtime.generation)
		return false
	}
	if len(r.elastic.borrowers) > 0 && r.elastic.borrowers[0] != runtime.generation {
		r.elastic.enqueue(runtime.generation)
		return false
	}
	return true
}

func (r *PoolRegistry) completePoolAdmission(runtime *poolRuntime, used int) {
	if target := r.elastic.reclaimTargets[runtime.generation]; target > 0 && used >= target {
		delete(r.elastic.reclaimTargets, runtime.generation)
	}
	r.elastic.removeBorrower(runtime.generation)
}

func (s *elasticPoolState) removeBorrower(generation uint64) {
	for index, current := range s.borrowers {
		if current != generation {
			continue
		}
		s.borrowers = append(s.borrowers[:index], s.borrowers[index+1:]...)
		return
	}
}

func (r *PoolRegistry) hasReclaimDemand(current *poolRuntime, currentUsed int) bool {
	r.mu.RLock()
	runtimes := make(map[uint64]*poolRuntime, len(r.byGeneration))
	for generation, runtime := range r.byGeneration {
		runtimes[generation] = runtime
	}
	r.mu.RUnlock()

	pending := false
	for generation, target := range r.elastic.reclaimTargets {
		runtime := runtimes[generation]
		if runtime == nil || runtime.retired {
			delete(r.elastic.reclaimTargets, generation)
			continue
		}
		used := currentUsed
		if runtime != current {
			used = runtime.gate.PoolSnapshot().Used
		}
		if used >= target {
			delete(r.elastic.reclaimTargets, generation)
			continue
		}
		pending = true
	}
	return pending
}

func (r *PoolRegistry) hasGuaranteedReadyDemand(current *poolRuntime) bool {
	r.mu.RLock()
	active := make([]*poolRuntime, 0, len(r.active))
	for _, runtime := range r.active {
		active = append(active, runtime)
	}
	r.mu.RUnlock()

	for _, runtime := range active {
		if runtime == current {
			continue
		}
		used := runtime.gate.PoolSnapshot().Used
		if used < runtime.guaranteed && runtime.gate.hasReadyProjects() {
			return true
		}
	}
	return false
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

func (r *PoolRegistry) cleanupElasticWaiters() {
	r.mu.RLock()
	runtimes := make(map[uint64]*poolRuntime, len(r.active))
	for _, runtime := range r.active {
		runtimes[runtime.generation] = runtime
	}
	r.mu.RUnlock()

	for generation, target := range r.elastic.reclaimTargets {
		runtime := runtimes[generation]
		if runtime == nil || !runtime.gate.hasReadyProjects() {
			delete(r.elastic.reclaimTargets, generation)
			continue
		}
		if runtime.gate.PoolSnapshot().Used >= min(target, runtime.guaranteed) {
			delete(r.elastic.reclaimTargets, generation)
		}
	}
	filtered := r.elastic.borrowers[:0]
	for _, generation := range r.elastic.borrowers {
		runtime := runtimes[generation]
		if runtime == nil || runtime.burstTo == runtime.guaranteed || !runtime.gate.hasReadyProjects() {
			continue
		}
		if runtime.gate.PoolSnapshot().Used < runtime.guaranteed {
			continue
		}
		filtered = append(filtered, generation)
	}
	r.elastic.borrowers = filtered
}

func (r *PoolRegistry) elasticPoolSnapshot(runtime *poolRuntime) PoolSnapshot {
	snapshot := runtime.gate.PoolSnapshot()
	sharedCapacity, sharedUsed := r.elasticTotals(nil, 0)
	reclaiming := r.hasReclaimDemand(nil, 0) || r.hasGuaranteedReadyDemand(nil)
	snapshot.Capacity = runtime.burstTo
	snapshot.Guaranteed = runtime.guaranteed
	snapshot.BurstTo = runtime.burstTo
	snapshot.Borrowed = max(0, snapshot.Used-runtime.guaranteed)
	snapshot.Available = min(
		max(0, runtime.burstTo-snapshot.Used),
		max(0, sharedCapacity-sharedUsed),
	)
	if reclaiming && snapshot.Used >= runtime.guaranteed {
		snapshot.Available = 0
	}
	snapshot.Draining = snapshot.Draining || runtime.retired
	if runtime.retired {
		snapshot.Available = 0
	}
	snapshot.Reclaiming = reclaiming && snapshot.Borrowed > 0
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
	return decision
}
