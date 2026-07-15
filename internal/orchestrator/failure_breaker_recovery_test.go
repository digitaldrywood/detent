package orchestrator

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestImmediateFailureBreakerCanaryReleasesRetriesAndKeepsBreakerActive(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 15, 15, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 4,
		FailureBreaker: FailureBreakerConfig{
			SameClassLimit: 2,
			Window:         time.Hour,
			Cooldown:       time.Hour,
		},
	})
	orch := &Orchestrator{cfg: cfg}
	state := newState(cfg)
	state.FailureBreaker.Class = "backend_startup_timeout"
	state.FailureBreaker.ResumeAt = now.Add(time.Hour)
	for _, id := range []string{"issue-1", "issue-2"} {
		state.Retry[id] = Retry{Issue: connector.Issue{ID: id, State: "In Progress"}, Attempt: 2, DueAt: now.Add(time.Hour)}
	}
	state.Retry["issue-later"] = Retry{Issue: connector.Issue{ID: "issue-later", State: "In Progress"}, Attempt: 2, DueAt: now.Add(2 * time.Hour)}

	result := orch.requestProjectFailureBreakerCanary(&state, now)
	if !result.Requested || !result.Active || !result.ResumeAt.Equal(now) || !state.FailureBreaker.Active() {
		t.Fatalf("request result/state = %#v/%#v", result, state.FailureBreaker)
	}
	for _, issueID := range []string{"issue-1", "issue-2"} {
		retry := state.Retry[issueID]
		if !retry.DueAt.Equal(now) {
			t.Fatalf("Retry[%q].DueAt = %s, want %s", issueID, retry.DueAt, now)
		}
	}
	if retry := state.Retry["issue-later"]; !retry.DueAt.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("Retry[issue-later].DueAt = %s, want unrelated backoff preserved", retry.DueAt)
	}
	if canary, allowed := tryReserveProjectFailureBreakerCanary(&state, "issue-1", now); !canary || !allowed {
		t.Fatalf("first reservation = %v, %v, want canary", canary, allowed)
	}
	if canary, allowed := tryReserveProjectFailureBreakerCanary(&state, "issue-2", now); canary || allowed {
		t.Fatalf("second reservation = %v, %v, want denied", canary, allowed)
	}
	if !state.FailureBreaker.Active() {
		t.Fatal("breaker cleared before canary progress")
	}
	var capacity map[string]any
	if err := json.Unmarshal([]byte(orch.capacitySnapshotJSON(&state, state.Retry["issue-1"].Issue)), &capacity); err != nil {
		t.Fatalf("capacitySnapshotJSON() error = %v", err)
	}
	breaker, ok := capacity["project_failure_breaker"].(map[string]any)
	if !ok || breaker["class"] != "backend_startup_timeout" || breaker["canary_issue_id"] != "issue-1" {
		t.Fatalf("project_failure_breaker = %#v, want active canary telemetry", capacity["project_failure_breaker"])
	}
}

func TestFailureBreakerCanaryProgressClosesIntoRecoveryRamp(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 15, 16, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{MaxConcurrentAgents: 4})
	orch := &Orchestrator{cfg: cfg}
	state := newState(cfg)
	state.FailureBreaker.Class = "runner_error:abc"
	state.FailureBreaker.CanaryIssueID = "issue-canary"
	state.FailureBreaker.ResumeAt = now

	orch.recordProjectFailureBreakerProgress(&state, "issue-canary", now.Add(time.Second))

	if state.FailureBreaker.Active() {
		t.Fatalf("FailureBreaker = %#v, want closed", state.FailureBreaker)
	}
	recovery := state.DispatchRecoveries[dispatchRecoveryProjectFailureBreaker]
	if recovery.Status != dispatchRecoveryStatusRamping || recovery.Limit != 2 || !recovery.Admissions["issue-canary"] {
		t.Fatalf("breaker recovery = %#v, want gradual ramp", recovery)
	}
	if _, allowed, reason := tryReserveDispatchRecovery(&state, "issue-2", now.Add(time.Second)); !allowed || reason != "" {
		t.Fatalf("second ramp admission = %v, %q, want allowed", allowed, reason)
	}
	if _, allowed, reason := tryReserveDispatchRecovery(&state, "issue-3", now.Add(time.Second)); allowed || reason != "project_failure_breaker_recovery" {
		t.Fatalf("third ramp admission = %v, %q, want bounded", allowed, reason)
	}
}

func TestMatchingFailureBreakerCanaryFailureRestartsCooldown(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 15, 17, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{FailureBreaker: FailureBreakerConfig{SameClassLimit: 2, Window: time.Hour, Cooldown: 5 * time.Minute}})
	orch := &Orchestrator{cfg: cfg}
	state := newState(cfg)
	class := "backend_startup_timeout"
	state.FailureBreaker.Class = class
	state.FailureBreaker.CanaryIssueID = "issue-canary"
	state.FailureBreaker.Failures[class] = []ProjectFailure{{IssueID: "issue-first", At: now.Add(-time.Minute)}}

	orch.recordProjectFailureBreakerFailure(&state, "issue-canary", class, now)

	if !state.FailureBreaker.Active() || state.FailureBreaker.CanaryIssueID != "" || state.FailureBreaker.Count != 2 || !state.FailureBreaker.ResumeAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("FailureBreaker = %#v, want retripped cooldown", state.FailureBreaker)
	}
	if projectFailureBreakerAllowsDispatch(&state, now.Add(time.Minute)) {
		t.Fatal("matching canary failure released another queued attempt")
	}
	if got := projectAttemptFailureClass(errors.New("startup timeout"), "timed_out", class, "startup timeout"); got == "" {
		t.Fatal("startup timeout did not retain a project failure class")
	}
}
