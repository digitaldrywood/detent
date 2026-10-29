package progresscheck

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptDiagnosticsRetainsFailureOutput(t *testing.T) {
	for _, test := range []struct {
		name string
		data tracker.NativeRunData
		want []string
	}{
		{name: "source finalization", data: tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Validation: &gate.CommandResult{Command: "go test ./example", ExitCode: 2, Output: "TestExample: expected repaired value, got old value"}}}, want: []string{"go test ./example", "expected repaired value", `"exit_code":2`}},
		{name: "landing gate", data: tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{Gate: &gate.CommandResult{Output: "gate tool cannot load configuration", ExitCode: 2}}}}, want: []string{"landing gate", "cannot load configuration"}},
		{name: "validator findings and commands", data: tracker.NativeRunData{Steps: []tracker.NativeStepReceipt{{Step: "validator", Validator: &gate.ValidatorResult{Summary: "Assertion not repaired", Findings: []gate.Finding{{Body: "return value still violates TestExample", Path: "example.go"}}, Commands: []gate.CommandResult{{Command: "go test ./example", Output: "TestExample still fails"}}}}}}, want: []string{"Assertion not repaired", "return value still violates", "example.go", "TestExample still fails"}},
		{name: "bounded output keeps diagnostic tail", data: tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Validation: &gate.CommandResult{Output: "command begins\n" + strings.Repeat("noise", MaxInputBytes) + "\nfailing assertion at end"}}}, want: []string{"command begins", "truncated", "failing assertion at end", `"output_truncated":true`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.data.AttemptID = "attempt_example"
			got := AttemptDiagnostics(tracker.NativeAttempt{NativeRunData: test.data})
			if len(got) > 13<<10 || !strings.Contains(got, "attempt_example") {
				t.Fatalf("diagnostic bounds or evidence identity lost: bytes=%d", len(got))
			}
			for _, want := range test.want {
				if !strings.Contains(got, want) {
					t.Fatalf("diagnostic missing %q: %s", want, got)
				}
			}
		})
	}
}

func TestNativeRequestBoundsAndAuditBindings(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		text string
	}{
		{"large text", strings.Repeat("evidence ", MaxInputBytes)},
		{"JSON escaped text", strings.Repeat("\x00", MaxInputBytes)},
	} {
		t.Run(test.name, func(t *testing.T) {
			recovery := tracker.NativeRecovery{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "example-project", WorkItemID: "example-issue", Revision: 2}, Body: test.text}}
			for i := range 30 {
				recovery.Attempts = append(recovery.Attempts, tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{AttemptID: fmt.Sprintf("attempt-%d", i), Outcome: "failed", TerminalFailure: &tracker.NativeTerminalFailure{Error: test.text, Stage: "publication", Attribution: "instance", ErrorClass: "publication"}}, Checkpoint: &tracker.NativeCheckpoint{HeadSHA: "head", BaseSHA: "base", CommitSHA: "checkpoint"}})
				recovery.Discussion = append(recovery.Discussion, tracker.NativeComment{ID: fmt.Sprintf("comment-%d", i), Revision: 1, Body: test.text, Actor: tracker.Actor{Kind: "human"}})
			}
			recovery.ChangeDetail = &tracker.ChangeDetail{Reviews: []tracker.ChangeReview{{ID: "audit", SecurityAudit: &tracker.SecurityAuditReview{Verdict: "pass", Run: securityaudit.Run{HeadSHA: "audited-head", BaseSHA: "audited-base"}}}}}
			request := NativeRequest(recovery, "policy", "instance")
			if len(request.Attempts) != MaxAttempts || request.Attempts[0].ID != "attempt-18" || len(request.Comments) != MaxComments {
				t.Fatalf("request bounds: attempts=%d comments=%d", len(request.Attempts), len(request.Comments))
			}
			if request.Attempts[0].Attribution != "instance" || request.Attempts[0].CheckpointSHA != "checkpoint" {
				t.Fatal("attribution or checkpoint lost")
			}
			if len(request.Receipts) != 1 || request.Receipts[0].Step != "security_audit" || request.Receipts[0].BaseSHA != "audited-base" || request.Receipts[0].HeadSHA != "audited-head" || request.Receipts[0].Outcome != "pass" {
				t.Fatalf("audit bindings=%+v", request.Receipts)
			}
			encoded, err := json.Marshal(request)
			if err != nil || len(encoded) > MaxInputBytes {
				t.Fatalf("input bytes=%d err=%v", len(encoded), err)
			}
			if _, err := request.ID(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeRequestActiveAttemptDoesNotRetryOlderFailure(t *testing.T) {
	t.Parallel()
	recovery := tracker.NativeRecovery{Attempts: []tracker.NativeAttempt{
		{NativeRunData: tracker.NativeRunData{AttemptID: "failed", Outcome: "failed"}, Status: "failed"},
		{NativeRunData: tracker.NativeRunData{AttemptID: "current"}, Status: "running"},
	}}
	backend := &fakeBackend{}
	_, checked, err := Check(t.Context(), NativeRequest(recovery, "policy", "instance"), backend, &fakeOwner{})
	if err != nil || checked || backend.calls != 0 {
		t.Fatalf("active continuation paid an AI turn: checked=%v calls=%d error=%v", checked, backend.calls, err)
	}
}

func TestNativeNoProgressSuccessRequiresCheck(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*tracker.NativeRecovery)
		want   bool
	}{
		{"finalization changed zero", func(r *tracker.NativeRecovery) { r.Attempts[1].Finalization.Changed = false }, true},
		{"no work product", func(r *tracker.NativeRecovery) {
			r.Attempts[1].Finalization = nil
			r.Attempts[1].Identity = &tracker.NativeExecutionIdentity{Role: "rework"}
		}, true},
		{"blocked source recovery", func(r *tracker.NativeRecovery) {
			r.Attempts[1].Disposition = &tracker.NativeDisposition{Status: "blocked", ReasonCode: "source_recovery_required"}
		}, true},
		{"repeated typed refusal", func(r *tracker.NativeRecovery) {
			for i := range r.Attempts {
				r.Attempts[i].Runtime = &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "source_missing", BaseSHA: "base", HeadSHA: "head"}}
			}
		}, true},
		{"productive success", func(*tracker.NativeRecovery) {}, false},
		{"same prose is not a refusal", func(r *tracker.NativeRecovery) {
			for i := range r.Attempts {
				r.Attempts[i].Runtime = &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{Refusal: "source missing", BaseSHA: "base", HeadSHA: "head"}}
			}
		}, false},
		{"changed source refusal", func(r *tracker.NativeRecovery) {
			for i := range r.Attempts {
				r.Attempts[i].Runtime = &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "source_missing", BaseSHA: "base", HeadSHA: "head"}}
			}
			r.Attempts[1].Runtime.Landing.HeadSHA = "new-head"
		}, false},
		{"different typed refusal", func(r *tracker.NativeRecovery) {
			for i := range r.Attempts {
				r.Attempts[i].Runtime = &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "source_missing", BaseSHA: "base", HeadSHA: "head"}}
			}
			r.Attempts[1].Runtime.Landing.RefusalKind = "audit_missing"
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := tracker.NativeRecovery{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "project", WorkItemID: "issue", Revision: 1}}}
			for _, id := range []string{"prior", "latest"} {
				r.Attempts = append(r.Attempts, tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{AttemptID: id, Outcome: "succeeded", Finalization: &tracker.NativeFinalization{Changed: true, BaseSHA: "base", HeadSHA: "head"}}})
			}
			test.mutate(&r)
			backend := &fakeBackend{decision: Decision{Outcome: Reroute, Step: "implement", Diagnosis: "latest" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the missing validation evidence in the source finalization path and verify the previously failing check on the pinned source", Reason: "Produce missing work", Evidence: []string{"latest"}}}
			_, checked, err := Check(t.Context(), NativeRequest(r, "policy", "instance"), backend, &fakeOwner{})
			if err != nil || checked != test.want || (backend.calls == 1) != test.want {
				t.Fatalf("checked=%v calls=%d err=%v, want %v", checked, backend.calls, err, test.want)
			}
		})
	}
}
