package tracker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizeWorkItem(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	todo := &WorkflowState{ID: 1, SourceName: " Todo ", Name: " Todo ", Dispatchable: true}
	done := &WorkflowState{ID: 2, SourceName: " Done ", Name: " Done ", Terminal: true}
	priority := 2
	tests := []struct {
		name              string
		record            Record
		wantDispatchable  bool
		wantReasons       []DispatchReasonCode
		wantState         SourceState
		wantSync          SyncStatus
		wantActiveLease   bool
		wantLabels        []string
		wantBodySuffix    string
		wantBlockerID     WorkItemID
		wantReasonLeaseID LeaseID
	}{
		{
			name: "complete source record",
			record: Record{
				ID:            7,
				Repository:    RepositoryReference{ID: 3, GitHubNodeID: " R_repo ", Owner: " digitaldrywood ", Name: " detent "},
				GitHub:        GitHubIssueReference{NodeID: "I_issue", Number: 2068},
				Title:         " Tracker contract ",
				Body:          " normalized body ",
				URL:           " https://example.test/issues/2068 ",
				SourceState:   " OPEN ",
				WorkflowState: todo,
				Queue:         &QueueSummary{Scope: " fleet ", State: " Todo ", Rank: " a0 ", PriorityRank: &priority},
				Labels:        []string{" feature ", "Feature", "hub"},
				Assignees:     []string{" detent-bot ", ""},
				Blockers:      []WorkItemReference{{ID: 6, SourceState: SourceStateClosed, WorkflowState: done}},
				SyncStatus:    SyncStatusSynced,
				ObservedAt:    now,
			},
			wantDispatchable: true,
			wantState:        SourceStateOpen,
			wantSync:         SyncStatusSynced,
			wantLabels:       []string{"feature", "hub"},
		},
		{
			name:   "missing upstream fields",
			record: Record{ID: 8, Labels: nil, Assignees: nil},
			wantReasons: []DispatchReasonCode{
				DispatchReasonSourceStateUnknown,
				DispatchReasonWorkflowStateMissing,
				DispatchReasonSyncStale,
			},
			wantState:  SourceStateUnknown,
			wantSync:   SyncStatusStale,
			wantLabels: []string{},
		},
		{
			name: "partial blocked leased errored record",
			record: Record{
				ID:            9,
				SourceState:   "closed",
				WorkflowState: done,
				Blockers:      []WorkItemReference{{ID: 5, SourceState: SourceStateOpen, WorkflowState: todo}},
				Lease:         &LeaseSummary{ID: " lease-9 ", Machine: MachineSummary{ID: " machine-a "}, SessionID: " session-a ", ExpiresAt: now.Add(time.Minute)},
				SyncStatus:    SyncStatusError,
				ObservedAt:    now,
			},
			wantReasons: []DispatchReasonCode{
				DispatchReasonIssueClosed,
				DispatchReasonWorkflowStateTerminal,
				DispatchReasonBlockerUnresolved,
				DispatchReasonLeaseActive,
				DispatchReasonSyncError,
			},
			wantState:         SourceStateClosed,
			wantSync:          SyncStatusError,
			wantActiveLease:   true,
			wantBlockerID:     5,
			wantReasonLeaseID: "lease-9",
		},
		{
			name: "closed blocker without configured terminal state",
			record: Record{
				ID:            12,
				SourceState:   "open",
				WorkflowState: todo,
				Blockers:      []WorkItemReference{{ID: 6, SourceState: SourceStateClosed}},
				SyncStatus:    SyncStatusSynced,
			},
			wantReasons:   []DispatchReasonCode{DispatchReasonBlockerUnresolved},
			wantState:     SourceStateOpen,
			wantSync:      SyncStatusSynced,
			wantLabels:    []string{},
			wantBlockerID: 6,
		},
		{
			name:             "expired lease omitted",
			record:           Record{ID: 10, SourceState: "open", WorkflowState: todo, Lease: &LeaseSummary{ID: "expired", ExpiresAt: now.Add(-time.Second)}, SyncStatus: SyncStatusPending, ObservedAt: now},
			wantDispatchable: true,
			wantState:        SourceStateOpen,
			wantSync:         SyncStatusPending,
			wantLabels:       []string{},
		},
		{
			name:             "unicode body excerpt",
			record:           Record{ID: 11, SourceState: "open", WorkflowState: todo, Body: strings.Repeat("木", bodyExcerptLimit+1), SyncStatus: SyncStatusSynced},
			wantDispatchable: true,
			wantState:        SourceStateOpen,
			wantSync:         SyncStatusSynced,
			wantLabels:       []string{},
			wantBodySuffix:   "…",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := Normalize(test.record)
			if got.Dispatchability.Dispatchable != test.wantDispatchable {
				t.Fatalf("Dispatchable = %t, want %t; reasons = %#v", got.Dispatchability.Dispatchable, test.wantDispatchable, got.Dispatchability.Reasons)
			}
			if got.Dispatchability.Reasons == nil {
				t.Fatal("Dispatchability.Reasons = nil, want structured reason slice")
			}
			if gotCodes := dispatchReasonCodes(got.Dispatchability.Reasons); strings.Join(gotCodes, ",") != strings.Join(reasonCodeStrings(test.wantReasons), ",") {
				t.Fatalf("reason codes = %v, want %v", gotCodes, test.wantReasons)
			}
			if got.SourceState != test.wantState {
				t.Errorf("SourceState = %q, want %q", got.SourceState, test.wantState)
			}
			if got.SyncStatus != test.wantSync {
				t.Errorf("SyncStatus = %q, want %q", got.SyncStatus, test.wantSync)
			}
			if (got.ActiveLease != nil) != test.wantActiveLease {
				t.Errorf("ActiveLease present = %t, want %t", got.ActiveLease != nil, test.wantActiveLease)
			}
			if test.wantReasonLeaseID != "" {
				if got.ActiveLease == nil || got.ActiveLease.ID != test.wantReasonLeaseID {
					t.Errorf("ActiveLease = %#v, want ID %q", got.ActiveLease, test.wantReasonLeaseID)
				}
			}
			if test.wantBlockerID != 0 {
				matchedReason := false
				for _, reason := range got.Dispatchability.Reasons {
					if reason.Code == DispatchReasonBlockerUnresolved && reason.WorkItemID != nil && *reason.WorkItemID == test.wantBlockerID {
						matchedReason = true
					}
				}
				if got.Blockers[0].ID != test.wantBlockerID || !matchedReason {
					t.Errorf("blocker identity was not retained: item=%#v reasons=%#v", got.Blockers, got.Dispatchability.Reasons)
				}
			}
			if test.wantLabels != nil && strings.Join(got.Labels, ",") != strings.Join(test.wantLabels, ",") {
				t.Errorf("Labels = %v, want %v", got.Labels, test.wantLabels)
			}
			if test.wantBodySuffix != "" && !strings.HasSuffix(got.BodyExcerpt, test.wantBodySuffix) {
				t.Errorf("BodyExcerpt suffix = %q, want %q", got.BodyExcerpt, test.wantBodySuffix)
			}
		})
	}
}

func TestStoreTrackerReadsNormalizedItems(t *testing.T) {
	t.Parallel()

	store := &fakeStore{records: []Record{{ID: 1, SourceState: "open", WorkflowState: &WorkflowState{Name: "Todo", Dispatchable: true}, SyncStatus: SyncStatusSynced}}}
	backend, err := NewStore(store)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	items, err := backend.ListCandidates(t.Context(), CandidateQuery{Scope: "fleet"})
	if err != nil {
		t.Fatalf("ListCandidates() error = %v", err)
	}
	if len(items) != 1 || !items[0].Dispatchability.Dispatchable || store.candidateQuery.Scope != "fleet" {
		t.Fatalf("ListCandidates() = %#v; query = %#v", items, store.candidateQuery)
	}
	items, err = backend.GetWorkItems(t.Context(), []WorkItemID{1})
	if err != nil {
		t.Fatalf("GetWorkItems() error = %v", err)
	}
	if len(items) != 1 || len(store.workItemIDs) != 1 || store.workItemIDs[0] != 1 {
		t.Fatalf("GetWorkItems() = %#v; IDs = %v", items, store.workItemIDs)
	}
	queue, err := backend.ListDispatchQueue(t.Context(), CandidateQuery{Scope: "fleet"}, DispatchSnapshot{Machines: []MachineAvailability{{ID: "machine-a", Healthy: true, Capacity: 1}}})
	if err != nil {
		t.Fatalf("ListDispatchQueue() error = %v", err)
	}
	if len(queue.Dispatchable) != 1 || len(queue.NonDispatchable) != 0 {
		t.Fatalf("ListDispatchQueue() = %#v, want one dispatchable item", queue)
	}

	claimed, err := backend.Claim(t.Context(), ClaimRequest{WorkItemID: 1})
	if err != nil || claimed.ID != "lease-a" || store.claimRequest.WorkItemID != 1 {
		t.Errorf("Claim() = %#v, %v; request = %#v", claimed, err, store.claimRequest)
	}
	renewed, err := backend.Renew(t.Context(), RenewRequest{LeaseID: "lease-a"})
	if err != nil || renewed.ID != "lease-a" || store.renewRequest.LeaseID != "lease-a" {
		t.Errorf("Renew() = %#v, %v; request = %#v", renewed, err, store.renewRequest)
	}
	if err := backend.Release(t.Context(), ReleaseRequest{LeaseID: "lease-a"}); err != nil || store.releaseRequest.LeaseID != "lease-a" {
		t.Errorf("Release() error = %v; request = %#v", err, store.releaseRequest)
	}
	if err := backend.AppendEvent(t.Context(), WorkEvent{WorkItemID: 1, FencingToken: 7, Kind: "progress"}); err != nil || store.workEvent.Kind != "progress" || store.workEvent.FencingToken != 7 {
		t.Errorf("AppendEvent() error = %v; event = %#v", err, store.workEvent)
	}
}

func TestNewStoreRequiresStore(t *testing.T) {
	t.Parallel()
	if _, err := NewStore(nil); !errors.Is(err, ErrStoreRequired) {
		t.Fatalf("NewStore(nil) error = %v, want ErrStoreRequired", err)
	}
}

type fakeStore struct {
	records        []Record
	candidateQuery CandidateQuery
	workItemIDs    []WorkItemID
	claimRequest   ClaimRequest
	renewRequest   RenewRequest
	releaseRequest ReleaseRequest
	workEvent      WorkEvent
}

func (s *fakeStore) ListCandidateRecords(_ context.Context, query CandidateQuery) ([]Record, error) {
	s.candidateQuery = query
	return s.records, nil
}

func (s *fakeStore) GetWorkItemRecords(_ context.Context, ids []WorkItemID) ([]Record, error) {
	s.workItemIDs = append([]WorkItemID(nil), ids...)
	return s.records, nil
}

func (s *fakeStore) Claim(_ context.Context, request ClaimRequest) (Lease, error) {
	s.claimRequest = request
	return Lease{LeaseSummary: LeaseSummary{ID: "lease-a"}}, nil
}

func (s *fakeStore) Renew(_ context.Context, request RenewRequest) (Lease, error) {
	s.renewRequest = request
	return Lease{LeaseSummary: LeaseSummary{ID: "lease-a"}}, nil
}

func (s *fakeStore) Release(_ context.Context, request ReleaseRequest) error {
	s.releaseRequest = request
	return nil
}

func (s *fakeStore) AppendEvent(_ context.Context, event WorkEvent) error {
	s.workEvent = event
	return nil
}

func dispatchReasonCodes(reasons []DispatchReason) []string {
	result := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		result = append(result, string(reason.Code))
	}
	return result
}

func reasonCodeStrings(reasons []DispatchReasonCode) []string {
	result := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		result = append(result, string(reason))
	}
	return result
}
