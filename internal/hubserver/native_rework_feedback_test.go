package hubserver

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNewReworkFeedback(t *testing.T) {
	started := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	before, after := started.Add(-time.Minute), started.Add(time.Minute)
	validator := func(at time.Time, session int64, summary string, findings ...gate.Finding) tracker.ChangeReview {
		return tracker.ChangeReview{VersionID: "version_1", Decision: "changes_requested", Body: summary, CreatedAt: at, Validator: &gate.ValidatorResult{VersionID: "version_1", SessionID: session, Verdict: gate.ValidatorVerdictRework, Findings: findings}}
	}
	implementation := func(at time.Time, session int64, summary string, findings ...gate.Finding) tracker.ChangeReview {
		return tracker.ChangeReview{VersionID: "version_1", Decision: "changes_requested", Body: summary, CreatedAt: at, ImplementationReview: &gate.ValidatorResult{VersionID: "version_1", SessionID: session, Verdict: gate.ValidatorVerdictRework, Findings: findings}}
	}
	race := gate.Finding{Severity: "P1", Path: "internal/a.go", Line: 10, Body: "Data race on the cache"}
	reworded := gate.Finding{Severity: "p1", Path: "internal/a.go", Line: 12, Body: "The cache map is written without the lock"}
	leak := gate.Finding{Severity: "P2", Path: "internal/b.go", Line: 4, Body: "Goroutine leak"}
	human := func(at time.Time, body string) tracker.ChangeReview {
		return tracker.ChangeReview{VersionID: "version_1", Decision: "changes_requested", Body: body, CreatedAt: at}
	}
	check := func(at time.Time, workflow, conclusion string) tracker.ChangeCheck {
		return tracker.ChangeCheck{ChangeCheckResult: tracker.ChangeCheckResult{Source: "detent", WorkflowID: workflow, Conclusion: conclusion}, VersionID: "version_1", ReceivedAt: at}
	}
	run := func(at time.Time, id string) tracker.ChangeCheck {
		result := check(at, "gate", "failure")
		result.CheckRunID = id
		return result
	}
	tests := []struct {
		name   string
		detail tracker.ChangeDetail
		want   bool
	}{
		{name: "rework attempt re-records the existing validator verdict in a new session", detail: tracker.ChangeDetail{Reviews: []tracker.ChangeReview{validator(before, 1, "Missing regression"), validator(after, 2, "Regression still missing, reworded")}}},
		{name: "rework attempt re-records the existing implementation review verdict", detail: tracker.ChangeDetail{Reviews: []tracker.ChangeReview{implementation(before, 1, "p1 a.go: race"), implementation(after, 2, "p1 a.go: race on head")}}},
		{name: "rework attempt reruns an existing failed check", detail: tracker.ChangeDetail{Checks: []tracker.ChangeCheck{check(before, "gate", "failure"), check(after, "gate", "failure")}}},
		{name: "rework attempt re-records the same findings reworded on other lines", detail: tracker.ChangeDetail{Reviews: []tracker.ChangeReview{implementation(before, 1, "One race", race), implementation(after, 2, "Still racy", reworded)}}},
		{name: "rework attempt review adds a finding on the unchanged head", detail: tracker.ChangeDetail{Reviews: []tracker.ChangeReview{implementation(before, 1, "One race", race), implementation(after, 2, "Race and leak", reworded, leak)}}, want: true},
		{name: "validator escalates an existing path to a new severity", detail: tracker.ChangeDetail{Reviews: []tracker.ChangeReview{validator(before, 1, "Minor", leak), validator(after, 2, "Major", gate.Finding{Severity: "P1", Path: leak.Path})}}, want: true},
		{name: "resubmitted check run is not new", detail: tracker.ChangeDetail{Checks: []tracker.ChangeCheck{run(before, "run-1"), run(after, "run-1")}}},
		{name: "a distinct check run of the same workflow is new", detail: tracker.ChangeDetail{Checks: []tracker.ChangeCheck{run(before, "run-1"), run(after, "run-2")}}, want: true},
		{name: "first automated verdict after the attempt started is new", detail: tracker.ChangeDetail{Reviews: []tracker.ChangeReview{validator(after, 2, "Missing regression")}}, want: true},
		{name: "a different automated reviewer is new", detail: tracker.ChangeDetail{Reviews: []tracker.ChangeReview{validator(before, 1, "Missing regression"), implementation(after, 2, "p1 a.go: race")}}, want: true},
		{name: "a new human review is new", detail: tracker.ChangeDetail{Reviews: []tracker.ChangeReview{validator(before, 1, "Missing regression"), human(after, "Also handle the empty list")}}, want: true},
		{name: "a repeated human review is not new", detail: tracker.ChangeDetail{Reviews: []tracker.ChangeReview{human(before, "Handle the empty list"), human(after, "Handle the empty list")}}},
		{name: "a different failed check is new", detail: tracker.ChangeDetail{Checks: []tracker.ChangeCheck{check(before, "gate", "failure"), check(after, "lint", "failure")}}, want: true},
		{name: "feedback on another version is ignored", detail: tracker.ChangeDetail{Reviews: []tracker.ChangeReview{{VersionID: "version_0", Decision: "changes_requested", Body: "Old", CreatedAt: after}}}},
		{name: "approvals and passing checks are not rework feedback", detail: tracker.ChangeDetail{Reviews: []tracker.ChangeReview{{VersionID: "version_1", Decision: "approved", CreatedAt: after}}, Checks: []tracker.ChangeCheck{check(after, "gate", "success")}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := newReworkFeedback(test.detail, "version_1", started); got != test.want {
				t.Fatalf("newReworkFeedback=%t want %t", got, test.want)
			}
		})
	}
}
