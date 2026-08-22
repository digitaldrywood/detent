package healthnotify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/notify"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestManagerDeliversConfiguredWebhook(t *testing.T) {
	t.Parallel()
	received := make(chan Event, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("User-Agent"); got != healthNotifierUserAgent {
			t.Errorf("User-Agent = %q, want %q", got, healthNotifierUserAgent)
		}
		var event Event
		if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
			t.Errorf("decode event: %v", err)
		}
		received <- event
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	stateStore := newMemoryStateStore()
	manager, err := NewManager(Config{
		Webhook:  notify.WebhookConfig{URL: server.URL, Timeout: time.Second},
		Instance: "buildbox",
		Host:     "buildbox.example.test",
		Debounce: time.Second,
	}, Dependencies{Store: stateStore, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	now := time.Date(2026, 8, 13, 18, 0, 0, 0, time.UTC)
	health := readyHealth("detent")
	active := dispatchSnapshot("detent", "authorization_selector_declined")
	managerReconcile(t, manager, active, health, now)
	managerReconcile(t, manager, active, health, now.Add(time.Second))

	first := <-received
	second := <-received
	if first.Event != EventName || second.Event != EventName || first.ID == second.ID {
		t.Fatalf("received events = %#v, %#v", first, second)
	}
}

func TestManagerEntryPayloadsFireOncePerIdentity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name            string
		snapshot        telemetry.Snapshot
		cause           string
		wantWaitReasons []string
		wantTracker     bool
		wantForge       bool
	}{
		{
			name: "dispatch payload carries wait reason",
			snapshot: telemetry.Snapshot{DispatchStalls: []telemetry.DispatchStatus{{
				ProjectID: "detent", Stalled: true, WaitReason: "authorization selector excludes every candidate", WaitReasonCode: "authorization_selector_declined",
			}}},
			cause:           CauseDispatchStall,
			wantWaitReasons: []string{"authorization selector excludes every candidate"},
		},
		{
			name: "tracker payload carries scoped condition",
			snapshot: telemetry.Snapshot{TrackerUnavailable: []telemetry.TrackerCondition{{
				ProjectID: "detent", Connector: "github", ConnectorInstance: "detent:github", Operation: "observed_status", ErrorClass: "server",
			}}},
			cause:       CauseTrackerUnavailable,
			wantTracker: true,
		},
		{
			name: "forge payload carries scoped condition",
			snapshot: telemetry.Snapshot{ForgeUnavailable: []telemetry.ForgeCondition{{
				ProjectID: "detent", Host: "github.com", Operation: "git push", ErrorClass: "server",
			}}},
			cause:     CauseForgeUnavailable,
			wantForge: true,
		},
		{
			name:     "CI payload omits wait reason",
			snapshot: telemetry.Snapshot{CIUnavailable: []telemetry.CICondition{{ProjectID: "detent"}}},
			cause:    CauseCIUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stateStore := newMemoryStateStore()
			sender := &recordingSender{}
			manager := newTestManager(t, stateStore, sender, Config{Debounce: 10 * time.Minute})
			health := readyHealth("detent")

			managerReconcile(t, manager, tt.snapshot, health, now)
			managerReconcile(t, manager, tt.snapshot, health, now.Add(10*time.Minute))
			managerReconcile(t, manager, tt.snapshot, health, now.Add(20*time.Minute))

			events := sender.Events()
			if len(events) != 2 {
				t.Fatalf("events = %#v, want project and fleet entry", events)
			}
			slices.SortFunc(events, func(left Event, right Event) int { return left.ScopeCompare(right) })
			projectEvent := events[1]
			fleetEvent := events[0]
			if fleetEvent.Scope != ScopeFleet || fleetEvent.State != StateFleetNeedsAttention || fleetEvent.ProjectID != "" {
				t.Fatalf("fleet event = %#v", fleetEvent)
			}
			if projectEvent.Scope != ScopeProject || projectEvent.ProjectID != "detent" || projectEvent.State != StateProjectNeedsAttention {
				t.Fatalf("project event = %#v", projectEvent)
			}
			for _, event := range events {
				if event.Schema != SchemaVersion || event.Event != EventName || event.Transition != TransitionEntry || event.Instance != "buildbox" || event.Host != "buildbox.example.test" {
					t.Fatalf("event envelope = %#v", event)
				}
				if !slices.Equal(event.Causes, []string{tt.cause}) || !slices.Equal(event.WaitReasons, tt.wantWaitReasons) {
					t.Fatalf("event causes/waits = %v/%v, want %v/%v", event.Causes, event.WaitReasons, []string{tt.cause}, tt.wantWaitReasons)
				}
				if !event.EnteredAt.Equal(now) || !event.NeedsAttentionEnteredAt.Equal(now) || event.ID == "" {
					t.Fatalf("event timestamps/id = %#v", event)
				}
				if got := len(event.TrackerConditions) > 0; got != tt.wantTracker {
					t.Fatalf("TrackerConditions = %#v, want present %v", event.TrackerConditions, tt.wantTracker)
				}
				if got := len(event.ForgeConditions) > 0; got != tt.wantForge {
					t.Fatalf("ForgeConditions = %#v, want present %v", event.ForgeConditions, tt.wantForge)
				}
			}
		})
	}
}

func TestManagerBreakerPayloadCarriesOperatorEvidence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 13, 18, 0, 0, 0, time.UTC)
	resetAt := now.Add(39 * time.Minute)
	eligibleCandidates := 1
	breaker := telemetry.FailureBreaker{
		ProjectID:              "digitaldrywood-video",
		Class:                  "runner_error:b6c174a86dfb",
		Count:                  5,
		AttemptCount:           5,
		DistinctItemCount:      1,
		Cause:                  "provider usage limit reached",
		RepresentativeError:    "You've hit your limit. Try again at 9:39 PM",
		BackendID:              "claude-code",
		BackendKind:            "claude_code",
		Provider:               "anthropic",
		EligibleCandidateCount: &eligibleCandidates,
		Items: []telemetry.FailureBreakerItem{{
			IssueID: "video-1", Identifier: "2026-07-10-detent-not-vibe-coding-short", IssueURL: "https://example.test/items/video-1", Title: "Author beat visuals", CurrentState: "Blocked", AttemptCount: 5, Parked: true,
		}},
		ResumeAt: now.Add(time.Hour),
	}
	outage := telemetry.BackendOutage{
		ProjectID: "digitaldrywood-video", BackendID: "claude-code", BackendKind: "claude_code", Provider: "anthropic", Kind: "usage_limit_exceeded", Reason: "provider usage limit reached", ResetAt: &resetAt, ResumeAt: resetAt.Add(5 * time.Second),
	}
	breaker.BackendOutage = &outage
	snapshot := telemetry.Snapshot{FailureBreakers: []telemetry.FailureBreaker{breaker}, BackendOutages: []telemetry.BackendOutage{outage}}
	stateStore := newMemoryStateStore()
	sender := &recordingSender{}
	manager := newTestManager(t, stateStore, sender, Config{Debounce: time.Minute})
	health := readyHealth("digitaldrywood-video")

	managerReconcile(t, manager, snapshot, health, now)
	managerReconcile(t, manager, snapshot, health, now.Add(time.Minute))

	var breakerEvent *Event
	for _, event := range sender.Events() {
		if event.ProjectID == "digitaldrywood-video" && slices.Equal(event.Causes, []string{CauseProjectFailureBreaker}) {
			value := event
			breakerEvent = &value
			break
		}
	}
	if breakerEvent == nil {
		t.Fatalf("events = %#v, want project failure breaker event", sender.Events())
	}
	if len(breakerEvent.FailureBreakers) != 1 || breakerEvent.FailureBreakers[0].AttemptCount != 5 || breakerEvent.FailureBreakers[0].DistinctItemCount != 1 {
		t.Fatalf("FailureBreakers = %#v", breakerEvent.FailureBreakers)
	}
	if len(breakerEvent.BackendOutages) != 1 || breakerEvent.BackendOutages[0].Provider != "anthropic" || breakerEvent.BackendOutages[0].ResetAt == nil {
		t.Fatalf("BackendOutages = %#v", breakerEvent.BackendOutages)
	}
	item := breakerEvent.FailureBreakers[0].Items[0]
	if item.Title != "Author beat visuals" || item.IssueURL == "" || !item.Parked {
		t.Fatalf("breaker item = %#v", item)
	}
}

func TestManagerSuppressesFlapInsideDebounce(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 18, 0, 0, 0, time.UTC)
	stateStore := newMemoryStateStore()
	sender := &recordingSender{}
	manager := newTestManager(t, stateStore, sender, Config{Debounce: 10 * time.Minute})
	health := readyHealth("detent")
	active := dispatchSnapshot("detent", "provider_rate_window_backpressure")

	managerReconcile(t, manager, active, health, now)
	managerReconcile(t, manager, telemetry.Snapshot{}, health, now.Add(5*time.Minute))
	managerReconcile(t, manager, active, health, now.Add(6*time.Minute))
	managerReconcile(t, manager, telemetry.Snapshot{}, health, now.Add(7*time.Minute))
	managerReconcile(t, manager, telemetry.Snapshot{}, health, now.Add(30*time.Minute))

	if events := sender.Events(); len(events) != 0 {
		t.Fatalf("events = %#v, want none", events)
	}
}

func TestManagerNotifiesOnlyFaultTransitions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	zero := 0
	tests := []struct {
		name       string
		snapshot   telemetry.Snapshot
		wantEvents int
	}{
		{
			name:     "provider pacing is diagnostic",
			snapshot: dispatchSnapshot("detent", "provider_rate_window_backpressure"),
		},
		{
			name:     "human gate is review queue",
			snapshot: dispatchSnapshot("detent", "artifact_gate_wait_status"),
		},
		{
			name: "github rest pause is diagnostic",
			snapshot: telemetry.Snapshot{BackendOutages: []telemetry.BackendOutage{{
				ProjectID: "detent", Kind: "github_rest_rate_limit",
			}}},
		},
		{
			name: "fully parked breaker is diagnostic",
			snapshot: telemetry.Snapshot{FailureBreakers: []telemetry.FailureBreaker{{
				ProjectID: "detent", EligibleCandidateCount: &zero, Items: []telemetry.FailureBreakerItem{{IssueID: "issue-1", Parked: true}},
			}}},
		},
		{
			name:       "total selector exclusion is fault",
			snapshot:   dispatchSnapshot("detent", "authorization_selector_declined"),
			wantEvents: 2,
		},
		{
			name: "provider exhaustion is fault",
			snapshot: telemetry.Snapshot{BackendOutages: []telemetry.BackendOutage{{
				ProjectID: "detent", Kind: "usage_limit_exceeded",
			}}},
			wantEvents: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stateStore := newMemoryStateStore()
			sender := &recordingSender{}
			manager := newTestManager(t, stateStore, sender, Config{Debounce: time.Minute})
			health := readyHealth("detent")

			managerReconcile(t, manager, tt.snapshot, health, now)
			managerReconcile(t, manager, tt.snapshot, health, now.Add(time.Minute))

			if events := sender.Events(); len(events) != tt.wantEvents {
				t.Fatalf("events = %#v, want %d", events, tt.wantEvents)
			}
		})
	}
}

func TestManagerPairsOneRecoveryWithEachEntry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 18, 0, 0, 0, time.UTC)
	stateStore := newMemoryStateStore()
	sender := &recordingSender{}
	manager := newTestManager(t, stateStore, sender, Config{Debounce: 10 * time.Minute})
	health := readyHealth("detent")
	active := dispatchSnapshot("detent", "authorization_selector_declined")

	managerReconcile(t, manager, active, health, now)
	managerReconcile(t, manager, active, health, now.Add(10*time.Minute))
	managerReconcile(t, manager, telemetry.Snapshot{}, health, now.Add(11*time.Minute))
	managerReconcile(t, manager, telemetry.Snapshot{}, health, now.Add(21*time.Minute))
	managerReconcile(t, manager, telemetry.Snapshot{}, health, now.Add(31*time.Minute))

	byIdentity := map[string][]Event{}
	for _, event := range sender.Events() {
		byIdentity[event.Identity] = append(byIdentity[event.Identity], event)
	}
	if len(byIdentity) != 2 {
		t.Fatalf("events by identity = %#v, want project and fleet", byIdentity)
	}
	for identity, events := range byIdentity {
		if len(events) != 2 || events[0].Transition != TransitionEntry || events[1].Transition != TransitionRecovery {
			t.Fatalf("events[%s] = %#v, want entry then recovery", identity, events)
		}
		if events[0].ID == events[1].ID || !events[1].NeedsAttentionEnteredAt.Equal(events[0].NeedsAttentionEnteredAt) {
			t.Fatalf("events[%s] pairing = %#v", identity, events)
		}
		if events[1].State != "ready" && events[1].State != "ok" {
			t.Fatalf("events[%s] recovery state = %q", identity, events[1].State)
		}
		if !events[1].EnteredAt.Equal(now.Add(11 * time.Minute)) {
			t.Fatalf("events[%s] recovery entered_at = %s", identity, events[1].EnteredAt)
		}
	}
}

func TestManagerNotifiesRefreshFailureAndRecovery(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 15, 0, 0, 0, time.UTC)
	lastRefreshAt := now.Add(-time.Minute)
	lastErrorAt := now.Add(-30 * time.Second)
	failed := telemetry.Snapshot{Projects: []telemetry.ProjectSnapshot{{
		Project: telemetry.Project{ID: "pyroapex"},
		Refresh: telemetry.Refresh{
			FailureThreshold: 3,
			LastRefreshAt:    &lastRefreshAt,
			LastError:        "candidate endpoint unavailable",
			LastErrorAt:      &lastErrorAt,
			Sources: []telemetry.RefreshSource{{
				Name: telemetry.RefreshSourceCandidates, FailureStreak: 3, LastError: "candidate endpoint unavailable", LastErrorAt: &lastErrorAt,
			}},
		},
	}}}
	recoveredAt := now.Add(2 * time.Minute)
	recovered := telemetry.Snapshot{Projects: []telemetry.ProjectSnapshot{{
		Project: telemetry.Project{ID: "pyroapex"},
		Refresh: telemetry.Refresh{
			FailureThreshold: 3,
			LastRefreshAt:    &recoveredAt,
			Sources: []telemetry.RefreshSource{{
				Name: telemetry.RefreshSourceCandidates, LastSuccessAt: &recoveredAt,
			}},
		},
	}}}
	stateStore := newMemoryStateStore()
	sender := &recordingSender{}
	manager := newTestManager(t, stateStore, sender, Config{Debounce: time.Minute})
	health := readyHealth("pyroapex")

	managerReconcile(t, manager, failed, health, now)
	managerReconcile(t, manager, failed, health, now.Add(time.Minute))
	managerReconcile(t, manager, recovered, health, now.Add(2*time.Minute))
	managerReconcile(t, manager, recovered, health, now.Add(3*time.Minute))

	events := sender.Events()
	if len(events) != 4 {
		t.Fatalf("events = %#v, want project and fleet entry plus recovery", events)
	}
	for _, event := range events {
		if !slices.Equal(event.Causes, []string{CauseRefreshFailure}) {
			t.Fatalf("event causes = %v, want refresh failure", event.Causes)
		}
		if len(event.RefreshFailures) != 1 || event.RefreshFailures[0].ProjectID != "pyroapex" || event.RefreshFailures[0].LastError != "candidate endpoint unavailable" {
			t.Fatalf("event refresh failures = %#v", event.RefreshFailures)
		}
	}
}

func TestManagerFleetRecoveryWaitsForEveryProjectCause(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 18, 0, 0, 0, time.UTC)
	stateStore := newMemoryStateStore()
	sender := &recordingSender{}
	manager := newTestManager(t, stateStore, sender, Config{Debounce: 10 * time.Minute})
	health := readyHealth("detent")
	both := dispatchSnapshot("detent", "authorization_selector_declined")
	both.CIUnavailable = []telemetry.CICondition{{ProjectID: "detent"}}
	ciOnly := telemetry.Snapshot{CIUnavailable: []telemetry.CICondition{{ProjectID: "detent"}}}

	managerReconcile(t, manager, both, health, now)
	managerReconcile(t, manager, both, health, now.Add(10*time.Minute))
	managerReconcile(t, manager, ciOnly, health, now.Add(11*time.Minute))
	managerReconcile(t, manager, ciOnly, health, now.Add(21*time.Minute))

	events := sender.Events()
	if len(events) != 4 {
		t.Fatalf("events after one cause clears = %#v, want three entries and project dispatch recovery", events)
	}
	for _, event := range events {
		if event.Scope == ScopeFleet && event.Transition == TransitionRecovery {
			t.Fatalf("fleet recovered while CI cause remained active: %#v", event)
		}
	}

	managerReconcile(t, manager, telemetry.Snapshot{}, health, now.Add(22*time.Minute))
	managerReconcile(t, manager, telemetry.Snapshot{}, health, now.Add(32*time.Minute))
	var fleetRecoveries int
	for _, event := range sender.Events() {
		if event.Scope == ScopeFleet && event.Transition == TransitionRecovery {
			fleetRecoveries++
		}
	}
	if fleetRecoveries != 1 {
		t.Fatalf("fleet recoveries = %d, want 1", fleetRecoveries)
	}
}

func TestManagerRestartPreservesActiveAndPendingDelivery(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 18, 0, 0, 0, time.UTC)
	stateStore := newMemoryStateStore()
	failing := &recordingSender{err: errors.New("receiver unavailable")}
	cfg := Config{Debounce: 10 * time.Minute, RetryBase: time.Minute, RetryMax: time.Minute}
	first := newTestManager(t, stateStore, failing, cfg)
	health := readyHealth("detent")
	active := dispatchSnapshot("detent", "authorization_selector_declined")

	managerReconcile(t, first, active, health, now)
	managerReconcile(t, first, active, health, now.Add(10*time.Minute))
	if got := len(failing.Events()); got != 2 {
		t.Fatalf("first manager attempts = %d, want 2", got)
	}
	failures, err := first.Failures(t.Context())
	if err != nil || len(failures) != 2 {
		t.Fatalf("Failures() = %#v, %v", failures, err)
	}

	succeeding := &recordingSender{}
	second := newTestManager(t, stateStore, succeeding, cfg)
	managerReconcile(t, second, active, health, now.Add(11*time.Minute))
	managerReconcile(t, second, active, health, now.Add(20*time.Minute))
	if got := len(succeeding.Events()); got != 2 {
		t.Fatalf("second manager deliveries = %d, want preserved project and fleet entries only", got)
	}
}

func TestManagerRestartDoesNotRefireDeliveredActiveCondition(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 18, 0, 0, 0, time.UTC)
	stateStore := newMemoryStateStore()
	firstSender := &recordingSender{}
	cfg := Config{Debounce: 10 * time.Minute}
	first := newTestManager(t, stateStore, firstSender, cfg)
	health := readyHealth("detent")
	active := dispatchSnapshot("detent", "authorization_selector_declined")

	managerReconcile(t, first, active, health, now)
	managerReconcile(t, first, active, health, now.Add(10*time.Minute))
	if got := len(firstSender.Events()); got != 2 {
		t.Fatalf("first manager deliveries = %d, want project and fleet entries", got)
	}

	secondSender := &recordingSender{}
	second := newTestManager(t, stateStore, secondSender, cfg)
	managerReconcile(t, second, active, health, now.Add(20*time.Minute))
	if events := secondSender.Events(); len(events) != 0 {
		t.Fatalf("events after restart = %#v, want none", events)
	}
}

func TestManagerDeliveryRetriesAreBoundedAndSurfaced(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 18, 0, 0, 0, time.UTC)
	stateStore := newMemoryStateStore()
	sender := &recordingSender{err: errors.New("receiver unavailable")}
	manager := newTestManager(t, stateStore, sender, Config{
		Debounce:    10 * time.Minute,
		MaxAttempts: 2,
		RetryBase:   time.Minute,
		RetryMax:    time.Minute,
	})
	health := readyHealth("detent")
	active := dispatchSnapshot("detent", "authorization_selector_declined")

	managerReconcile(t, manager, active, health, now)
	managerReconcile(t, manager, active, health, now.Add(10*time.Minute))
	managerReconcile(t, manager, active, health, now.Add(10*time.Minute+30*time.Second))
	if got := len(sender.Events()); got != 2 {
		t.Fatalf("attempts before backoff = %d, want 2", got)
	}
	managerReconcile(t, manager, active, health, now.Add(11*time.Minute))
	managerReconcile(t, manager, active, health, now.Add(30*time.Minute))
	if got := len(sender.Events()); got != 4 {
		t.Fatalf("bounded attempts = %d, want 4", got)
	}
	failures, err := manager.Failures(t.Context())
	if err != nil {
		t.Fatalf("Failures() error = %v", err)
	}
	if len(failures) != 2 {
		t.Fatalf("failures = %#v, want project and fleet", failures)
	}
	for _, failure := range failures {
		if failure.Attempts != 2 || failure.MaxAttempts != 2 || failure.FailedAt == nil || failure.NextAttemptAt != nil || failure.LastError != "receiver unavailable" {
			t.Fatalf("failure = %#v", failure)
		}
	}
}

func TestManagerSkipsUnchangedStateWrites(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 18, 0, 0, 0, time.UTC)
	stateStore := newMemoryStateStore()
	manager := newTestManager(t, stateStore, &recordingSender{}, Config{Debounce: 10 * time.Minute})
	health := readyHealth("detent")

	managerReconcile(t, manager, telemetry.Snapshot{}, health, now)
	if got := stateStore.SaveCalls(); got != 1 {
		t.Fatalf("save calls after initial observation = %d, want 1", got)
	}
	managerReconcile(t, manager, telemetry.Snapshot{}, health, now.Add(time.Second))
	if got := stateStore.SaveCalls(); got != 1 {
		t.Fatalf("save calls after unchanged observation = %d, want 1", got)
	}
}

func TestNewManagerUnconfiguredIsSilent(t *testing.T) {
	t.Parallel()
	manager, err := NewManager(Config{}, Dependencies{})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if manager == nil || manager.Enabled() {
		t.Fatalf("NewManager() = %#v, want disabled manager", manager)
	}
	if err := manager.Reconcile(t.Context(), telemetry.Snapshot{}, nil, time.Now()); err != nil {
		t.Fatalf("disabled Reconcile() error = %v", err)
	}
}

type memoryStateStore struct {
	mu        sync.Mutex
	records   map[string]store.HealthNotificationState
	saveCalls int
}

func newMemoryStateStore() *memoryStateStore {
	return &memoryStateStore{records: map[string]store.HealthNotificationState{}}
}

func (s *memoryStateStore) ListHealthNotificationStates(context.Context) ([]store.HealthNotificationState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]store.HealthNotificationState, 0, len(s.records))
	for _, record := range s.records {
		record.StateJSON = append([]byte(nil), record.StateJSON...)
		records = append(records, record)
	}
	return records, nil
}

func (s *memoryStateStore) SaveHealthNotificationStates(_ context.Context, records []store.HealthNotificationState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveCalls++
	for _, record := range records {
		record.StateJSON = append([]byte(nil), record.StateJSON...)
		s.records[record.Identity] = record
	}
	return nil
}

func (s *memoryStateStore) SaveCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveCalls
}

type recordingSender struct {
	mu     sync.Mutex
	events []Event
	err    error
}

func (s *recordingSender) Send(_ context.Context, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	event, ok := payload.(Event)
	if !ok {
		return errors.New("unexpected payload type")
	}
	s.events = append(s.events, event)
	return s.err
}

func (s *recordingSender) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

func newTestManager(t *testing.T, stateStore store.HealthNotificationStateStore, sender notify.Sender, cfg Config) *Manager {
	t.Helper()
	cfg.Webhook.URL = "https://alerts.example.test/detent"
	cfg.Instance = "buildbox"
	cfg.Host = "buildbox.example.test"
	manager, err := NewManager(cfg, Dependencies{
		Store:  stateStore,
		Sender: sender,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	return manager
}

func managerReconcile(t *testing.T, manager *Manager, snapshot telemetry.Snapshot, health []project.Health, now time.Time) {
	t.Helper()
	snapshot.GeneratedAt = now
	snapshot.Seq = 1
	if err := manager.Reconcile(t.Context(), snapshot, health, now); err != nil {
		t.Fatalf("Reconcile(%s) error = %v", now, err)
	}
}

func readyHealth(projectID string) []project.Health {
	return []project.Health{{
		Project: globalconfig.Project{ID: projectID},
		Status:  project.HealthStatusReady,
	}}
}

func dispatchSnapshot(projectID string, waitReason string) telemetry.Snapshot {
	return telemetry.Snapshot{DispatchStalls: []telemetry.DispatchStatus{{
		ProjectID:      projectID,
		Stalled:        true,
		WaitReason:     waitReason,
		WaitReasonCode: waitReason,
	}}}
}

func (e Event) ScopeCompare(other Event) int {
	if e.Scope == other.Scope {
		return 0
	}
	if e.Scope == ScopeFleet {
		return -1
	}
	return 1
}
