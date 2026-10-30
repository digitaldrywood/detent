package progresscheck

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeBackend struct {
	decision Decision
	calls    int
}

func (b *fakeBackend) Decide(context.Context, Request) (Decision, error) {
	b.calls++
	return b.decision, nil
}

type fakeOwner struct {
	records    map[string]Record
	comments   int
	defects    map[string]string
	publishErr error
}

func (o *fakeOwner) Load(_ context.Context, _, _, id string) (Record, bool, error) {
	record, found := o.records[id]
	return record, found, nil
}

func (o *fakeOwner) Publish(_ context.Context, record Record) (Record, error) {
	if o.publishErr != nil {
		return Record{}, o.publishErr
	}
	if retained, found := o.records[record.RequestID]; found {
		return retained, nil
	}
	if o.records == nil {
		o.records = make(map[string]Record)
	}
	if o.defects == nil {
		o.defects = make(map[string]string)
	}
	if record.Decision.Outcome == SystemDefect {
		key := record.Decision.DefectKey
		if o.defects[key] == "" {
			o.defects[key] = "defect-issue"
		}
		record.DefectIssueID = o.defects[key]
	}
	record.CommentID = "progress-comment"
	o.comments++
	o.records[record.RequestID] = record
	return record, nil
}

func refusalRequest() Request {
	return Request{
		ProjectID: "example-project", IssueID: "example-issue", PolicyID: "policy", EvidenceRevision: "revision-1",
		SourceSHA: "head", BaseSHA: "base", Instance: "instance-1", AvailableSteps: []string{"implement", "security_audit", "rebase"},
		Attempts: []Attempt{
			{ID: "attempt-1", Step: "publication", Outcome: "failed", Error: "422: landing requires security audit evidence for its exact base and head", FailureClass: "publication", Attribution: "instance", Instance: "instance-1", SourceSHA: "head", BaseSHA: "base"},
			{ID: "attempt-2", Step: "publication", Outcome: "failed", Error: "422: landing requires security audit evidence for its exact base and head", FailureClass: "publication", Attribution: "instance", Instance: "instance-1", SourceSHA: "head", BaseSHA: "base"},
		},
	}
}

func TestCheckDecisionReplayAndEvidenceChange(t *testing.T) {
	tests := []struct {
		name          string
		decision      Decision
		changedSource bool
	}{
		{"repaired assertion progresses", Decision{Outcome: Progressing, Diagnosis: "attempt-2" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Convergence: "The failing validation now has a matching successful command receipt after the assertion was repaired", Reason: "The failing assertion has been repaired", Evidence: []string{"attempt-2"}}, true},
		{"1748 repeated publication refusal reroutes", Decision{Outcome: Reroute, Step: "security_audit", Diagnosis: "attempt-1" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the missing validation evidence in the source finalization path and verify the previously failing check on the pinned source", Reason: "Produce the missing audit receipt before repeating publication", Evidence: []string{"attempt-1", "attempt-2"}}, false},
		{"Detent refusal reports defect", Decision{Outcome: SystemDefect, Diagnosis: "attempt-1" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Reason: "The publication protocol cannot carry the required audit evidence", Evidence: []string{"attempt-1", "attempt-2"}, DefectKey: "publication-audit-protocol", DefectTitle: "Publication cannot carry audit evidence", DefectBody: "Two unchanged-source attempts received the same 422 refusal"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := refusalRequest()
			if tt.changedSource {
				request.SourceSHA = "new-head"
			}
			backend := &fakeBackend{decision: tt.decision}
			owner := &fakeOwner{}
			first, checked, err := Check(t.Context(), request, backend, owner)
			if err != nil || !checked {
				t.Fatalf("Check = %v, %v", checked, err)
			}
			if first.Decision.Outcome != tt.decision.Outcome || first.Decision.Step != tt.decision.Step {
				t.Fatalf("decision = %+v", first.Decision)
			}
			replay, checked, err := Check(t.Context(), request, backend, owner)
			if err != nil || !checked || replay.RequestID != first.RequestID {
				t.Fatalf("replay = %+v, %v, %v", replay, checked, err)
			}
			if backend.calls != 1 || owner.comments != 1 {
				t.Fatalf("replay calls = %d, comments = %d", backend.calls, owner.comments)
			}
			if tt.decision.Outcome == SystemDefect {
				if len(owner.defects) != 1 || first.DefectIssueID == "" {
					t.Fatalf("defect publication = %+v", first)
				}
				request.EvidenceRevision = "revision-2"
				request.Comments = []Comment{{ID: "human-comment", Revision: "1", Human: true, Body: "The publication protocol has been repaired"}}
				backend.decision = Decision{Outcome: Progressing, Diagnosis: "human-comment" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Convergence: "The failing validation now has a matching successful command receipt after the assertion was repaired", Reason: "New human evidence requires another attempt", Evidence: []string{"human-comment"}}
				changed, checked, err := Check(t.Context(), request, backend, owner)
				if err != nil || !checked || changed.Decision.Outcome != Progressing || changed.RequestID == first.RequestID {
					t.Fatalf("changed evidence = %+v, %v", changed, err)
				}
				if backend.calls != 2 || len(owner.defects) != 1 {
					t.Fatalf("calls = %d, defects = %d", backend.calls, len(owner.defects))
				}
			}
		})
	}
}

func TestCheckSuccessAndPublicationFailure(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(map[bool]string{true: "success pays no turn", false: "publication failure does not return decision"}[success], func(t *testing.T) {
			request := refusalRequest()
			backend := &fakeBackend{decision: Decision{Outcome: Reroute, Step: "implement", Diagnosis: "attempt-2" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the missing validation evidence in the source finalization path and verify the previously failing check on the pinned source", Reason: "Repair source", Evidence: []string{"attempt-2"}}}
			owner := &fakeOwner{publishErr: errors.New("tracker unavailable")}
			if success {
				request.Attempts[1].Outcome = "success"
			}
			record, checked, err := Check(t.Context(), request, backend, owner)
			if success {
				if err != nil || checked || backend.calls != 0 {
					t.Fatalf("success = %v, %v, calls %d", checked, err, backend.calls)
				}
			} else if err == nil || checked || record.RequestID != "" || owner.comments != 0 {
				t.Fatalf("publication failure = %+v, %v, %v", record, checked, err)
			}
		})
	}
}

type textBackend string

func (b textBackend) Generate(context.Context, string, string) (string, error) { return string(b), nil }

func TestAIRejectsUnsafeOrMalformedDecisions(t *testing.T) {
	tests := []struct {
		name, output string
		valid        bool
	}{
		{"mechanical change without diagnosis", `{"outcome":"progressing","reason":"Source and base changed","evidence":["attempt-2"]}`, false},
		{"reroute without actionable instruction", `{"outcome":"reroute","step":"implement","reason":"Try implementation again","evidence":["attempt-2"]}`, false},
		{"diagnosis must cite evidence", `{"outcome":"reroute","step":"implement","reason":"Repair assertion","diagnosis":"The assertion still fails","instruction":"Repair the code exercised by TestExample and run it","evidence":["attempt-2"]}`, false},
		{"diagnosed reroute still requires instruction", `{"outcome":"reroute","step":"implement","reason":"Repair assertion","diagnosis":"attempt-2: TestExample expects the repaired value but code returns the old value","evidence":["attempt-2"]}`, false},
		{"mechanical change lacks convergence evidence", `{"outcome":"progressing","reason":"Source changed","diagnosis":"attempt-2: source finalization still fails after rebasing","evidence":["attempt-2"]}`, false},
		{"base defect with diagnosis", `{"outcome":"system_defect","reason":"Repair base before retrying","diagnosis":"attempt-2: TestExample fails on the pinned base with the same assertion, independently of the diff","evidence":["attempt-2"],"defect_key":"base-example-assertion","defect_title":"Repair base assertion","defect_body":"The pinned base fails TestExample before applying the change; repair the exercised implementation and verify both base and change."}`, true},
		{"progressing step is ignored", `{"outcome":"progressing","step":"implement","reason":"New evidence","diagnosis":"attempt-2 lacked a validation receipt","convergence":"The repaired assertion now has a successful validation receipt","evidence":["attempt-2"]}`, true},
		{"typed reroute", `{"outcome":"reroute","step":"security_audit","reason":"Missing receipt","diagnosis":"attempt-2 lacks the audit receipt required by publication","instruction":"Run the audit against the pinned source and retain its receipt before publication","evidence":["attempt-2"]}`, true},
		{"unknown outcome", `{"outcome":"park","reason":"Repeated","evidence":["attempt-2"]}`, false},
		{"unavailable step", `{"outcome":"reroute","step":"bypass_audit","reason":"Missing receipt","evidence":["attempt-2"]}`, false},
		{"lease field", `{"outcome":"progressing","reason":"Go","evidence":["attempt-2"],"lease_owner":"new"}`, false},
		{"fabricated evidence", `{"outcome":"progressing","reason":"Go","evidence":["missing"]}`, false},
		{"missing evidence", `{"outcome":"progressing","reason":"Go"}`, false},
		{"missing defect body", `{"outcome":"system_defect","reason":"Protocol error","evidence":["attempt-2"],"defect_key":"protocol"}`, false},
		{"trailing object", `{"outcome":"progressing","reason":"Go","evidence":["attempt-2"]} {}`, false},
		{"oversized output", strings.Repeat("x", MaxOutputBytes+1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := (AI{Backend: textBackend(tt.output)}).Decide(t.Context(), refusalRequest())
			if (err == nil) != tt.valid {
				t.Fatalf("error = %v, valid = %v", err, tt.valid)
			}
		})
	}
}

func TestEmptyLandingCompletionRequiresAcceptanceEvidence(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*Request, *Decision)
		valid  bool
	}{
		{name: "passing regression on the base", valid: true},
		{name: "recorded incorporated reviewed delta", valid: true, mutate: func(r *Request, d *Decision) { d.Evidence = []string{"base-proof"} }},
		{name: "nonempty delta", mutate: func(r *Request, d *Decision) { r.EmptyDelta = false }},
		{name: "missing landing refusal", mutate: func(r *Request, d *Decision) { r.Attempts[1].LandingRefusal = "" }},
		{name: "real undelivered work", mutate: func(r *Request, d *Decision) { r.Attempts[1].LandingRefusal = "conflict" }},
		{name: "review or audit gate pending", mutate: func(r *Request, d *Decision) { r.AvailableSteps = []string{"implement", "merge"} }},
		{name: "empty delta alone", mutate: func(r *Request, d *Decision) { d.Evidence = []string{"empty-version"} }},
		{name: "refusal alone", mutate: func(r *Request, d *Decision) { d.Evidence = []string{"attempt-2:landing"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := refusalRequest()
			request.EmptyDelta = true
			request.AvailableSteps = append(request.AvailableSteps, CompleteStep)
			request.Attempts[1].Outcome = "success"
			request.Attempts[1].LandingRefusal = "nothing_to_land"
			request.Receipts = []Receipt{{ID: "attempt-2:landing", Step: "landing", Outcome: "nothing_to_land"}, {ID: "empty-version", Step: "source"}, {ID: "base-test", Step: "check", Outcome: "success", HeadSHA: "base", Detail: "Acceptance regression passes"}}
			request.Comments = []Comment{{ID: "base-proof", Body: "Previously reviewed delta is incorporated in base"}}
			decision := Decision{Outcome: Reroute, Step: CompleteStep, Diagnosis: "base-test and base-proof show the required behavior is already on the base", Instruction: "Complete the issue using the verified base acceptance evidence", Reason: "Acceptance regression passes on the base", Evidence: []string{"base-test"}}
			if test.mutate != nil {
				test.mutate(&request, &decision)
			}
			if err := validateDecision(request, decision); (err == nil) != test.valid {
				t.Fatalf("completion validation error=%v want valid=%v", err, test.valid)
			}
		})
	}
}

func TestCheckRejectsUnboundedEvidenceBeforeBackend(t *testing.T) {
	for _, mutate := range []struct {
		name  string
		apply func(*Request)
	}{
		{"attempt count", func(r *Request) { r.Attempts = make([]Attempt, MaxAttempts+1) }},
		{"receipt count", func(r *Request) { r.Receipts = make([]Receipt, MaxReceipts+1) }},
		{"comment count", func(r *Request) { r.Comments = make([]Comment, MaxComments+1) }},
		{"byte count", func(r *Request) { r.Attempts[0].Error = strings.Repeat("x", MaxInputBytes) }},
		{"receipt evidence collision", func(r *Request) { r.Receipts = []Receipt{{ID: "attempt-2"}} }},
		{"comment evidence collision", func(r *Request) { r.Comments = []Comment{{ID: "attempt-2"}} }},

		{"active attempt", func(r *Request) { r.Attempts[1].Outcome = "running" }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			request := refusalRequest()
			mutate.apply(&request)
			backend := &fakeBackend{}
			_, checked, err := Check(t.Context(), request, backend, &fakeOwner{})
			if err == nil || checked || backend.calls != 0 {
				t.Fatalf("Check = %v, %v, calls %d", checked, err, backend.calls)
			}
		})
	}
}

func TestCheckSuccessfulOrCancelledWorkPaysNoTurnForStaleEvidence(t *testing.T) {
	for _, outcome := range []string{"success", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			request := refusalRequest()
			request.Attempts[len(request.Attempts)-1].Outcome = outcome
			request.Attempts[0].Error = strings.Repeat("x", MaxInputBytes+1)
			backend := &fakeBackend{}
			_, checked, err := Check(t.Context(), request, backend, &fakeOwner{})
			if err != nil || checked || backend.calls != 0 {
				t.Fatalf("Check = %v, %v, backend calls %d", checked, err, backend.calls)
			}
		})
	}
}

func TestRetainedLegacyDecisionsRemainValid(t *testing.T) {
	for _, outcome := range []Outcome{Progressing, Reroute, SystemDefect} {
		t.Run(string(outcome), func(t *testing.T) {
			request := refusalRequest()
			decision := Decision{Outcome: outcome, Reason: "Retained judgment", Evidence: []string{"attempt-2"}}
			if outcome == Reroute {
				decision.Step = "implement"
			}
			if outcome == SystemDefect {
				decision.DefectKey, decision.DefectTitle, decision.DefectBody = "key", "title", "body"
			}
			id, err := request.ID()
			if err != nil {
				t.Fatal(err)
			}
			record := Record{RequestID: id, ProjectID: request.ProjectID, IssueID: request.IssueID, EvidenceRevision: request.EvidenceRevision, Decision: decision, CommentID: "comment", DefectIssueID: "defect"}
			backend := &fakeBackend{}
			got, checked, err := Check(t.Context(), request, backend, &fakeOwner{records: map[string]Record{id: record}})
			if err != nil || !checked || backend.calls != 0 || !got.Decision.Legacy() {
				t.Fatalf("legacy replay invalidated: checked=%t calls=%d record=%+v err=%v", checked, backend.calls, got, err)
			}
			if err := validateDecision(request, decision); err == nil {
				t.Fatal("new AI decision accepted legacy shape")
			}
			if err := record.Validate(request); err != nil {
				t.Fatalf("retained legacy record invalid: %v", err)
			}
			if err := record.ValidatePublication(request); err == nil {
				t.Fatal("new publication accepted legacy shape")
			}
		})
	}
}
