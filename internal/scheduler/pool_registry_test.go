package scheduler_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestPoolRegistryGrantsIndependentCapacity(t *testing.T) {
	t.Parallel()

	registry := newPoolRegistry(t,
		[]scheduler.PoolConfig{
			poolConfig(scheduler.DefaultPoolName, "round_robin", 1, nil),
			poolConfig("video", "round_robin", 2, nil),
		},
		[]scheduler.ProjectCandidate{
			{ID: "code", Pool: scheduler.DefaultPoolName},
			{ID: "video-a", Pool: "video"},
			{ID: "video-b", Pool: "video"},
		},
	)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	slots := acquirePoolSlots(t, registry, now,
		scheduler.ProjectCandidate{ID: "code"},
		scheduler.ProjectCandidate{ID: "video-a"},
		scheduler.ProjectCandidate{ID: "video-b"},
	)
	if snapshots := registry.PoolSnapshots(); len(snapshots) != 2 ||
		snapshots[0].Used+snapshots[1].Used != 3 {
		t.Fatalf("PoolSnapshots() = %#v, want three slots used across two pools", snapshots)
	}
	releasePoolSlots(t, registry, slots)
}

func TestPoolRegistryWithoutBurstRemainsRigid(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		burstTo int
	}{
		{name: "omitted"},
		{name: "equal to guarantee", burstTo: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			videoPool := poolConfig("video", "round_robin", 1, nil)
			videoPool.BurstTo = test.burstTo
			registry := newPoolRegistry(t,
				[]scheduler.PoolConfig{
					poolConfig(scheduler.DefaultPoolName, "round_robin", 1, nil),
					videoPool,
				},
				[]scheduler.ProjectCandidate{
					{ID: "video-a", Pool: "video"},
					{ID: "video-b", Pool: "video"},
				},
			)
			slot := acquirePoolSlots(t, registry, time.Time{}, scheduler.ProjectCandidate{ID: "video-a", Pool: "video"})[0]
			assertPoolUnavailable(t, registry, scheduler.ProjectCandidate{ID: "video-b", Pool: "video"})
			snapshot := registry.PoolSnapshotFor("video-a")
			if snapshot.Capacity != 1 || snapshot.Guaranteed != 1 || snapshot.BurstTo != 1 ||
				snapshot.Borrowed != 0 || snapshot.Used != 1 {
				t.Fatalf("PoolSnapshotFor(video-a) = %#v, want rigid 1-slot pool", snapshot)
			}
			releasePoolSlots(t, registry, []scheduler.Slot{slot})
		})
	}
}

func TestPoolRegistryBorrowsIdleGuaranteesAndReclaimsByAttrition(t *testing.T) {
	t.Parallel()

	projects := []scheduler.ProjectCandidate{
		{ID: "code-a"},
		{ID: "code-b"},
		{ID: "video-a", Pool: "video"},
		{ID: "video-b", Pool: "video"},
		{ID: "video-c", Pool: "video"},
		{ID: "video-d", Pool: "video"},
	}
	registry := newPoolRegistry(t,
		[]scheduler.PoolConfig{
			poolConfig(scheduler.DefaultPoolName, "round_robin", 2, nil),
			elasticPoolConfig("video", "round_robin", 1, 3, nil),
		},
		projects,
	)
	videoSlots := acquirePoolSlots(t, registry, time.Time{}, projects[2], projects[3], projects[4])
	snapshot := registry.PoolSnapshotFor("video-a")
	if snapshot.Capacity != 3 || snapshot.Guaranteed != 1 || snapshot.BurstTo != 3 ||
		snapshot.Used != 3 || snapshot.Borrowed != 2 || snapshot.Available != 0 {
		t.Fatalf("PoolSnapshotFor(video-a) = %#v, want 1 guaranteed and 2 borrowed", snapshot)
	}
	_, acquired, decision, err := registry.TryAcquireWithDecision(
		context.Background(),
		projects[5],
		scheduler.SlotRequest{State: "Todo"},
		time.Time{},
	)
	if err != nil {
		t.Fatalf("TryAcquireWithDecision(video-d) error = %v", err)
	}
	if acquired || decision.GuaranteedCapacity != 1 || decision.BurstCapacity != 3 ||
		decision.BorrowedSlots != 2 || decision.SharedCapacity != 3 ||
		decision.SharedUsed != 3 || decision.SharedAvailable != 0 {
		t.Fatalf("TryAcquireWithDecision(video-d) decision = %#v, want elastic capacity telemetry", decision)
	}
	assertPoolUnavailable(t, registry, projects[0])
	if snapshot := registry.PoolSnapshotFor("video-a"); !snapshot.Reclaiming {
		t.Fatalf("PoolSnapshotFor(video-a) = %#v, want reclaiming", snapshot)
	}

	if err := registry.Release(videoSlots[0]); err != nil {
		t.Fatalf("Release(video-a) error = %v", err)
	}
	assertPoolUnavailable(t, registry, projects[5])
	codeSlots := acquirePoolSlots(t, registry, time.Time{}, projects[0])
	assertPoolUnavailable(t, registry, projects[1])
	if err := registry.Release(videoSlots[1]); err != nil {
		t.Fatalf("Release(video-b) error = %v", err)
	}
	codeSlots = append(codeSlots, acquirePoolSlots(t, registry, time.Time{}, projects[1])...)
	snapshot = registry.PoolSnapshotFor("code-a")
	if snapshot.Used != 2 || snapshot.Guaranteed != 2 || snapshot.Borrowed != 0 {
		t.Fatalf("PoolSnapshotFor(code-a) = %#v, want full guarantee", snapshot)
	}
	releasePoolSlots(t, registry, append(codeSlots, videoSlots[2]))
}

func TestPoolRegistryReservesGuaranteeForReadyLender(t *testing.T) {
	t.Parallel()

	code := scheduler.ProjectCandidate{ID: "code"}
	videoA := scheduler.ProjectCandidate{ID: "video-a", Pool: "video"}
	videoB := scheduler.ProjectCandidate{ID: "video-b", Pool: "video"}
	registry := newPoolRegistry(t,
		[]scheduler.PoolConfig{
			poolConfig(scheduler.DefaultPoolName, "round_robin", 1, nil),
			elasticPoolConfig("video", "round_robin", 1, 2, nil),
		},
		[]scheduler.ProjectCandidate{code, videoA, videoB},
	)
	videoSlot := acquirePoolSlots(t, registry, time.Time{}, videoA)[0]
	registry.MarkReady(code)
	assertPoolUnavailable(t, registry, videoB)
	codeSlot := acquirePoolSlots(t, registry, time.Time{}, code)[0]
	if snapshot := registry.PoolSnapshotFor("video-a"); snapshot.Borrowed != 0 {
		t.Fatalf("PoolSnapshotFor(video-a) = %#v, want no borrowed slot", snapshot)
	}
	releasePoolSlots(t, registry, []scheduler.Slot{videoSlot, codeSlot})
}

func TestPoolRegistryBorrowerContentionIsFirstCome(t *testing.T) {
	t.Parallel()

	projects := []scheduler.ProjectCandidate{
		{ID: "lender"},
		{ID: "alpha-a", Pool: "alpha"},
		{ID: "alpha-b", Pool: "alpha"},
		{ID: "beta-a", Pool: "beta"},
		{ID: "beta-b", Pool: "beta"},
	}
	registry := newPoolRegistry(t,
		[]scheduler.PoolConfig{
			poolConfig(scheduler.DefaultPoolName, "round_robin", 1, nil),
			elasticPoolConfig("alpha", "round_robin", 1, 2, nil),
			elasticPoolConfig("beta", "round_robin", 1, 2, nil),
		},
		projects,
	)
	slots := acquirePoolSlots(t, registry, time.Time{}, projects[0], projects[1], projects[3])
	assertPoolUnavailable(t, registry, projects[2])
	assertPoolUnavailable(t, registry, projects[4])
	if err := registry.Release(slots[0]); err != nil {
		t.Fatalf("Release(lender) error = %v", err)
	}
	assertPoolUnavailable(t, registry, projects[4])
	alphaBorrowed := acquirePoolSlots(t, registry, time.Time{}, projects[2])[0]
	if err := registry.Release(alphaBorrowed); err != nil {
		t.Fatalf("Release(alpha borrowed) error = %v", err)
	}
	betaBorrowed := acquirePoolSlots(t, registry, time.Time{}, projects[4])[0]
	releasePoolSlots(t, registry, []scheduler.Slot{slots[1], slots[2], betaBorrowed})
}

func TestPoolRegistryElasticStrictPreemptionDoesNotCrossPools(t *testing.T) {
	t.Parallel()

	projects := []scheduler.ProjectCandidate{
		{ID: "code-urgent", Priority: 0},
		{ID: "video-a", Pool: "video", Priority: 4},
		{ID: "video-b", Pool: "video", Priority: 4},
	}
	registry := newPoolRegistry(t,
		[]scheduler.PoolConfig{
			poolConfig(scheduler.DefaultPoolName, "strict", 1, nil),
			elasticPoolConfig("video", "strict", 1, 2, nil),
		},
		projects,
	)
	registry.MarkIdle("code-urgent")
	videoSlots := acquirePoolSlots(t, registry, time.Time{}, projects[1], projects[2])
	videoPreemptions := 0
	for _, slot := range videoSlots {
		registry.SetPreempt(slot, func() { videoPreemptions++ })
	}
	assertPoolUnavailable(t, registry, projects[0])
	if videoPreemptions != 0 {
		t.Fatalf("video preemptions = %d, want 0", videoPreemptions)
	}
	if err := registry.Release(videoSlots[0]); err != nil {
		t.Fatalf("Release(video-a) error = %v", err)
	}
	codeSlot := acquirePoolSlots(t, registry, time.Time{}, projects[0])[0]
	if videoPreemptions != 0 {
		t.Fatalf("video preemptions after code grant = %d, want 0", videoPreemptions)
	}
	releasePoolSlots(t, registry, []scheduler.Slot{videoSlots[1], codeSlot})
}

func TestPoolRegistryReconfiguresBurstWhileRunning(t *testing.T) {
	t.Parallel()

	projects := []scheduler.ProjectCandidate{
		{ID: "video-a", Pool: "video"},
		{ID: "video-b", Pool: "video"},
		{ID: "video-c", Pool: "video"},
		{ID: "video-d", Pool: "video"},
	}
	defaultPool := poolConfig(scheduler.DefaultPoolName, "weighted", 2, nil)
	rigidVideo := poolConfig("video", "weighted", 1, nil)
	registry := newPoolRegistry(t, []scheduler.PoolConfig{defaultPool, rigidVideo}, projects)
	slots := acquirePoolSlots(t, registry, time.Time{}, projects[0])

	elasticVideo := elasticPoolConfig("video", "weighted", 1, 3, nil)
	if err := registry.Reconfigure([]scheduler.PoolConfig{defaultPool, elasticVideo}, projects); err != nil {
		t.Fatalf("add burst Reconfigure() error = %v", err)
	}
	slots = append(slots, acquirePoolSlots(t, registry, time.Time{}, projects[1], projects[2])...)

	elasticVideo.BurstTo = 2
	if err := registry.Reconfigure([]scheduler.PoolConfig{defaultPool, elasticVideo}, projects); err != nil {
		t.Fatalf("change burst Reconfigure() error = %v", err)
	}
	assertPoolUnavailable(t, registry, projects[3])
	if err := registry.Release(slots[0]); err != nil {
		t.Fatalf("Release(first) error = %v", err)
	}
	assertPoolUnavailable(t, registry, projects[3])

	if err := registry.Reconfigure([]scheduler.PoolConfig{defaultPool, rigidVideo}, projects); err != nil {
		t.Fatalf("remove burst Reconfigure() error = %v", err)
	}
	if snapshot := registry.PoolSnapshotFor("video-b"); snapshot.Capacity != 1 ||
		snapshot.Guaranteed != 1 || snapshot.Used != 2 {
		t.Fatalf("PoolSnapshotFor(video-b) = %#v, want rigid pool draining two slots", snapshot)
	}
	if err := registry.Release(slots[1]); err != nil {
		t.Fatalf("Release(second) error = %v", err)
	}
	assertPoolUnavailable(t, registry, projects[3])
	if err := registry.Release(slots[2]); err != nil {
		t.Fatalf("Release(third) error = %v", err)
	}
	replacement := acquirePoolSlots(t, registry, time.Time{}, projects[3])[0]
	releasePoolSlots(t, registry, []scheduler.Slot{replacement})
}

func TestPoolRegistryScopesSchedulingState(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"weighted", "round_robin"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			registry := newPoolRegistry(t,
				[]scheduler.PoolConfig{
					poolConfig(scheduler.DefaultPoolName, mode, 1, nil),
					poolConfig("video", mode, 1, nil),
				},
				[]scheduler.ProjectCandidate{
					{ID: "alpha", Pool: scheduler.DefaultPoolName},
					{ID: "zulu", Pool: "video"},
				},
			)
			registry.MarkReady(scheduler.ProjectCandidate{ID: "zulu", Pool: "video"})
			slot, ok, decision, err := registry.TryAcquireWithDecision(
				context.Background(),
				scheduler.ProjectCandidate{ID: "alpha"},
				scheduler.SlotRequest{State: "Todo"},
				time.Time{},
			)
			if err != nil {
				t.Fatalf("TryAcquireWithDecision() error = %v", err)
			}
			if !ok || decision.PoolName != scheduler.DefaultPoolName {
				t.Fatalf("TryAcquireWithDecision() ok = %t decision = %#v, want default pool grant", ok, decision)
			}
			if err := registry.Release(slot); err != nil {
				t.Fatalf("Release() error = %v", err)
			}
		})
	}
}

func TestPoolRegistryStrictPreemptionDoesNotCrossPools(t *testing.T) {
	t.Parallel()

	registry := newPoolRegistry(t,
		[]scheduler.PoolConfig{
			poolConfig(scheduler.DefaultPoolName, "strict", 1, nil),
			poolConfig("video", "strict", 1, nil),
		},
		[]scheduler.ProjectCandidate{
			{ID: "code-low", Priority: 4},
			{ID: "code-urgent", Priority: 0},
			{ID: "video-low", Pool: "video", Priority: 4},
		},
	)
	registry.MarkIdle("code-urgent")
	codeSlot := acquirePoolSlots(t, registry, time.Time{}, scheduler.ProjectCandidate{ID: "code-low", Priority: 4})[0]
	videoSlot := acquirePoolSlots(t, registry, time.Time{}, scheduler.ProjectCandidate{ID: "video-low", Priority: 4})[0]
	codePreemptions := 0
	videoPreemptions := 0
	registry.SetPreempt(codeSlot, func() { codePreemptions++ })
	registry.SetPreempt(videoSlot, func() { videoPreemptions++ })

	urgentSlot := acquirePoolSlots(t, registry, time.Time{}, scheduler.ProjectCandidate{ID: "code-urgent", Priority: 0})[0]
	if codePreemptions != 1 || videoPreemptions != 0 {
		t.Fatalf("preemptions code/video = %d/%d, want 1/0", codePreemptions, videoPreemptions)
	}
	releasePoolSlots(t, registry, []scheduler.Slot{urgentSlot, videoSlot})
}

func TestPoolRegistryFairShareIgnoresOtherPoolHistory(t *testing.T) {
	t.Parallel()

	usage := &fairShareStore{usage: []store.FairShareUsage{
		{ProjectID: "alpha", Dispatches: 5},
		{ProjectID: "beta", Dispatches: 10},
		{ProjectID: "video", Dispatches: 0},
	}}
	registry := newPoolRegistry(t,
		[]scheduler.PoolConfig{
			poolConfig(scheduler.DefaultPoolName, "fair_share", 1, usage),
			poolConfig("video", "fair_share", 1, usage),
		},
		[]scheduler.ProjectCandidate{
			{ID: "alpha"},
			{ID: "beta"},
			{ID: "video", Pool: "video"},
		},
	)
	registry.MarkReady(scheduler.ProjectCandidate{ID: "alpha"})
	registry.MarkReady(scheduler.ProjectCandidate{ID: "beta"})
	registry.MarkReady(scheduler.ProjectCandidate{ID: "video", Pool: "video"})

	slot, ok, decision, err := registry.TryAcquireWithDecision(
		context.Background(),
		scheduler.ProjectCandidate{ID: "alpha"},
		scheduler.SlotRequest{State: "Todo"},
		time.Time{},
	)
	if err != nil {
		t.Fatalf("TryAcquireWithDecision() error = %v", err)
	}
	if !ok || decision.SelectedProjectID != "alpha" {
		t.Fatalf("TryAcquireWithDecision() ok = %t decision = %#v, want alpha", ok, decision)
	}
	if err := registry.Release(slot); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
}

func TestPoolRegistryReconfigureDrainsLoweredCapacity(t *testing.T) {
	t.Parallel()

	projects := []scheduler.ProjectCandidate{{ID: "video-a", Pool: "video"}, {ID: "video-b", Pool: "video"}}
	registry := newPoolRegistry(t,
		[]scheduler.PoolConfig{
			poolConfig(scheduler.DefaultPoolName, "weighted", 1, nil),
			poolConfig("video", "weighted", 2, nil),
		},
		projects,
	)
	slots := acquirePoolSlots(t, registry, time.Time{}, projects...)
	if err := registry.Reconfigure([]scheduler.PoolConfig{
		poolConfig(scheduler.DefaultPoolName, "weighted", 1, nil),
		poolConfig("video", "weighted", 1, nil),
	}, projects); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	assertPoolUnavailable(t, registry, projects[0])
	if snapshot := registry.PoolSnapshotFor("video-a"); snapshot.Capacity != 1 || snapshot.Used != 2 {
		t.Fatalf("PoolSnapshotFor(video-a) = %#v, want capacity 1 used 2", snapshot)
	}
	if err := registry.Release(slots[0]); err != nil {
		t.Fatalf("Release(first) error = %v", err)
	}
	assertPoolUnavailable(t, registry, projects[0])
	if err := registry.Release(slots[1]); err != nil {
		t.Fatalf("Release(second) error = %v", err)
	}
	slot := acquirePoolSlots(t, registry, time.Time{}, projects[0])[0]
	releasePoolSlots(t, registry, []scheduler.Slot{slot})
}

func TestPoolRegistryRemovedPoolDrainsAcrossReplacementGeneration(t *testing.T) {
	t.Parallel()

	video := scheduler.ProjectCandidate{ID: "video", Pool: "video"}
	registry := newPoolRegistry(t,
		[]scheduler.PoolConfig{
			poolConfig(scheduler.DefaultPoolName, "weighted", 1, nil),
			poolConfig("video", "weighted", 1, nil),
		},
		[]scheduler.ProjectCandidate{video},
	)
	oldSlot := acquirePoolSlots(t, registry, time.Time{}, video)[0]

	reassigned := scheduler.ProjectCandidate{ID: "video"}
	if err := registry.Reconfigure(
		[]scheduler.PoolConfig{poolConfig(scheduler.DefaultPoolName, "weighted", 1, nil)},
		[]scheduler.ProjectCandidate{reassigned},
	); err != nil {
		t.Fatalf("remove Reconfigure() error = %v", err)
	}
	defaultSlot := acquirePoolSlots(t, registry, time.Time{}, reassigned)[0]
	if snapshots := registry.PoolSnapshots(); len(snapshots) != 2 || !snapshots[1].Draining {
		t.Fatalf("PoolSnapshots() after removal = %#v, want active default and draining video", snapshots)
	}

	if err := registry.Reconfigure(
		[]scheduler.PoolConfig{
			poolConfig(scheduler.DefaultPoolName, "weighted", 1, nil),
			poolConfig("video", "weighted", 1, nil),
		},
		[]scheduler.ProjectCandidate{video},
	); err != nil {
		t.Fatalf("re-add Reconfigure() error = %v", err)
	}
	newSlot := acquirePoolSlots(t, registry, time.Time{}, video)[0]
	if snapshots := registry.PoolSnapshots(); len(snapshots) != 3 {
		t.Fatalf("PoolSnapshots() with replacement = %#v, want three pool generations", snapshots)
	}
	if err := registry.Release(oldSlot); err != nil {
		t.Fatalf("Release(oldSlot) error = %v", err)
	}
	if snapshots := registry.PoolSnapshots(); len(snapshots) != 2 {
		t.Fatalf("PoolSnapshots() after drain = %#v, want two active pools", snapshots)
	}
	releasePoolSlots(t, registry, []scheduler.Slot{defaultSlot, newSlot})
}

func TestPoolRegistryPauseIncludesPoolsAddedWhilePaused(t *testing.T) {
	t.Parallel()

	registry := newPoolRegistry(t,
		[]scheduler.PoolConfig{poolConfig(scheduler.DefaultPoolName, "weighted", 1, nil)},
		[]scheduler.ProjectCandidate{{ID: "code"}},
	)
	resume := registry.PauseDispatch()
	if err := registry.Reconfigure(
		[]scheduler.PoolConfig{
			poolConfig(scheduler.DefaultPoolName, "weighted", 1, nil),
			poolConfig("video", "weighted", 1, nil),
		},
		[]scheduler.ProjectCandidate{{ID: "code"}, {ID: "video", Pool: "video"}},
	); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	assertPoolPaused(t, registry, scheduler.ProjectCandidate{ID: "code"})
	assertPoolPaused(t, registry, scheduler.ProjectCandidate{ID: "video"})
	resume()
	resume()
	slots := acquirePoolSlots(t, registry, time.Time{},
		scheduler.ProjectCandidate{ID: "code"},
		scheduler.ProjectCandidate{ID: "video"},
	)
	releasePoolSlots(t, registry, slots)
}

func TestPoolRegistryUsesDefaultPoolForLegacyProjects(t *testing.T) {
	t.Parallel()

	registry := newPoolRegistry(t,
		[]scheduler.PoolConfig{poolConfig(scheduler.DefaultPoolName, "strict", 1, nil)},
		[]scheduler.ProjectCandidate{{ID: "legacy"}},
	)
	gate := registry.GateFor("legacy")
	detailed, ok := gate.(interface {
		TryAcquireWithDecision(context.Context, scheduler.ProjectCandidate, scheduler.SlotRequest, time.Time) (
			scheduler.Slot,
			bool,
			scheduler.DispatchGateDecision,
			error,
		)
	})
	if !ok {
		t.Fatalf("GateFor() type = %T, want detailed gate", gate)
	}
	slot, acquired, decision, err := detailed.TryAcquireWithDecision(
		context.Background(),
		scheduler.ProjectCandidate{},
		scheduler.SlotRequest{State: "Todo"},
		time.Time{},
	)
	if err != nil {
		t.Fatalf("TryAcquireWithDecision() error = %v", err)
	}
	if !acquired || decision.PoolName != scheduler.DefaultPoolName ||
		decision.GlobalCapacity != 1 || registry.PoolSnapshotFor("legacy").Mode != scheduler.ModeStrictPriority {
		t.Fatalf("legacy decision = %#v snapshot = %#v", decision, registry.PoolSnapshotFor("legacy"))
	}
	if err := gate.Release(slot); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
}

func TestProjectPoolGateRoutesLifecycleToCurrentPool(t *testing.T) {
	t.Parallel()

	project := scheduler.ProjectCandidate{ID: "video", Pool: "video"}
	registry := newPoolRegistry(t,
		[]scheduler.PoolConfig{
			poolConfig(scheduler.DefaultPoolName, "weighted", 1, nil),
			poolConfig("video", "weighted", 1, nil),
		},
		[]scheduler.ProjectCandidate{project},
	)
	gate := registry.GateFor(project.ID)
	snapshotter, ok := gate.(scheduler.PoolSnapshotter)
	if !ok {
		t.Fatalf("GateFor() type = %T, want PoolSnapshotter", gate)
	}
	if snapshot := snapshotter.PoolSnapshot(); snapshot.Name != "video" || snapshot.Capacity != 1 {
		t.Fatalf("PoolSnapshot() = %#v, want video capacity 1", snapshot)
	}
	cycleGate, ok := gate.(interface {
		BeginProjectCycle(scheduler.ProjectCandidate)
		EndProjectCycle(string)
	})
	if !ok {
		t.Fatalf("GateFor() type = %T, want project cycle gate", gate)
	}

	cycleGate.BeginProjectCycle(scheduler.ProjectCandidate{Priority: 2})
	gate.MarkReady(scheduler.ProjectCandidate{Priority: 2})
	cycleGate.EndProjectCycle("")
	slot, acquired, err := gate.TryAcquire(
		context.Background(),
		scheduler.ProjectCandidate{},
		scheduler.SlotRequest{State: "Todo"},
		time.Time{},
	)
	if err != nil {
		t.Fatalf("TryAcquire() error = %v", err)
	}
	if !acquired {
		t.Fatal("TryAcquire() acquired = false, want true")
	}
	if snapshot := snapshotter.PoolSnapshot(); !slices.Equal(snapshot.Holders, []string{"video"}) {
		t.Fatalf("PoolSnapshot().Holders = %#v, want video", snapshot.Holders)
	}
	gate.SetPreempt(slot, func() {})
	if err := gate.Release(slot); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if err := gate.Release(slot); err != nil {
		t.Fatalf("second Release() error = %v", err)
	}
	gate.MarkIdle("")

	registry.SetProjects([]scheduler.ProjectCandidate{{ID: project.ID}})
	if snapshot := snapshotter.PoolSnapshot(); snapshot.Name != scheduler.DefaultPoolName {
		t.Fatalf("PoolSnapshot() after reassignment = %#v, want default", snapshot)
	}
}

func TestPoolRegistryRejectsInvalidConfigurations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		pools []scheduler.PoolConfig
		want  string
	}{
		{name: "missing default", pools: []scheduler.PoolConfig{poolConfig("video", "weighted", 1, nil)}, want: `"default" is required`},
		{name: "blank name", pools: []scheduler.PoolConfig{{Scheduler: scheduler.Config{Capacity: 1}}}, want: "name is required"},
		{name: "duplicate", pools: []scheduler.PoolConfig{
			poolConfig(scheduler.DefaultPoolName, "weighted", 1, nil),
			poolConfig(scheduler.DefaultPoolName, "weighted", 1, nil),
		}, want: `"default" is duplicated`},
		{name: "nonpositive capacity", pools: []scheduler.PoolConfig{{Name: scheduler.DefaultPoolName}}, want: "capacity must be positive"},
		{name: "burst below guarantee", pools: []scheduler.PoolConfig{{
			Name: scheduler.DefaultPoolName, BurstTo: 1, Scheduler: scheduler.Config{Kind: "weighted", Capacity: 2},
		}}, want: "burst capacity must be greater than or equal to capacity"},
		{name: "unsupported scheduling", pools: []scheduler.PoolConfig{
			poolConfig(scheduler.DefaultPoolName, "unknown", 1, nil),
		}, want: "unsupported scheduler backend"},
		{name: "non-global scheduler", pools: []scheduler.PoolConfig{
			poolConfig(scheduler.DefaultPoolName, "counting_semaphore", 1, nil),
		}, want: "unsupported scheduler backend"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := scheduler.NewPoolRegistry(tt.pools, nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NewPoolRegistry() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestNilPoolRegistryConfigurationMethodsAreNoOps(t *testing.T) {
	t.Parallel()

	var registry *scheduler.PoolRegistry
	registry.SetProjects(nil)
	if err := registry.Reconfigure(nil, nil); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	resume := registry.PauseDispatch()
	resume()
	if snapshots := registry.PoolSnapshots(); snapshots != nil {
		t.Fatalf("PoolSnapshots() = %#v, want nil", snapshots)
	}
}

func newPoolRegistry(
	t *testing.T,
	pools []scheduler.PoolConfig,
	projects []scheduler.ProjectCandidate,
) *scheduler.PoolRegistry {
	t.Helper()
	registry, err := scheduler.NewPoolRegistry(pools, projects)
	if err != nil {
		t.Fatalf("NewPoolRegistry() error = %v", err)
	}
	return registry
}

func poolConfig(name string, kind string, capacity int, fairShare scheduler.FairShareStore) scheduler.PoolConfig {
	return scheduler.PoolConfig{
		Name: name,
		Scheduler: scheduler.Config{
			Kind:           kind,
			Capacity:       capacity,
			FairShareStore: fairShare,
		},
	}
}

func elasticPoolConfig(
	name string,
	kind string,
	capacity int,
	burstTo int,
	fairShare scheduler.FairShareStore,
) scheduler.PoolConfig {
	config := poolConfig(name, kind, capacity, fairShare)
	config.BurstTo = burstTo
	return config
}

func acquirePoolSlots(
	t *testing.T,
	registry *scheduler.PoolRegistry,
	now time.Time,
	projects ...scheduler.ProjectCandidate,
) []scheduler.Slot {
	t.Helper()
	slots := make([]scheduler.Slot, 0, len(projects))
	for _, project := range projects {
		slot, ok, decision, err := registry.TryAcquireWithDecision(
			context.Background(),
			project,
			scheduler.SlotRequest{State: "Todo"},
			now,
		)
		if err != nil {
			t.Fatalf("TryAcquireWithDecision(%s) error = %v", project.ID, err)
		}
		if !ok {
			t.Fatalf("TryAcquireWithDecision(%s) decision = %#v, want grant", project.ID, decision)
		}
		slots = append(slots, slot)
	}
	return slots
}

func releasePoolSlots(t *testing.T, registry *scheduler.PoolRegistry, slots []scheduler.Slot) {
	t.Helper()
	for _, slot := range slots {
		if err := registry.Release(slot); err != nil && !errors.Is(err, scheduler.ErrSlotNotHeld) {
			t.Fatalf("Release() error = %v", err)
		}
	}
}

func assertPoolUnavailable(t *testing.T, registry *scheduler.PoolRegistry, project scheduler.ProjectCandidate) {
	t.Helper()
	_, ok, decision, err := registry.TryAcquireWithDecision(
		context.Background(),
		project,
		scheduler.SlotRequest{State: "Todo"},
		time.Time{},
	)
	if err != nil {
		t.Fatalf("TryAcquireWithDecision() error = %v", err)
	}
	if ok || decision.Reason != scheduler.DispatchGateReasonGlobalCapacityFull {
		t.Fatalf("TryAcquireWithDecision() ok = %t decision = %#v, want capacity wait", ok, decision)
	}
}

func assertPoolPaused(t *testing.T, registry *scheduler.PoolRegistry, project scheduler.ProjectCandidate) {
	t.Helper()
	_, ok, decision, err := registry.TryAcquireWithDecision(
		context.Background(),
		project,
		scheduler.SlotRequest{State: "Todo"},
		time.Time{},
	)
	if err != nil {
		t.Fatalf("TryAcquireWithDecision() error = %v", err)
	}
	if ok || decision.Reason != scheduler.DispatchGateReasonPaused {
		t.Fatalf("TryAcquireWithDecision() ok = %t decision = %#v, want paused", ok, decision)
	}
}
