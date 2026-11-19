package hubserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTenantRunReconnectPreservesWorkerCleanupIdentity(t *testing.T) {
	t.Parallel()
	f := newRunnerPGFixture(t)
	f.service.config.Version = "2.0.0"
	for index, lease := range f.leases {
		t.Run([]string{"separate observations", "batched observations"}[index], func(t *testing.T) {
			f.service.config.Version = "2.0.0"
			now := time.Now().UTC()
			launch := tracker.NativeWorkerLaunch{HubVersion: "untrusted", PID: 123, ProcessStart: now, ExecutionVersion: "example", ExecutionCommit: strings.Repeat("a", 40), ExecutionSHA256: strings.Repeat("b", 64), ControlVersion: "example", PolicyID: f.policy.PolicyID, Protocol: 1, SelectedAt: now}
			data := f.data[index]
			data.Sequence = 2
			data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", WorkerLaunch: &launch}
			observed := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "launch_" + data.AttemptID}, Type: "run.observed", SchemaVersion: 1, Data: data}
			reconnect := observed
			reconnect.IdempotencyKey = "reconnect_" + data.AttemptID
			reconnect.Data.Sequence = 3
			reconnect.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation"}
			path := "/api/v2/organizations/org_startup/projects/prj_runner/work-items/" + string(lease.WorkItemID) + "/events"
			if index == 0 {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], observed), http.StatusOK)
			} else {
				reconnect.PriorEvents = []tracker.NativeRunEvent{observed}
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], reconnect), http.StatusOK)
			credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), "org_startup", apikey.HashToken(f.tokens[index]), "", "")
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := f.service.readNativeAttemptTenant(t.Context(), hubscope.NativeScope{Organization: "org_startup", Project: "prj_runner", Credential: credential}, string(lease.WorkItemID), data.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			expectedLaunch := launch
			expectedLaunch.HubVersion = "2.0.0"
			if attempt.Runtime == nil || attempt.Runtime.WorkerLaunch == nil || *attempt.Runtime.WorkerLaunch != expectedLaunch {
				t.Fatalf("reconnect lost archived worker launch: %+v", attempt.Runtime)
			}
			raw, err := json.Marshal(attempt.Runtime.WorkerLaunch)
			if err != nil {
				t.Fatal(err)
			}
			var versions struct {
				Hub    string `json:"hub_version"`
				Worker string `json:"execution_version"`
			}
			if err := json.Unmarshal(raw, &versions); err != nil {
				t.Fatal(err)
			}
			if versions.Hub != "2.0.0" || versions.Worker != "example" {
				t.Fatalf("attempt versions = %+v, want Hub 2.0.0 and worker example", versions)
			}
			f.service.config.Version = "2.1.0"
			prefix := reconnect
			prefix.PriorEvents = nil
			replay := prefix
			replay.IdempotencyKey = "prefix_replay_" + data.AttemptID
			replay.Data.Sequence = 4
			replay.PriorEvents = []tracker.NativeRunEvent{observed, prefix}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], replay), http.StatusOK)
			older := prefix
			older.IdempotencyKey = "older_prefix_" + data.AttemptID
			older.Data.Sequence = 5
			replay.PriorEvents = nil
			older.PriorEvents = []tracker.NativeRunEvent{prefix, replay}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], older), http.StatusOK)
			afterUpgrade, err := f.service.readNativeAttemptTenant(t.Context(), hubscope.NativeScope{Organization: "org_startup", Project: "prj_runner", Credential: credential}, string(lease.WorkItemID), data.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			if afterUpgrade.Runtime.WorkerLaunch.HubVersion != "2.0.0" {
				t.Fatalf("Hub upgrade changed attempt version: %+v", afterUpgrade.Runtime.WorkerLaunch)
			}
			altered := prefix
			altered.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "rework"}
			rejected := older
			rejected.IdempotencyKey = "altered_prefix_" + data.AttemptID
			rejected.Data.Sequence = 6
			rejected.PriorEvents = []tracker.NativeRunEvent{altered}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], rejected), http.StatusConflict)
			changed := reconnect
			changed.IdempotencyKey = "changed_identity_" + data.AttemptID
			changed.PriorEvents = nil
			changed.Data.Sequence = 6
			identity := *changed.Data.Identity
			identity.Model = "different"
			changed.Data.Identity = &identity
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], changed), http.StatusConflict)
			cleared := reconnect
			cleared.IdempotencyKey = "clear_runtime_" + data.AttemptID
			cleared.PriorEvents = nil
			cleared.Data.Sequence = 6
			cleared.Type = "run.finished"
			cleared.Data.Outcome = "cancelled"
			cleared.Data.Runtime = nil
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], cleared), http.StatusConflict)
			cleared.IdempotencyKey = "clear_process_" + data.AttemptID
			invalid := launch
			invalid.PID = 0
			cleared.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", WorkerLaunch: &invalid}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], cleared), http.StatusUnprocessableEntity)

		})
	}
}

func TestWorkerBootstrapBindsIdentityOnce(t *testing.T) {
	t.Parallel()
	placeholder := tracker.WorkerBootstrapIdentity()
	reserved := tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "example"}
	resolved := reserved
	resolved.ReasoningEffort = "high"
	unreserved := reserved
	unreserved.Model = "other"
	for _, test := range []struct {
		name                         string
		before, after                tracker.NativeExecutionIdentity
		starting, continues, refuses bool
	}{
		{name: "unreserved worker binds selected provider", before: placeholder, after: resolved, starting: true},
		{name: "reserved worker binds reasoning effort", before: reserved, after: resolved, starting: true},
		{name: "reserved worker cannot change models", before: reserved, after: unreserved, starting: true, refuses: true},
		{name: "bound worker cannot change identity", before: resolved, after: unreserved, refuses: true},
		{name: "bound worker cannot reenter bootstrap", before: resolved, after: resolved, continues: true, refuses: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			start := time.Unix(1, 0).UTC()
			previous := tracker.NativeRunData{
				RunID: "run_example", AttemptID: "attempt_example", LeaseID: "lease_example", FencingToken: 1,
				Sequence: 1, Identity: &test.before,
				Runtime: &tracker.NativeRuntimeObservation{WorkerLaunch: &tracker.NativeWorkerLaunch{Bootstrap: test.starting, PID: 12, ProcessStart: start}},
			}
			next := previous
			next.Sequence, next.Identity = 2, &test.after
			next.Runtime = &tracker.NativeRuntimeObservation{WorkerLaunch: &tracker.NativeWorkerLaunch{Bootstrap: test.continues, PID: 12, ProcessStart: start}}
			err := preserveTenantRunCleanup(&previous, &next)
			if test.refuses && !errors.Is(err, leasestore.ErrAttemptConflict) || !test.refuses && err != nil {
				t.Fatalf("bootstrap identity update: %v", err)
			}
		})
	}
}
