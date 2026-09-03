package hubserver

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConcurrentClaimsProduceOneFencedWinner(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	clock := &leaseTestClock{value: now}
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: clock.Now})
	_, issueID := seedProjection(t, service.database.db)
	seedHubMachine(t, service, "machine-prior", now)
	prior, err := service.Tracker().Claim(t.Context(), tracker.ClaimRequest{
		WorkItemID: tracker.WorkItemID(issueID),
		MachineID:  "machine-prior",
		SessionID:  "session-prior",
		TTL:        time.Minute,
	})
	if err != nil {
		t.Fatalf("Claim(prior) error = %v", err)
	}
	if err := service.Tracker().Release(t.Context(), tracker.ReleaseRequest{LeaseID: prior.ID, FencingToken: prior.FencingToken}); err != nil {
		t.Fatalf("Release(prior) error = %v", err)
	}

	const contenders = 10
	for i := range contenders {
		seedHubMachine(t, service, tracker.MachineID(fmt.Sprintf("machine-%d", i)), now)
	}
	type claimResult struct {
		lease tracker.Lease
		err   error
	}
	start := make(chan struct{})
	results := make(chan claimResult, contenders)
	var group sync.WaitGroup
	for i := range contenders {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			lease, claimErr := service.Tracker().Claim(t.Context(), tracker.ClaimRequest{
				WorkItemID: tracker.WorkItemID(issueID),
				MachineID:  tracker.MachineID(fmt.Sprintf("machine-%d", index)),
				SessionID:  fmt.Sprintf("session-%d", index),
				TTL:        time.Minute,
			})
			results <- claimResult{lease: lease, err: claimErr}
		}(i)
	}
	close(start)
	group.Wait()
	close(results)

	winners := make([]tracker.Lease, 0, 1)
	conflicts := 0
	for result := range results {
		switch {
		case result.err == nil:
			winners = append(winners, result.lease)
		case errors.Is(result.err, tracker.ErrLeaseConflict):
			conflicts++
		default:
			t.Errorf("Claim() error = %v, want lease conflict", result.err)
		}
	}
	if len(winners) != 1 || conflicts != contenders-1 {
		t.Fatalf("claim results = %d winners, %d conflicts; want 1 winner, %d conflicts", len(winners), conflicts, contenders-1)
	}
	winner := winners[0]
	if winner.FencingToken <= prior.FencingToken {
		t.Errorf("winner fencing token = %d, want greater than prior token %d", winner.FencingToken, prior.FencingToken)
	}
	if winner.PreviousSession == nil || winner.PreviousSession.SessionID != prior.SessionID {
		t.Errorf("winner previous session = %#v, want session %q", winner.PreviousSession, prior.SessionID)
	}

	var total int
	var active int
	var maximumToken tracker.FencingToken
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT COUNT(*), COUNT(*) FILTER (WHERE released_at IS NULL), MAX(fencing_token) FROM leases WHERE issue_id = ?", issueID).Scan(&total, &active, &maximumToken); err != nil {
		t.Fatalf("query claim results: %v", err)
	}
	if total != 2 || active != 1 || maximumToken != winner.FencingToken {
		t.Fatalf("persisted leases = total %d active %d max token %d; want total 2 active 1 max token %d", total, active, maximumToken, winner.FencingToken)
	}
}

func TestLeaseLifecycleRenewsHeartbeatsAndPreservesReleaseReason(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 12, 30, 0, 0, time.UTC)
	clock := &leaseTestClock{value: now}
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: clock.Now})
	_, issueID := seedProjection(t, service.database.db)
	seedHubMachine(t, service, "machine-a", now)
	request := tracker.ClaimRequest{WorkItemID: tracker.WorkItemID(issueID), MachineID: "machine-a", SessionID: "session-a", TTL: time.Minute}
	claimed, err := service.Tracker().Claim(t.Context(), request)
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	retried, err := service.Tracker().Claim(t.Context(), request)
	if err != nil {
		t.Fatalf("Claim(idempotent) error = %v", err)
	}
	if retried.ID != claimed.ID || retried.FencingToken != claimed.FencingToken || retried.PreviousSession != nil {
		t.Fatalf("Claim(idempotent) = %#v, want original lease without previous session", retried)
	}

	clock.Advance(20 * time.Second)
	renewedAt := clock.Now()
	renewed, err := service.Tracker().Renew(t.Context(), tracker.RenewRequest{LeaseID: claimed.ID, FencingToken: claimed.FencingToken, TTL: 2 * time.Minute})
	if err != nil {
		t.Fatalf("Renew() error = %v", err)
	}
	if !renewed.RenewedAt.Equal(renewedAt) || !renewed.ExpiresAt.Equal(renewedAt.Add(2*time.Minute)) {
		t.Errorf("Renew() timestamps = renewed %s expires %s", renewed.RenewedAt, renewed.ExpiresAt)
	}
	var heartbeatAt string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT last_heartbeat_at FROM machines WHERE id = 'machine-a'").Scan(&heartbeatAt); err != nil {
		t.Fatalf("read renewed heartbeat: %v", err)
	}
	if heartbeatAt != formatHubTime(renewedAt) {
		t.Errorf("machine heartbeat = %q, want %q", heartbeatAt, formatHubTime(renewedAt))
	}

	if err := service.Tracker().Release(t.Context(), tracker.ReleaseRequest{LeaseID: renewed.ID, FencingToken: renewed.FencingToken, Reason: "completed"}); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	replacement, err := service.Tracker().Claim(t.Context(), tracker.ClaimRequest{WorkItemID: tracker.WorkItemID(issueID), MachineID: "machine-a", SessionID: "session-b", TTL: time.Minute})
	if err != nil {
		t.Fatalf("Claim(replacement) error = %v", err)
	}
	if replacement.PreviousSession == nil || replacement.PreviousSession.LastEvent == nil || replacement.PreviousSession.LastEvent.Kind != "lease_released" || replacement.PreviousSession.LastEvent.Payload["reason"] != "completed" {
		t.Errorf("replacement previous session = %#v, want release reason", replacement.PreviousSession)
	}
	if _, err := service.Tracker().Claim(t.Context(), request); !errors.Is(err, tracker.ErrLeaseConflict) {
		t.Errorf("Claim(reused session) error = %v, want ErrLeaseConflict", err)
	}
}

func TestExpiredLeaseReassignmentRejectsEveryStaleMutation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC)
	clock := &leaseTestClock{value: now}
	var leaseIDs atomic.Int64
	service := openTestService(t, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"),
		now:          clock.Now,
		newLeaseID: func() string {
			return fmt.Sprintf("lease-%d", leaseIDs.Add(1))
		},
	})
	_, issueID := seedProjection(t, service.database.db)
	seedHubMachine(t, service, "machine-a", now)
	seedHubMachine(t, service, "machine-b", now)

	first, err := service.Tracker().Claim(t.Context(), tracker.ClaimRequest{
		WorkItemID: tracker.WorkItemID(issueID),
		MachineID:  "machine-a",
		SessionID:  "session-a",
		TTL:        time.Minute,
	})
	if err != nil {
		t.Fatalf("Claim(first) error = %v", err)
	}
	if err := service.Tracker().AppendEvent(t.Context(), tracker.WorkEvent{
		WorkItemID:   tracker.WorkItemID(issueID),
		FencingToken: first.FencingToken,
		RunID:        "run-a",
		Kind:         "progress",
		Payload: map[string]any{
			"summary":   "implementation in progress",
			"workspace": "/workspaces/session-a",
		},
	}); err != nil {
		t.Fatalf("AppendEvent(first) error = %v", err)
	}

	clock.Advance(2 * time.Minute)
	items, err := service.Tracker().GetWorkItems(t.Context(), []tracker.WorkItemID{tracker.WorkItemID(issueID)})
	if err != nil {
		t.Fatalf("GetWorkItems(expired) error = %v", err)
	}
	if len(items) != 1 || items[0].ActiveLease != nil {
		t.Fatalf("expired work item = %#v, want no active lease", items)
	}
	var leaseRows int
	var eventRows int
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM leases WHERE issue_id = ?", issueID).Scan(&leaseRows); err != nil {
		t.Fatalf("count expired lease rows: %v", err)
	}
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM work_events WHERE issue_id = ?", issueID).Scan(&eventRows); err != nil {
		t.Fatalf("count expired session events: %v", err)
	}
	if leaseRows != 1 || eventRows != 1 {
		t.Fatalf("metadata after expiry = %d leases, %d events; want 1 lease and 1 event", leaseRows, eventRows)
	}

	second, err := service.Tracker().Claim(t.Context(), tracker.ClaimRequest{
		WorkItemID: tracker.WorkItemID(issueID),
		MachineID:  "machine-b",
		SessionID:  "session-b",
		TTL:        2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Claim(second) error = %v", err)
	}
	if second.FencingToken <= first.FencingToken {
		t.Errorf("second fencing token = %d, want greater than %d", second.FencingToken, first.FencingToken)
	}
	previous := second.PreviousSession
	if previous == nil || previous.SessionID != "session-a" || previous.ReleasedAt == nil || previous.LastEvent == nil {
		t.Fatalf("second previous session = %#v, want released session-a with its last event", previous)
	}
	if previous.LastEvent.Kind != "progress" || previous.LastEvent.Payload["workspace"] != "/workspaces/session-a" || previous.LastEvent.Payload["summary"] != "implementation in progress" {
		t.Errorf("previous session event = %#v, want durable recovery payload", previous.LastEvent)
	}

	tests := []struct {
		name   string
		mutate func() error
	}{
		{
			name: "renew",
			mutate: func() error {
				_, renewErr := service.Tracker().Renew(t.Context(), tracker.RenewRequest{LeaseID: first.ID, FencingToken: first.FencingToken, TTL: time.Minute})
				return renewErr
			},
		},
		{
			name: "release",
			mutate: func() error {
				return service.Tracker().Release(t.Context(), tracker.ReleaseRequest{LeaseID: first.ID, FencingToken: first.FencingToken})
			},
		},
		{
			name: "append event",
			mutate: func() error {
				return service.Tracker().AppendEvent(t.Context(), tracker.WorkEvent{WorkItemID: tracker.WorkItemID(issueID), FencingToken: first.FencingToken, Kind: "late_progress"})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.mutate(); !errors.Is(err, tracker.ErrStaleFencingToken) {
				t.Fatalf("stale mutation error = %v, want ErrStaleFencingToken", err)
			}
		})
	}

	if err := service.Tracker().AppendEvent(t.Context(), tracker.WorkEvent{WorkItemID: tracker.WorkItemID(issueID), FencingToken: second.FencingToken, Kind: "recovered"}); err != nil {
		t.Fatalf("AppendEvent(second) error = %v", err)
	}
	var lateEvents int
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM work_events WHERE kind = 'late_progress'").Scan(&lateEvents); err != nil {
		t.Fatalf("count late events: %v", err)
	}
	if lateEvents != 0 {
		t.Fatalf("late stale events = %d, want 0", lateEvents)
	}
}

func TestClaimRecoversDisappearedMachineSessionAfterHubRestart(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC)
	clock := &leaseTestClock{value: now}
	path := filepath.Join(t.TempDir(), "hub.db")
	config := Config{DatabasePath: path, now: clock.Now}
	service := openTestService(t, config)
	_, issueID := seedProjection(t, service.database.db)
	seedHubMachine(t, service, "machine-a", now)
	seedHubMachine(t, service, "machine-b", now)
	first, err := service.Tracker().Claim(t.Context(), tracker.ClaimRequest{
		WorkItemID: tracker.WorkItemID(issueID),
		MachineID:  "machine-a",
		SessionID:  "session-a",
		TTL:        time.Minute,
	})
	if err != nil {
		t.Fatalf("Claim(first) error = %v", err)
	}
	if err := service.Tracker().AppendEvent(t.Context(), tracker.WorkEvent{
		WorkItemID:   tracker.WorkItemID(issueID),
		FencingToken: first.FencingToken,
		Kind:         "session_summary",
		Payload:      map[string]any{"provider_session_id": "provider-a", "workspace": "/workspace/a"},
	}); err != nil {
		t.Fatalf("AppendEvent(session summary) error = %v", err)
	}
	clock.Advance(2 * time.Minute)
	if err := service.Close(); err != nil {
		t.Fatalf("Close(first hub) error = %v", err)
	}

	restarted := openTestService(t, config)
	replacement, err := restarted.Tracker().Claim(t.Context(), tracker.ClaimRequest{
		WorkItemID: tracker.WorkItemID(issueID),
		MachineID:  "machine-b",
		SessionID:  "session-b",
		TTL:        time.Minute,
	})
	if err != nil {
		t.Fatalf("Claim(replacement) error = %v", err)
	}
	previous := replacement.PreviousSession
	if previous == nil || previous.SessionID != "session-a" || previous.LastEvent == nil {
		t.Fatalf("replacement previous session = %#v, want durable session-a summary", previous)
	}
	if previous.LastEvent.Payload["provider_session_id"] != "provider-a" || previous.LastEvent.Payload["workspace"] != "/workspace/a" {
		t.Errorf("replacement recovery payload = %#v", previous.LastEvent.Payload)
	}
}

func TestLeaseMutationValidation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 15, 0, 0, 0, time.UTC)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
	_, issueID := seedProjection(t, service.database.db)
	seedHubMachine(t, service, "machine-a", now)
	token := tracker.FencingToken(1)
	tests := []struct {
		name   string
		mutate func() error
		want   error
	}{
		{
			name: "invalid claim TTL",
			mutate: func() error {
				_, err := service.Tracker().Claim(t.Context(), tracker.ClaimRequest{WorkItemID: tracker.WorkItemID(issueID), MachineID: "machine-a", SessionID: "session-a"})
				return err
			},
			want: tracker.ErrInvalidClaimRequest,
		},
		{
			name: "missing work item",
			mutate: func() error {
				_, err := service.Tracker().Claim(t.Context(), tracker.ClaimRequest{WorkItemID: 999, MachineID: "machine-a", SessionID: "session-a", TTL: time.Minute})
				return err
			},
			want: tracker.ErrWorkItemNotFound,
		},
		{
			name: "missing machine",
			mutate: func() error {
				_, err := service.Tracker().Claim(t.Context(), tracker.ClaimRequest{WorkItemID: tracker.WorkItemID(issueID), MachineID: "missing", SessionID: "session-a", TTL: time.Minute})
				return err
			},
			want: tracker.ErrMachineNotFound,
		},
		{
			name: "missing lease",
			mutate: func() error {
				_, err := service.Tracker().Renew(t.Context(), tracker.RenewRequest{LeaseID: "missing", FencingToken: token, TTL: time.Minute})
				return err
			},
			want: tracker.ErrLeaseNotFound,
		},
		{
			name: "invalid release token",
			mutate: func() error {
				return service.Tracker().Release(t.Context(), tracker.ReleaseRequest{LeaseID: "lease-a"})
			},
			want: tracker.ErrInvalidLeaseRequest,
		},
		{
			name: "event requires token",
			mutate: func() error {
				return service.Tracker().AppendEvent(t.Context(), tracker.WorkEvent{WorkItemID: tracker.WorkItemID(issueID), Kind: "progress"})
			},
			want: tracker.ErrInvalidWorkEvent,
		},
		{
			name: "event payload must encode",
			mutate: func() error {
				return service.Tracker().AppendEvent(t.Context(), tracker.WorkEvent{WorkItemID: tracker.WorkItemID(issueID), FencingToken: token, Kind: "progress", Payload: map[string]any{"invalid": make(chan int)}})
			},
			want: tracker.ErrInvalidWorkEvent,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.mutate(); !errors.Is(err, test.want) {
				t.Fatalf("mutation error = %v, want %v", err, test.want)
			}
		})
	}
}

func seedHubMachine(t *testing.T, service *Service, machineID tracker.MachineID, now time.Time) {
	t.Helper()
	if _, err := service.database.db.ExecContext(t.Context(), `
INSERT INTO machines (id, hostname, display_name, capacity, version, last_heartbeat_at, registered_at, updated_at)
VALUES (?, ?, ?, 1, 'test', ?, ?, ?)`, machineID, machineID, machineID, formatHubTime(now), formatHubTime(now), formatHubTime(now)); err != nil {
		t.Fatalf("insert machine %s: %v", machineID, err)
	}
}

type leaseTestClock struct {
	mu    sync.Mutex
	value time.Time
}

func (c *leaseTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func (c *leaseTestClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = c.value.Add(duration)
}
