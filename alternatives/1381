package hubserver

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestInstanceStepSuccessesAreScopedAndBounded(t *testing.T) {
	f := newNativeFixture(t, nil, "", "failure-evidence")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	r.enroll(t)
	now := f.service.config.now().UTC()
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	create := func(title string) tracker.NativeIssue {
		issue, err := createNativeIssueDraft(t.Context(), tx, scope, tracker.CreateIssue{Body: nativeContractTestBody, Title: title, State: "Todo"}, now, false, false)
		if err != nil {
			t.Fatal(err)
		}
		return issue
	}
	current, other := create("current"), create("other")
	var wanted string
	for _, test := range []struct {
		name           string
		item           tracker.NativeIssue
		runner, status string
		at             time.Time
	}{
		{"valid", other, r.binding.RunnerID, "succeeded", now.Add(-time.Minute)},
		{"same issue", current, r.binding.RunnerID, "succeeded", now.Add(-time.Minute)},
		{"other runner", other, "runner_other", "succeeded", now.Add(-time.Minute)},
		{"old", other, r.binding.RunnerID, "succeeded", now.Add(-time.Hour)},
		{"future", other, r.binding.RunnerID, "succeeded", now.Add(time.Hour)},
		{"failed", other, r.binding.RunnerID, "failed", now.Add(-time.Minute)},
	} {
		data := tracker.NativeRunData{RunnerID: test.runner, MachineID: r.binding.MachineID, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "example", Model: "example"}, Steps: []tracker.NativeStepReceipt{{Step: "validation", CompletedAt: test.at, Validation: &gate.CommandResult{ExitCode: 0}}}}
		id := seedHealthRateAttempt(t, tx, scope, test.item, string(r.binding.MachineID), test.status, data, test.at, -1)
		if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET runner_id=? WHERE id=?", test.runner, id); err != nil {
			t.Fatal(err)
		}
		if test.name == "valid" {
			wanted = id
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	successes, err := f.service.readInstanceStepSuccesses(t.Context(), scope, string(current.WorkItemID), r.binding.RunnerID)
	if err != nil || len(successes) != 2 {
		t.Fatalf("successes=%+v error=%v", successes, err)
	}
	for _, success := range successes {
		if success.AttemptID != wanted || success.WorkItemID != other.WorkItemID || success.MachineID != r.binding.MachineID {
			t.Fatalf("unrelated success included: %+v", success)
		}
	}
	if successes[0].Step != "workspace_preparation" || successes[1].Step != "validation" {
		t.Fatalf("step receipts lost: %+v", successes)
	}
	otherScope := scope
	otherScope.project = "prj_other"
	if got, err := f.service.readInstanceStepSuccesses(t.Context(), otherScope, string(current.WorkItemID), r.binding.RunnerID); err != nil || len(got) != 0 {
		t.Fatalf("cross-project successes=%+v error=%v", got, err)
	}
	tx, err = f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range 16 {
		id := seedHealthRateAttempt(t, tx, scope, other, string(r.binding.MachineID), "failed", tracker.NativeRunData{}, now.Add(-time.Duration(i)*time.Millisecond), -1)
		if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET runner_id=? WHERE id=?", r.binding.RunnerID, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := f.service.readInstanceStepSuccesses(t.Context(), scope, string(current.WorkItemID), r.binding.RunnerID); err != nil || len(got) != 0 {
		t.Fatalf("history exceeded 16 attempts: %s error=%v", fmt.Sprint(got), err)
	}
}

func TestPreProviderFailureDecisionRetainsUsage(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name, outcome string
		allow         bool
	}{
		{"instance", "instance", true}, {"issue", "issue", true}, {"unknown", "unknown", true}, {"invalid", "park", false}, {"absent", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := &tracker.NativeTerminalFailure{ObservedAt: now}
			if test.outcome != "" {
				failure.Decision = &tracker.FailureAttribution{Outcome: test.outcome, Reason: "recorded decision", Evidence: tracker.FailureEvidence{Step: "checkout"}}
			}
			data := tracker.NativeRunData{Sequence: 1, Outcome: "failed", TerminalFailure: failure, Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: 1, Generation: 1, HeartbeatAt: now, Phase: "completed"}, Usage: []tracker.NativeUsage{{Provider: "example", Model: "example", Input: 10, Output: 1}}}
			err := validateNativeExecution(data, "run.finished")
			if (err == nil) != test.allow {
				t.Fatalf("allowed=%v error=%v", test.allow, err)
			}
			if test.allow && (failure.Stage != "pre_provider" || failure.Attribution != test.outcome) {
				t.Fatalf("attribution lost: %+v", failure)
			}
		})
	}
}
