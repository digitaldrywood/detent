package hubserver

import (
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestClaimCandidatesRetryInstancePreProviderFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		failure       tracker.NativeTerminalFailure
		preProvider   bool
		aged          bool
		wantCandidate bool
	}{
		{name: "after_create hook failure retries", failure: tracker.NativeTerminalFailure{Error: "workspace hook after_create failed (exit status 1)", ErrorClass: "workspace_hook"}, preProvider: true, wantCandidate: true},
		{name: "published branch lookup failure retries", failure: tracker.NativeTerminalFailure{Error: "workspace preparation failed: create workspace: inspect published workspace branch: git ls-remote origin failed: exit status 128", ErrorClass: "workspace_hook"}, preProvider: true, wantCandidate: true},
		{name: "checkpoint fetch failure retries", failure: tracker.NativeTerminalFailure{Error: "workspace preparation failed: create workspace: fetch checkpoint: git fetch --no-tags failed: exit status 128", ErrorClass: "workspace_hook"}, preProvider: true, wantCandidate: true},
		{name: "aged instance failure retries", failure: tracker.NativeTerminalFailure{Error: "workspace hook after_create failed (exit status 1)", ErrorClass: "workspace_hook"}, preProvider: true, aged: true, wantCandidate: true},
		{name: "issue attributed failure keeps retrying", failure: tracker.NativeTerminalFailure{Error: "native checkpoint requires recovery", ErrorClass: "source_recovery"}, preProvider: true, wantCandidate: true},
		{name: "unclassified failure keeps retrying", failure: tracker.NativeTerminalFailure{Error: "unknown setup error"}, preProvider: true, wantCandidate: true},
		{name: "provider failure keeps retrying", failure: tracker.NativeTerminalFailure{Error: "workspace hook before_run failed (exit status 1)", ErrorClass: "workspace_hook"}, wantCandidate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDispatchGuardFixture(t)
			f.succeed(t, &tracker.NativeDisposition{Status: "complete"})
			if !f.candidate(t) {
				t.Fatal("unaccepted completion is not claimable before the failure")
			}

			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, tracker.NativeClaim{PolicyID: f.policy, WorkItemID: f.issue.WorkItemID, MachineID: "conversation-machine", SessionID: newNativeID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &f.lease)
			f.attempt, f.run = newNativeID("attempt"), newNativeID("run")
			failure := test.failure
			failure.ObservedAt = f.now
			event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("finish")}, Type: "run.finished", SchemaVersion: 1, Data: tracker.NativeRunData{
				LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "failed", TerminalFailure: &failure,
			}}
			if test.preProvider {
				event.Data.Sequence = 1
				event.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 7, Generation: 3, Phase: "completed", HeartbeatAt: f.now}
			} else {
				event.Data.Identity = &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}
				started := event
				started.IdempotencyKey, started.Type, started.Data.Sequence, started.Data.Outcome, started.Data.TerminalFailure = newNativeID("start"), "run.started", 1, "", nil
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, started), http.StatusOK)
				event.Data.Sequence = 2
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event), http.StatusOK)
			f.release(t)
			if test.aged {
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET updated_at = ? WHERE id = ?`, formatHubTime(time.Now().UTC().Add(-time.Hour)), f.attempt); err != nil {
					t.Fatal(err)
				}
			}

			var status, stage, attribution string
			if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT status, COALESCE(json_extract(data_json, '$.terminal_failure.stage'), ''), COALESCE(json_extract(data_json, '$.terminal_failure.attribution'), '') FROM native_attempts WHERE id = ?`, f.attempt).Scan(&status, &stage, &attribution); err != nil {
				t.Fatal(err)
			}
			if status != "failed" {
				t.Fatalf("attempt status = %q, want failed", status)
			}
			if test.preProvider != (stage == "pre_provider") {
				t.Fatalf("terminal failure stage = %q, pre-provider %t", stage, test.preProvider)
			}
			if got := f.candidate(t); got != test.wantCandidate {
				t.Fatalf("candidate = %t, want %t (attribution %q)", got, test.wantCandidate, attribution)
			}

			r := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO lease_runners(lease_id,runner_id) VALUES(?,?)", f.lease.ID, r.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			attempts, err := readHealthRateAttempts(t.Context(), f.service.database.db, f.scope(), operatortool.AnalyticsWindow{From: time.Now().UTC().Add(-2 * time.Hour), To: time.Now().UTC().Add(time.Minute)})
			if err != nil {
				t.Fatal(err)
			}
			wantSetup := test.preProvider && attribution == "instance"
			found := false
			for _, a := range attempts {
				if a.ID == f.attempt {
					found = true
					if a.Runner != r.binding.RunnerID || a.InstanceSetupFailure != wantSetup {
						t.Fatalf("health attempt runner = %q setup failure = %t, want %s and %t", a.Runner, a.InstanceSetupFailure, r.binding.RunnerID, wantSetup)
					}
				}
			}
			if !found {
				t.Fatalf("health attempts omit the failed attempt %s", f.attempt)
			}
		})
	}
}
