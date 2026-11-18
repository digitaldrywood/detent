package hubserver

import (
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
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
		{name: "shared memory exhaustion retries", failure: tracker.NativeTerminalFailure{Error: "host shared memory exhausted", ErrorClass: tracker.WorkspaceSharedMemoryExhausted, Stage: "workspace_preparation"}, preProvider: true, wantCandidate: true},
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

			r := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.enroll(t)
			reportRunnerCapacity(t, f.service, f.base, r.redemption.Credential, r.binding.MachineID, r.redemption.DisplayName, r.redemption.BackendIsolation, r.redemption.Capacity)
			f.worker = r.redemption.Credential
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, tracker.NativeClaim{PolicyID: f.policy, WorkItemID: f.issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: hubapi.NewID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &f.lease)
			f.attempt, f.run = hubapi.NewID("attempt"), hubapi.NewID("run")
			test.failure.ObservedAt = f.now
			event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("finish")}, Type: "run.finished", SchemaVersion: 1, Data: tracker.NativeRunData{
				LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "failed", TerminalFailure: &test.failure,
			}}
			if test.preProvider {
				event.Data.Sequence = 1
				event.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 7, Generation: 3, Phase: "completed", HeartbeatAt: f.now}
			} else {
				event.Data.Identity = &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}
				started := event
				started.IdempotencyKey, started.Type, started.Data.Sequence, started.Data.Outcome, started.Data.TerminalFailure = hubapi.NewID("start"), "run.started", 1, "", nil
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, started), http.StatusOK)
				event.Data.Sequence = 2
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event), http.StatusOK)
			f.release(t)
			if test.aged {
				result, err := nativeAttemptTestDatabase(t, f.nativeFixture).ExecContext(t.Context(), "UPDATE native_attempts SET updated_at=$1 WHERE organization_id=$2 AND id=$3", f.now.Add(-time.Hour), f.project.OrganizationID, f.attempt)
				if err != nil {
					t.Fatal(err)
				}
				if count, err := result.RowsAffected(); err != nil || count != 1 {
					t.Fatalf("aged attempt fixture affected%d rows: %v", count, err)
				}
			}

			attempt, err := f.service.readNativeAttempt(t.Context(), f.scope(), string(f.issue.WorkItemID), f.attempt)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.Status != "failed" {
				t.Fatalf("attempt status = %q, want failed", attempt.Status)
			}
			if attempt.TerminalFailure == nil {
				t.Fatal("failed attempt lost terminal failure detail")
			}
			if tracker.WorkspaceResourceMessage(test.failure.ErrorClass) != "" && (attempt.TerminalFailure.Error != test.failure.Error || attempt.TerminalFailure.ErrorClass != test.failure.ErrorClass || attempt.TerminalFailure.Attribution != "instance") {
				t.Fatalf("persisted hook failure = %#v", attempt.TerminalFailure)
			}
			stage, attribution := attempt.TerminalFailure.Stage, attempt.TerminalFailure.Attribution
			if test.preProvider != (stage == "pre_provider") {
				t.Fatalf("terminal failure stage = %q, pre-provider %t", stage, test.preProvider)
			}
			if got := f.candidate(t); got != test.wantCandidate {
				t.Fatalf("candidate = %t, want %t (attribution %q)", got, test.wantCandidate, attribution)
			}

			source, err := f.service.readHealthProjectSource(t.Context(), f.project.OrganizationID, f.project.ID, "", f.now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if !source.attemptsKnown {
				t.Fatal("health source did not retain complete typed attempt facts")
			}
			wantSetup := test.preProvider && attribution == "instance"
			found := false
			for _, a := range source.rate.Attempts {
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
