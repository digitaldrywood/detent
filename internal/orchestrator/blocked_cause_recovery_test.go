package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestNativeBlockedMaintenanceSkipsUnfencedRetry(t *testing.T) {
	issue := connector.Issue{ID: "item", State: "Blocked"}
	tracker := &nativeWorkflowConnector{beforeStates: func() { t.Error("blocked maintenance called the legacy retry path") }}
	orch := &Orchestrator{connector: tracker}
	state := newState(orch.cfg)
	if transitioned := orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, time.Now()); len(transitioned) != 0 || len(state.Retry) != 0 {
		t.Fatalf("blocked maintenance transitioned %v and queued %v", transitioned, state.Retry)
	}
}

func TestBlockedRecoveryMetadataSeparatesIntentFromReachability(t *testing.T) {
	t.Parallel()

	issue := dependencyAutoUnblockIssue("issue-resume-intent", blockedStatusState)
	orch := blockedCauseTestOrchestrator(&dependencyAutoUnblockConnector{})
	orch.recoveryInspector = staticBlockedRecoveryInspector{present: true, changed: true}
	metadata := orch.newBlockedRecoveryMetadata(
		t.Context(),
		issue,
		RunModeImplement,
		workpadBlockedUnactionedReason,
		"Todo",
		DiffStats{},
	)
	raw := workflowLaneMetadataJSON(issue, metadata)
	if !strings.Contains(raw, `"intent_resumable":true`) {
		t.Fatalf("metadata = %s", raw)
	}
	if strings.Contains(raw, `"cause_fingerprint"`) || strings.Contains(raw, `"cause_fingerprint_version"`) {
		t.Fatalf("metadata = %s", raw)
	}
	if strings.Contains(raw, `"resumable":true`) {
		t.Fatalf("metadata retains ambiguous resumable field: %s", raw)
	}
}

func blockedCauseTestOrchestrator(tracker *dependencyAutoUnblockConnector) *Orchestrator {
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{})
	orch.cfg.BlockedRecovery.Enabled = false
	orch.cfg.StatusLabelPrefix = "detent:"
	return orch
}

type staticBlockedRecoveryInspector struct {
	fingerprint string
	present     bool
	changed     bool
}

func (i staticBlockedRecoveryInspector) BlockedRecoveryWorkspace(context.Context, runpkg.RunRequest) (bool, bool) {
	return i.present, i.changed
}

func (i staticBlockedRecoveryInspector) BlockedRecoveryConfigFingerprint() string {
	return i.fingerprint
}
