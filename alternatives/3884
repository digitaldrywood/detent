package runner

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeResumeSessionStore struct {
	*fakeSessionStore
	policy policy.Descriptor
}

func (s nativeResumeSessionStore) SessionPolicy(context.Context, string, int64) (policy.Descriptor, error) {
	return s.policy, nil
}

func TestNativeSessionResumeUsesHubAttemptIdentity(t *testing.T) {
	for _, machine := range []tracker.MachineID{"machine_original", "machine_replacement"} {
		t.Run(string(machine), func(t *testing.T) {
			approved := runnerTestPolicy()
			sessions := &fakeSessionStore{resumeState: store.AgentResumeState{DetentSessionID: 91, ProviderThreadID: "persisted-thread", ProviderSessionID: "persisted-session", Orphaned: true}}
			r := &Runner{store: nativeResumeSessionStore{fakeSessionStore: sessions, policy: approved}, projectID: "example", logger: slog.New(slog.DiscardHandler)}
			previous := tracker.NativeAttempt{Status: "cancelled", NativeRunData: tracker.NativeRunData{AttemptID: "attempt_source", MachineID: "machine_original", Identity: &tracker.NativeExecutionIdentity{Role: RoleCode, Backend: "example", Model: "example-model"}}, Checkpoint: &tracker.NativeCheckpoint{Resume: "resume_session", Storage: "git_ref", Ref: tracker.CheckpointRefPrefix + "source", CommitSHA: "source", TreeSHA: "tree"}}
			execution := &testExecution{recovery: tracker.NativeRecovery{SourceAttemptID: previous.AttemptID, Attempts: []tracker.NativeAttempt{previous}, Lease: tracker.NativeLease{MachineID: machine}}}
			state, err := r.nativeSessionResumeState(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "wi_example"}, Policy: approved}, agentRuntime{backendConfigs: map[string]config.AgentBackend{"example": {Kind: "example"}}})
			if err != nil || state.DetentSessionID != 91 || state.ProviderThreadID != "persisted-thread" || state.ProviderSessionID != "persisted-session" {
				t.Fatalf("Hub session resume lost identity: %+v %v", state, err)
			}
			if sessions.resumeLookup.NativeAttemptID != previous.AttemptID || sessions.resumeLookup.WorkAttemptID != 0 {
				t.Fatalf("resume depended on a local attempt identifier: %+v", sessions.resumeLookup)
			}
			request := RunRequest{Execution: execution, RetryMode: RetryModeResume, ResumeState: state}
			if resumed, err := r.runRequestResumeState(t.Context(), config.Agent{}, request, "example-model", "example", "example", RoleCode); err != nil || resumed.DetentSessionID != state.DetentSessionID {
				t.Fatalf("fenced orphan resume required local generation: %+v %v", resumed, err)
			}
			execution.validateErr = ErrExecutionAuthorityUnavailable
			if _, err := r.runRequestResumeState(t.Context(), config.Agent{}, request, "example-model", "example", "example", RoleCode); !errors.Is(err, ErrExecutionAuthorityUnavailable) {
				t.Fatalf("revoked authority resumed provider: %v", err)
			}
		})
	}
}
