package tracker

import (
	"reflect"
	"testing"
	"time"
)

func TestDeriveDispatchQueueReasons(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	availableMachine := MachineAvailability{ID: "machine-a", Healthy: true, Capacity: 2}
	tests := []struct {
		name      string
		item      WorkItem
		snapshot  DispatchSnapshot
		wantCodes []DispatchReasonCode
	}{
		{
			name:     "all gates allow dispatch",
			item:     readyWorkItem(1, 1, "digitaldrywood", "detent", 1, nil, "", now),
			snapshot: DispatchSnapshot{EvaluatedAt: now, Machines: []MachineAvailability{availableMachine}},
		},
		{
			name: "required blocker unresolved",
			item: func() WorkItem {
				item := readyWorkItem(2, 1, "digitaldrywood", "detent", 2, nil, "", now)
				item.Blockers = []WorkItemReference{{ID: 99, SourceState: SourceStateOpen, WorkflowState: &WorkflowState{Name: "Todo", Dispatchable: true}}}
				return item
			}(),
			snapshot:  DispatchSnapshot{EvaluatedAt: now, Machines: []MachineAvailability{availableMachine}},
			wantCodes: []DispatchReasonCode{DispatchReasonBlockerUnresolved},
		},
		{
			name: "foreign lease active",
			item: func() WorkItem {
				item := readyWorkItem(3, 1, "digitaldrywood", "detent", 3, nil, "", now)
				item.ActiveLease = &LeaseSummary{ID: "lease-b", Machine: MachineSummary{ID: "machine-b"}, ExpiresAt: now.Add(time.Minute)}
				return item
			}(),
			snapshot:  DispatchSnapshot{EvaluatedAt: now, TargetMachineID: "machine-a", Machines: []MachineAvailability{availableMachine}},
			wantCodes: []DispatchReasonCode{DispatchReasonLeaseActive},
		},
		{
			name: "own lease can resume",
			item: func() WorkItem {
				item := readyWorkItem(4, 1, "digitaldrywood", "detent", 4, nil, "", now)
				item.ActiveLease = &LeaseSummary{ID: "lease-a", Machine: MachineSummary{ID: "machine-a"}, SessionID: "session-a", ExpiresAt: now.Add(time.Minute)}
				return item
			}(),
			snapshot: DispatchSnapshot{
				EvaluatedAt:           now,
				TargetMachineID:       "machine-a",
				TargetSessionID:       "session-a",
				Machines:              []MachineAvailability{{ID: "machine-a", Healthy: true, Capacity: 1, ActiveLeases: 1}},
				RepositoryConcurrency: map[RepositoryID]ConcurrencyUsage{1: {Active: 1, Limit: 1}},
				ProjectConcurrency:    map[string]ConcurrencyUsage{"fleet": {Active: 1, Limit: 1}},
			},
		},
		{
			name: "same machine different session cannot resume",
			item: func() WorkItem {
				item := readyWorkItem(18, 1, "digitaldrywood", "detent", 18, nil, "", now)
				item.ActiveLease = &LeaseSummary{ID: "lease-a", Machine: MachineSummary{ID: "machine-a"}, SessionID: "session-a", ExpiresAt: now.Add(time.Minute)}
				return item
			}(),
			snapshot: DispatchSnapshot{
				EvaluatedAt:           now,
				TargetMachineID:       "machine-a",
				TargetSessionID:       "session-b",
				Machines:              []MachineAvailability{{ID: "machine-a", Healthy: true, Capacity: 1, ActiveLeases: 1}},
				RepositoryConcurrency: map[RepositoryID]ConcurrencyUsage{1: {Active: 1, Limit: 1}},
				ProjectConcurrency:    map[string]ConcurrencyUsage{"fleet": {Active: 1, Limit: 1}},
			},
			wantCodes: []DispatchReasonCode{
				DispatchReasonLeaseActive,
				DispatchReasonRepositoryConcurrencyLimit,
				DispatchReasonProjectConcurrencyLimit,
				DispatchReasonMachineCapacityFull,
			},
		},
		{
			name: "expired lease is ignored",
			item: func() WorkItem {
				item := readyWorkItem(5, 1, "digitaldrywood", "detent", 5, nil, "", now)
				item.ActiveLease = &LeaseSummary{ID: "expired", Machine: MachineSummary{ID: "machine-b"}, ExpiresAt: now.Add(-time.Nanosecond)}
				return item
			}(),
			snapshot: DispatchSnapshot{EvaluatedAt: now, Machines: []MachineAvailability{availableMachine}},
		},
		{
			name:      "repository concurrency full",
			item:      readyWorkItem(6, 1, "digitaldrywood", "detent", 6, nil, "", now),
			snapshot:  DispatchSnapshot{EvaluatedAt: now, Machines: []MachineAvailability{availableMachine}, RepositoryConcurrency: map[RepositoryID]ConcurrencyUsage{1: {Active: 2, Limit: 2}}},
			wantCodes: []DispatchReasonCode{DispatchReasonRepositoryConcurrencyLimit},
		},
		{
			name:      "project concurrency full",
			item:      readyWorkItem(7, 1, "digitaldrywood", "detent", 7, nil, "", now),
			snapshot:  DispatchSnapshot{EvaluatedAt: now, Machines: []MachineAvailability{availableMachine}, ProjectConcurrency: map[string]ConcurrencyUsage{" FLEET ": {Active: 1, Limit: 1}}},
			wantCodes: []DispatchReasonCode{DispatchReasonProjectConcurrencyLimit},
		},
		{
			name:     "repository and project have room",
			item:     readyWorkItem(8, 1, "digitaldrywood", "detent", 8, nil, "", now),
			snapshot: DispatchSnapshot{EvaluatedAt: now, Machines: []MachineAvailability{availableMachine}, RepositoryConcurrency: map[RepositoryID]ConcurrencyUsage{1: {Active: 1, Limit: 2}}, ProjectConcurrency: map[string]ConcurrencyUsage{"fleet": {Active: 0, Limit: 1}}},
		},
		{
			name:      "no compatible machine",
			item:      readyWorkItem(9, 1, "digitaldrywood", "detent", 9, nil, "", now),
			snapshot:  DispatchSnapshot{EvaluatedAt: now, Machines: []MachineAvailability{{ID: "machine-b", Healthy: true, Capacity: 1, RepositoryIDs: []RepositoryID{2}}}},
			wantCodes: []DispatchReasonCode{DispatchReasonNoCompatibleMachine},
		},
		{
			name:      "target machine is outside project scope",
			item:      readyWorkItem(10, 1, "digitaldrywood", "detent", 10, nil, "", now),
			snapshot:  DispatchSnapshot{EvaluatedAt: now, TargetMachineID: "machine-a", Machines: []MachineAvailability{{ID: "machine-a", Healthy: true, Capacity: 1, ProjectScopes: []string{"other"}}, {ID: "machine-b", Healthy: true, Capacity: 1}}},
			wantCodes: []DispatchReasonCode{DispatchReasonNoCompatibleMachine},
		},
		{
			name:      "compatible machine unhealthy",
			item:      readyWorkItem(11, 1, "digitaldrywood", "detent", 11, nil, "", now),
			snapshot:  DispatchSnapshot{EvaluatedAt: now, Machines: []MachineAvailability{{ID: "machine-a", Capacity: 1}}},
			wantCodes: []DispatchReasonCode{DispatchReasonMachineUnhealthy},
		},
		{
			name:      "compatible healthy machine full",
			item:      readyWorkItem(12, 1, "digitaldrywood", "detent", 12, nil, "", now),
			snapshot:  DispatchSnapshot{EvaluatedAt: now, Machines: []MachineAvailability{{ID: "machine-a", Healthy: true, Capacity: 1, ActiveLeases: 1}}},
			wantCodes: []DispatchReasonCode{DispatchReasonMachineCapacityFull},
		},
		{
			name:      "operator globally paused",
			item:      readyWorkItem(13, 1, "digitaldrywood", "detent", 13, nil, "", now),
			snapshot:  DispatchSnapshot{EvaluatedAt: now, Machines: []MachineAvailability{availableMachine}, OperatorPaused: true},
			wantCodes: []DispatchReasonCode{DispatchReasonOperatorPaused},
		},
		{
			name:      "repository paused",
			item:      readyWorkItem(14, 1, "digitaldrywood", "detent", 14, nil, "", now),
			snapshot:  DispatchSnapshot{EvaluatedAt: now, Machines: []MachineAvailability{availableMachine}, PausedRepositories: []RepositoryID{1}},
			wantCodes: []DispatchReasonCode{DispatchReasonOperatorPaused},
		},
		{
			name:      "project paused",
			item:      readyWorkItem(15, 1, "digitaldrywood", "detent", 15, nil, "", now),
			snapshot:  DispatchSnapshot{EvaluatedAt: now, Machines: []MachineAvailability{availableMachine}, PausedProjects: []string{" FLEET "}},
			wantCodes: []DispatchReasonCode{DispatchReasonOperatorPaused},
		},
		{
			name:      "global sync safety gate",
			item:      readyWorkItem(16, 1, "digitaldrywood", "detent", 16, nil, "", now),
			snapshot:  DispatchSnapshot{EvaluatedAt: now, Machines: []MachineAvailability{availableMachine}, SyncSafetyBlocked: true},
			wantCodes: []DispatchReasonCode{DispatchReasonSyncUnsafe},
		},
		{
			name:      "repository sync unsafe",
			item:      readyWorkItem(17, 1, "digitaldrywood", "detent", 17, nil, "", now),
			snapshot:  DispatchSnapshot{EvaluatedAt: now, Machines: []MachineAvailability{availableMachine}, SyncUnsafeRepositories: []RepositoryID{1}},
			wantCodes: []DispatchReasonCode{DispatchReasonSyncUnsafe},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			original := test.item.Dispatchability
			queue := DeriveDispatchQueue([]WorkItem{test.item}, test.snapshot)
			if !reflect.DeepEqual(test.item.Dispatchability, original) {
				t.Fatalf("DeriveDispatchQueue mutated input Dispatchability: got %#v, want %#v", test.item.Dispatchability, original)
			}
			if len(test.wantCodes) == 0 {
				if len(queue.Dispatchable) != 1 || len(queue.NonDispatchable) != 0 {
					t.Fatalf("queue = %#v, want one dispatchable item", queue)
				}
				if queue.Dispatchable[0].Dispatchability.Reasons == nil {
					t.Fatal("dispatchable reasons = nil, want empty structured slice")
				}
				return
			}
			if len(queue.Dispatchable) != 0 || len(queue.NonDispatchable) != 1 {
				t.Fatalf("queue = %#v, want one non-dispatchable item", queue)
			}
			gotCodes := reasonCodes(queue.NonDispatchable[0].Dispatchability.Reasons)
			if !reflect.DeepEqual(gotCodes, test.wantCodes) {
				t.Fatalf("reason codes = %v, want %v", gotCodes, test.wantCodes)
			}
		})
	}
}

func TestDeriveDispatchQueueTotalOrder(t *testing.T) {
	t.Parallel()

	oldest := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := oldest.Add(time.Hour)
	same := oldest.Add(2 * time.Hour)
	urgent := QueuePriorityUrgent
	high := QueuePriorityHigh
	normal := QueuePriorityNormal
	low := QueuePriorityLow
	items := []WorkItem{
		readyWorkItem(105, 1, "zeta", "unset", 1, nil, "a", oldest),
		readyWorkItem(209, 4, "gamma", "repo", 10, &high, "b", same),
		readyWorkItem(103, 1, "zeta", "normal", 1, &normal, "a", oldest),
		readyWorkItem(202, 1, "zeta", "high-z", 1, &high, "z", oldest),
		readyWorkItem(207, 3, "beta", "repo", 1, &high, "b", same),
		readyWorkItem(101, 1, "zeta", "urgent", 1, &urgent, "", same),
		readyWorkItem(204, 1, "zeta", "older", 1, &high, "b", oldest),
		readyWorkItem(208, 4, "gamma", "repo", 2, &high, "b", same),
		readyWorkItem(104, 1, "zeta", "low", 1, &low, "a", oldest),
		readyWorkItem(203, 1, "zeta", "rank-unset", 1, &high, "", oldest),
		readyWorkItem(201, 1, "zeta", "high-a", 1, &high, "a", newer),
		readyWorkItem(206, 2, "alpha", "repo", 10, &high, "b", same),
		readyWorkItem(205, 1, "zeta", "newer", 1, &high, "b", newer),
	}
	want := []WorkItemID{101, 201, 204, 205, 206, 207, 208, 209, 202, 203, 103, 104, 105}
	reversed := make([]WorkItem, len(items))
	for i := range items {
		reversed[len(items)-1-i] = items[i]
	}
	rotated := append(append([]WorkItem(nil), items[5:]...), items[:5]...)
	snapshot := DispatchSnapshot{EvaluatedAt: same, Machines: []MachineAvailability{{ID: "machine-a", Healthy: true, Capacity: 1}}}

	for name, input := range map[string][]WorkItem{"original": items, "reversed": reversed, "rotated": rotated} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			queue := DeriveDispatchQueue(input, snapshot)
			if len(queue.NonDispatchable) != 0 {
				t.Fatalf("non-dispatchable = %#v, want none", queue.NonDispatchable)
			}
			got := workItemIDs(queue.Dispatchable)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ordered IDs = %v, want %v", got, want)
			}
		})
	}
}

func TestDeriveDispatchQueueDeterministicAcrossMachineStateOrder(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	items := []WorkItem{
		readyWorkItem(2, 1, "digitaldrywood", "detent", 2, nil, "b", now),
		readyWorkItem(1, 1, "digitaldrywood", "detent", 1, nil, "a", now),
	}
	first := DispatchSnapshot{EvaluatedAt: now, Machines: []MachineAvailability{{ID: "machine-full", Healthy: true, Capacity: 1, ActiveLeases: 1}, {ID: "machine-open", Healthy: true, Capacity: 1}}}
	second := first
	second.Machines = []MachineAvailability{first.Machines[1], first.Machines[0]}

	left := DeriveDispatchQueue(items, first)
	right := DeriveDispatchQueue(items, second)
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("queues differ for identical machine state: left %#v right %#v", left, right)
	}
}

func TestDeriveDispatchQueueStructuredReasonDetails(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	item := readyWorkItem(1, 7, "digitaldrywood", "detent", 2072, nil, "a", now)
	item.Blockers = []WorkItemReference{{ID: 6, WorkflowState: &WorkflowState{Name: "Todo"}}}
	item.ActiveLease = &LeaseSummary{ID: "lease-b", Machine: MachineSummary{ID: "machine-b"}, SessionID: "session-b", ExpiresAt: now.Add(time.Minute)}
	snapshot := DispatchSnapshot{
		EvaluatedAt:            now,
		TargetMachineID:        "machine-a",
		RepositoryConcurrency:  map[RepositoryID]ConcurrencyUsage{7: {Active: 2, Limit: 2}},
		ProjectConcurrency:     map[string]ConcurrencyUsage{"fleet": {Active: 1, Limit: 1}},
		Machines:               []MachineAvailability{{ID: "machine-a", Healthy: true, Capacity: 1, ActiveLeases: 1}},
		PausedRepositories:     []RepositoryID{7},
		SyncUnsafeRepositories: []RepositoryID{7},
	}

	queue := DeriveDispatchQueue([]WorkItem{item}, snapshot)
	if len(queue.NonDispatchable) != 1 {
		t.Fatalf("non-dispatchable count = %d, want 1", len(queue.NonDispatchable))
	}
	reasons := queue.NonDispatchable[0].Dispatchability.Reasons
	wantCodes := []DispatchReasonCode{
		DispatchReasonBlockerUnresolved,
		DispatchReasonLeaseActive,
		DispatchReasonRepositoryConcurrencyLimit,
		DispatchReasonProjectConcurrencyLimit,
		DispatchReasonMachineCapacityFull,
		DispatchReasonOperatorPaused,
		DispatchReasonSyncUnsafe,
	}
	if got := reasonCodes(reasons); !reflect.DeepEqual(got, wantCodes) {
		t.Fatalf("reason codes = %v, want %v", got, wantCodes)
	}
	if reasons[0].WorkItemID == nil || *reasons[0].WorkItemID != 6 {
		t.Errorf("blocker reason = %#v, want blocker 6", reasons[0])
	}
	if reasons[1].LeaseID != "lease-b" || reasons[1].MachineID != "machine-b" || reasons[1].SessionID != "session-b" {
		t.Errorf("lease reason = %#v, want lease-b on machine-b in session-b", reasons[1])
	}
	if reasons[2].Repository != 7 || reasons[2].Active != 2 || reasons[2].Limit != 2 {
		t.Errorf("repository reason = %#v, want repository 7 at 2/2", reasons[2])
	}
	if reasons[3].Scope != "fleet" || reasons[3].Active != 1 || reasons[3].Limit != 1 {
		t.Errorf("project reason = %#v, want fleet at 1/1", reasons[3])
	}
	if reasons[4].MachineID != "machine-a" || reasons[4].Active != 1 || reasons[4].Limit != 1 {
		t.Errorf("machine reason = %#v, want machine-a at 1/1", reasons[4])
	}
	if reasons[5].Repository != 7 || reasons[6].Repository != 7 {
		t.Errorf("repository gate reasons = %#v %#v, want repository 7", reasons[5], reasons[6])
	}
}

func readyWorkItem(id WorkItemID, repositoryID RepositoryID, owner, repository string, number int, priority *int, rank string, created time.Time) WorkItem {
	return WorkItem{
		ID:            id,
		Repository:    RepositoryReference{ID: repositoryID, Owner: owner, Name: repository},
		GitHub:        GitHubIssueReference{Number: number},
		SourceState:   SourceStateOpen,
		WorkflowState: &WorkflowState{Name: "Todo", Dispatchable: true},
		Queue:         &QueueSummary{Scope: "fleet", State: "Todo", Rank: rank, PriorityRank: priority},
		CreatedAt:     &created,
		Blockers:      []WorkItemReference{},
		SyncStatus:    SyncStatusSynced,
		Dispatchability: Dispatchability{
			Reasons: []DispatchReason{},
		},
	}
}

func reasonCodes(reasons []DispatchReason) []DispatchReasonCode {
	codes := make([]DispatchReasonCode, 0, len(reasons))
	for _, reason := range reasons {
		codes = append(codes, reason.Code)
	}
	return codes
}

func workItemIDs(items []WorkItem) []WorkItemID {
	ids := make([]WorkItemID, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}
